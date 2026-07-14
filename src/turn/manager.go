// Package turn coordinates active agent turns across front-ends.
//
// It owns the per-channel "active turn" map so HTTP/CLI prompts and chat
// messages share the same steering behavior: if a channel already has an
// in-flight turn, a new message is sent as pi steer/prompt(steer) instead of
// acquiring a second pool slot.
package turn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"wall-e/pool"
	"wall-e/rpc"
)

// Manager coordinates active turns for a worker pool.
type Manager struct {
	pool *pool.Pool
	ctx  context.Context

	mu    sync.Mutex
	turns map[pool.ChannelID]*Turn
}

// Turn is one active channel turn. It broadcasts pi events to any interested
// front-end subscribers while owning the single Slot.Events consumer.
type Turn struct {
	mgr     *Manager
	channel pool.ChannelID

	ready chan struct{}
	done  chan struct{}

	mu         sync.Mutex
	slot       *pool.Slot
	acquireErr error
	subs       map[chan rpc.Event]chan string
}

// Subscription is a per-caller event stream for an active turn. Call Close when
// the caller no longer wants events. Closing a subscription does not abort the
// underlying turn; other subscribers/delivery adapters may still be using it.
type Subscription struct {
	Events <-chan rpc.Event
	// FinalText receives the authoritative concatenation of all assistant text
	// deltas when the turn completes normally. It closes without a value if the
	// turn fails. Unlike Events, its completion value is never dropped.
	FinalText <-chan string
	close     func()
}

// Close detaches this subscriber from the turn. It is safe to call multiple
// times.
func (s *Subscription) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

// SubmitOptions controls active-turn behavior.
type SubmitOptions struct {
	// UsePromptSteer sends prompt(streamingBehavior=steer) instead of steer for
	// active turns. Pi extension slash commands require prompt-steer.
	UsePromptSteer bool
	// SubscribeOnSteer controls whether Submit returns a subscription when the
	// message steered an already-active turn. Chat front-ends usually leave this
	// false so a mid-stream user message updates the existing response instead of
	// creating a second visible response. HTTP/CLI callers usually set true so
	// they can continue streaming the active turn.
	SubscribeOnSteer bool
	// ExtraNewSubscribers pre-attaches extra subscriptions before a newly-started
	// turn sends its prompt. Delivery adapters use this when both HTTP/CLI and an
	// external chat need complete streams for the same new turn.
	ExtraNewSubscribers int
}

// SubmitResult describes whether Submit started a new turn or steered an
// existing one.
type SubmitResult struct {
	Started            bool
	Steered            bool
	ExtraSubscriptions []*Subscription
}

// NewManager creates a turn manager. ctx bounds internal Acquire/Prompt/stream
// work; if nil, context.Background is used.
func NewManager(ctx context.Context, p *pool.Pool) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Manager{pool: p, ctx: ctx, turns: make(map[pool.ChannelID]*Turn)}
}

// Submit starts or steers a turn for channel. For a new turn it returns a
// subscription to the full event stream. For an active turn it sends steer (or
// prompt-steer) and returns a subscription only when opts.SubscribeOnSteer is
// true.
func (m *Manager) Submit(ctx context.Context, channel pool.ChannelID, message string, opts SubmitOptions) (*Subscription, SubmitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		m.mu.Lock()
		t := m.turns[channel]
		if t == nil {
			t = &Turn{mgr: m, channel: channel, ready: make(chan struct{}), done: make(chan struct{}), subs: make(map[chan rpc.Event]chan string)}
			m.turns[channel] = t
			sub := t.subscribeLocked()
			extras := make([]*Subscription, 0, opts.ExtraNewSubscribers)
			for i := 0; i < opts.ExtraNewSubscribers; i++ {
				extras = append(extras, t.subscribeLocked())
			}
			m.mu.Unlock()
			go t.run(ctx, message)
			select {
			case <-t.ready:
			case <-ctx.Done():
				sub.Close()
				for _, extra := range extras {
					extra.Close()
				}
				return nil, SubmitResult{}, ctx.Err()
			}
			t.mu.Lock()
			acqErr := t.acquireErr
			t.mu.Unlock()
			if acqErr != nil {
				sub.Close()
				for _, extra := range extras {
					extra.Close()
				}
				return nil, SubmitResult{}, acqErr
			}
			return sub, SubmitResult{Started: true, ExtraSubscriptions: extras}, nil
		}
		var sub *Subscription
		if opts.SubscribeOnSteer {
			sub = t.subscribeLocked()
		}
		m.mu.Unlock()

		select {
		case <-t.ready:
		case <-t.done:
			if sub != nil {
				sub.Close()
			}
			continue // stale active entry; retry and likely start a new turn
		case <-ctx.Done():
			if sub != nil {
				sub.Close()
			}
			return nil, SubmitResult{}, ctx.Err()
		}

		t.mu.Lock()
		slot := t.slot
		acqErr := t.acquireErr
		t.mu.Unlock()
		if acqErr != nil || slot == nil {
			if sub != nil {
				sub.Close()
			}
			if acqErr == nil {
				acqErr = errors.New("turn: active turn has no slot")
			}
			return nil, SubmitResult{}, acqErr
		}
		var err error
		if opts.UsePromptSteer {
			_, err = slot.Client().Prompt(ctx, message, true)
		} else {
			_, err = slot.Client().Steer(ctx, message)
		}
		if err != nil {
			if sub != nil {
				sub.Close()
			}
			return nil, SubmitResult{}, err
		}
		return sub, SubmitResult{Steered: true}, nil
	}
}

// Subscribe attaches to the currently active turn for channel. It returns nil
// if no turn is active.
func (m *Manager) Subscribe(channel pool.ChannelID) *Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.turns[channel]; t != nil {
		return t.subscribeLocked()
	}
	return nil
}

// Active reports whether channel currently has an active turn.
func (m *Manager) Active(channel pool.ChannelID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns[channel] != nil
}

// Abort aborts an active turn for channel.
func (m *Manager) Abort(ctx context.Context, channel pool.ChannelID) (rpc.Response, error) {
	m.mu.Lock()
	t := m.turns[channel]
	m.mu.Unlock()
	if t == nil {
		return rpc.Response{}, ErrNoActiveTurn
	}
	select {
	case <-t.ready:
	case <-t.done:
		return rpc.Response{}, ErrNoActiveTurn
	case <-ctx.Done():
		return rpc.Response{}, ctx.Err()
	}
	t.mu.Lock()
	slot := t.slot
	acqErr := t.acquireErr
	t.mu.Unlock()
	if acqErr != nil || slot == nil {
		if acqErr != nil {
			return rpc.Response{}, acqErr
		}
		return rpc.Response{}, ErrNoActiveTurn
	}
	return slot.Client().Abort(ctx)
}

// ErrNoActiveTurn is returned by Abort when there is nothing to abort.
var ErrNoActiveTurn = errors.New("turn: no active turn")

func (t *Turn) run(acquireCtx context.Context, message string) {
	var finalText strings.Builder
	completed := false
	defer close(t.done)
	defer func() {
		t.mgr.mu.Lock()
		if t.mgr.turns[t.channel] == t {
			delete(t.mgr.turns, t.channel)
		}
		t.mgr.mu.Unlock()
		t.closeSubscribers(finalText.String(), completed)
	}()

	slot, err := t.mgr.pool.Acquire(acquireCtx, t.channel)
	t.mu.Lock()
	t.slot = slot
	t.acquireErr = err
	t.mu.Unlock()
	close(t.ready)
	if err != nil {
		return
	}
	defer t.mgr.pool.Release(t.channel)

	if _, err := slot.Client().Prompt(t.mgr.ctx, message, false); err != nil {
		return
	}
	for ev := range slot.Events() {
		if ev.Type == rpc.EventMessageUpdate {
			if delta, ok := turnTextDelta(ev.Raw); ok {
				finalText.WriteString(delta)
			}
		}
		t.broadcast(ev)
		if ev.Type == rpc.EventAgentEnd {
			completed = true
			return
		}
	}
}

// subscribeLocked adds a subscriber. Caller must hold m.mu, which serializes
// against manager map removal; Turn.mu protects the subscriber set itself.
func (t *Turn) subscribeLocked() *Subscription {
	ch := make(chan rpc.Event, 64)
	final := make(chan string, 1)
	t.mu.Lock()
	select {
	case <-t.done:
		close(ch)
		close(final)
		t.mu.Unlock()
		return &Subscription{Events: ch, FinalText: final, close: func() {}}
	default:
	}
	t.subs[ch] = final
	t.mu.Unlock()
	var once sync.Once
	return &Subscription{Events: ch, FinalText: final, close: func() {
		once.Do(func() {
			t.mu.Lock()
			if finalCh, ok := t.subs[ch]; ok {
				delete(t.subs, ch)
				close(ch)
				close(finalCh)
			}
			t.mu.Unlock()
		})
	}}
}

func (t *Turn) broadcast(ev rpc.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch := range t.subs {
		select {
		case ch <- ev:
		default:
			// Drop for slow subscribers. The turn owner still consumes all pi events.
		}
	}
}

func (t *Turn) closeSubscribers(finalText string, completed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch, final := range t.subs {
		if completed {
			final <- finalText
		}
		close(final)
		close(ch)
		delete(t.subs, ch)
	}
}

func turnTextDelta(raw []byte) (string, bool) {
	var ev struct {
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil || ev.AssistantMessageEvent.Type != "text_delta" {
		return "", false
	}
	return ev.AssistantMessageEvent.Delta, true
}
