// Package pool implements the wall-e worker pool: a bounded set of `pi --mode
// rpc` processes, at most one bound to any active channel at a time, with
// per-channel serialization, drain-on-reuse, and LRU warm reuse.
//
// Concurrency model
// ------------------
// The pool owns at most Size live `*rpc.Client` processes. Each live process
// is wrapped in an internal `slot`. A slot is "busy" while an Acquire is held
// (until the caller calls Release) and "streaming" while the underlying pi
// agent is between agent_start and agent_end. The two are independent: a slot
// may be Released while still streaming (the caller stopped driving it but the
// agent hasn't emitted agent_end yet), which is exactly when drain-on-reuse
// matters.
//
// Acquire(channel):
//  1. If a slot is bound to `channel`, serialize on it: block while it is busy,
//     then reuse it (same session, no drain, no switch_session).
//  2. Otherwise (no slot for `channel`):
//     a. If under capacity (< Size live slots), spawn a fresh pi process,
//     switch_session to the channel's current file, bind, mark busy.
//     b. If at capacity, pick the LRU idle (not busy) slot, drain it if still
//     streaming (wait for agent_end up to DrainTimeout, then abort), then
//     respawn it with WALLE_CHANNEL set for the new channel, switch_session to
//     the new channel's file, and rebind.
//     c. If at capacity with no idle slot (all busy), block until a slot frees,
//     then proceed as in (b).
//
// Drain-on-reuse never kills a busy process: it either lets the stream finish
// naturally (agent_end) or aborts it. Killing only happens on Shutdown and when
// reusing a slot whose process has exited (respawn).
//
// Per-channel serialization is enforced here (not in the HTTP/chat layer): two
// concurrent Acquires for the same channel run one after the other on the same
// slot. Two concurrent Acquires for different channels run on different slots
// (or serialize on the same slot when the pool is saturated).
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"wall-e/rpc"
	"wall-e/session"
)

// ChannelID is the logical, platform-stable identifier for a channel. It is an
// alias of session.ChannelID so the pool and session manager share a type.
type ChannelID = session.ChannelID

// ErrPoolClosed is returned by Acquire after Shutdown (or a closed pool).
var ErrPoolClosed = errors.New("pool: closed")

// ErrDrainTimeout is returned when a drain exceeded its grace period even
// after aborting the agent (the process did not emit agent_end).
var ErrDrainTimeout = errors.New("pool: drain timeout")

// Config configures a Pool.
type Config struct {
	// Size is the maximum number of live pi processes. Defaults to 4.
	Size int
	// DrainTimeout is the max wait for agent_end during drain-on-reuse and
	// Shutdown before aborting/killing. Defaults to 30s.
	DrainTimeout time.Duration
	// RPCConfig is passed to the factory when spawning new pi processes.
	RPCConfig rpc.Config
	// Sessions provides the current session file path per channel (used for
	// switch_session on bind/reuse). Required.
	Sessions *session.Manager
	// NewClient spawns a fresh *rpc.Client. Defaults to rpc.New; injectable
	// for tests.
	NewClient func(cfg rpc.Config) (*rpc.Client, error)
}

// Pool manages a bounded set of pi worker processes.
type Pool struct {
	cfg       Config
	newClient func(rpc.Config) (*rpc.Client, error)
	drainTo   time.Duration

	mu         sync.Mutex
	slots      map[ChannelID]*slot
	idleSignal chan struct{} // closed (then recreated) when any slot goes busy→idle
	sem        chan struct{} // bounds live process count to Size
	closed     atomic.Bool
	doneCh     chan struct{}
}

// Slot is a handle to an acquired worker. The caller drives the pi process via
// Client() and consumes streaming events via Events(). It MUST be returned to
// the pool via Pool.Release when the turn is done.
type Slot struct {
	pool    *Pool
	slot    *slot
	channel ChannelID
}

// slot is an internal worker: a live pi process (rpc.Client) plus its current
// channel binding and busy/streaming state.
type slot struct {
	client  *rpc.Client
	channel ChannelID

	mu       sync.Mutex
	busy     bool
	lastUsed time.Time

	streaming    atomic.Bool
	clientClosed atomic.Bool

	events chan rpc.Event // forwarded events for the active Slot consumer
}

// New creates a Pool.
func New(cfg Config) (*Pool, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("pool: Sessions is required")
	}
	size := cfg.Size
	if size <= 0 {
		size = 4
	}
	drain := cfg.DrainTimeout
	if drain <= 0 {
		drain = 30 * time.Second
	}
	nc := cfg.NewClient
	if nc == nil {
		nc = rpc.New
	}
	p := &Pool{
		cfg:        cfg,
		newClient:  nc,
		drainTo:    drain,
		slots:      make(map[ChannelID]*slot),
		idleSignal: make(chan struct{}),
		sem:        make(chan struct{}, size),
		doneCh:     make(chan struct{}),
	}
	// Fill the semaphore: each live slot will take one token when spawned.
	for i := 0; i < size; i++ {
		p.sem <- struct{}{}
	}
	return p, nil
}

// Acquire returns a Slot bound to `channel`, spawning or reusing a pi process
// as needed. It blocks (ctx-aware) when the channel is currently busy
// (serialization) or when the pool is saturated with busy slots.
func (p *Pool) Acquire(ctx context.Context, channel ChannelID) (*Slot, error) {
	if p.closed.Load() {
		return nil, ErrPoolClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Same-channel path: serialize on the existing slot.
	for {
		if p.closed.Load() {
			return nil, ErrPoolClosed
		}
		p.mu.Lock()
		s := p.slots[channel]
		if s == nil {
			p.mu.Unlock()
			break // no slot for this channel → new-channel path
		}
		s.mu.Lock()
		if !s.busy {
			s.busy = true
			s.lastUsed = time.Now()
			s.mu.Unlock()
			p.mu.Unlock()
			p.flushEvents(s)
			return &Slot{pool: p, slot: s, channel: channel}, nil
		}
		s.mu.Unlock()
		sig := p.idleSignal
		p.mu.Unlock()

		select {
		case <-sig:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.doneCh:
			return nil, ErrPoolClosed
		}
	}

	// New-channel path.
	return p.acquireNewChannel(ctx, channel)
}

func (p *Pool) acquireNewChannel(ctx context.Context, channel ChannelID) (*Slot, error) {
	for {
		if p.closed.Load() {
			return nil, ErrPoolClosed
		}
		p.mu.Lock()
		// Race: another goroutine may have created a slot for this channel.
		if s := p.slots[channel]; s != nil {
			p.mu.Unlock()
			// Fall back to same-channel serialization.
			return p.serializeOn(ctx, s, channel)
		}

		if len(p.slots) < cap(p.sem) {
			// Under capacity: spawn a fresh slot.
			// Take a semaphore token (should be immediately available since
			// len(slots) < cap, but be safe).
			p.mu.Unlock()
			select {
			case <-p.sem:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-p.doneCh:
				return nil, ErrPoolClosed
			}
			if p.closed.Load() {
				p.sem <- struct{}{}
				return nil, ErrPoolClosed
			}
			s, err := p.spawnSlot(ctx, channel)
			if err != nil {
				p.sem <- struct{}{} // return token
				return nil, err
			}
			// Bind under lock; handle the race where another goroutine bound
			// this channel concurrently.
			p.mu.Lock()
			if existing := p.slots[channel]; existing != nil {
				p.mu.Unlock()
				// Discard the slot we just spawned.
				_ = s.client.Close()
				p.sem <- struct{}{}
				return p.serializeOn(ctx, existing, channel)
			}
			s.busy = true
			s.lastUsed = time.Now()
			p.slots[channel] = s
			p.mu.Unlock()
			p.flushEvents(s)
			return &Slot{pool: p, slot: s, channel: channel}, nil
		}

		// At capacity: find an LRU idle slot to reuse.
		victim := p.findIdleLRU()
		if victim != nil {
			// Claim it (re-check under its lock).
			victim.mu.Lock()
			if victim.busy {
				victim.mu.Unlock()
				p.mu.Unlock()
				continue // race; retry
			}
			victim.busy = true
			oldCh := victim.channel
			delete(p.slots, oldCh)
			p.slots[channel] = victim
			victim.channel = channel
			victim.mu.Unlock()
			p.mu.Unlock()

			if err := p.prepare(ctx, victim, oldCh, channel); err != nil {
				// Revert: mark idle, rebind to old channel, return error. If
				// prepare had already respawned for the new channel, close it so
				// the next old-channel acquire cannot observe a stale
				// WALLE_CHANNEL.
				p.invalidateClient(victim)
				p.revertReuse(victim, oldCh)
				return nil, err
			}
			p.flushEvents(victim)
			return &Slot{pool: p, slot: victim, channel: channel}, nil
		}

		// No idle slot: wait for one to free up.
		sig := p.idleSignal
		p.mu.Unlock()
		select {
		case <-sig:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.doneCh:
			return nil, ErrPoolClosed
		}
	}
}

// serializeOn blocks until `s` is not busy (same-channel serialization), then
// returns a Slot reusing it. No drain (same session).
func (p *Pool) serializeOn(ctx context.Context, s *slot, channel ChannelID) (*Slot, error) {
	for {
		if p.closed.Load() {
			return nil, ErrPoolClosed
		}
		p.mu.Lock()
		// Re-verify the slot is still bound to this channel.
		if cur := p.slots[channel]; cur != s {
			p.mu.Unlock()
			// Slot was rebound/evicted; restart via Acquire.
			return p.Acquire(ctx, channel)
		}
		s.mu.Lock()
		if !s.busy {
			s.busy = true
			s.lastUsed = time.Now()
			s.mu.Unlock()
			p.mu.Unlock()
			p.flushEvents(s)
			return &Slot{pool: p, slot: s, channel: channel}, nil
		}
		s.mu.Unlock()
		sig := p.idleSignal
		p.mu.Unlock()
		select {
		case <-sig:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.doneCh:
			return nil, ErrPoolClosed
		}
	}
}

// findIdleLRU returns the least-recently-used idle (not busy) slot, or nil.
// Caller must hold p.mu.
func (p *Pool) findIdleLRU() *slot {
	var victim *slot
	var victimLU time.Time
	for _, sl := range p.slots {
		sl.mu.Lock()
		idle := !sl.busy
		lu := sl.lastUsed
		sl.mu.Unlock()
		if !idle {
			continue
		}
		if victim == nil || lu.Before(victimLU) {
			victim = sl
			victimLU = lu
		}
	}
	return victim
}

// spawnSlot creates a fresh pi process + slot for `channel` and switch_sessions
// to the channel's current session file. Does NOT mark busy (caller does).
func (p *Pool) spawnSlot(ctx context.Context, channel ChannelID) (*slot, error) {
	c, err := p.newClient(p.rpcConfigForChannel(channel))
	if err != nil {
		return nil, fmt.Errorf("pool: spawn client: %w", err)
	}
	s := &slot{
		client:  c,
		channel: channel,
		events:  make(chan rpc.Event, 64),
	}
	p.startForwarder(s)
	if err := p.switchTo(ctx, s, channel); err != nil {
		_ = c.Close()
		return nil, err
	}
	return s, nil
}

// prepare readies a reused slot for a (possibly) new channel: ensures the
// client is alive, drains any in-flight stream, then switches to the new
// channel's file. When the channel changes, the pi process is respawned so its
// process environment contains the correct WALLE_CHANNEL value.
func (p *Pool) prepare(ctx context.Context, s *slot, oldChannel, channel ChannelID) error {
	respawned := false
	// If the process died, respawn.
	if s.clientClosed.Load() {
		if err := p.respawn(ctx, s, channel); err != nil {
			return err
		}
		respawned = true
	}
	// Drain if still streaming from the previous channel's turn.
	if s.streaming.Load() {
		if err := p.drain(ctx, s); err != nil {
			return err
		}
	}
	// A different channel gets a fresh process so WALLE_CHANNEL is not stale.
	if oldChannel != "" && oldChannel != channel && !respawned {
		if err := p.respawn(ctx, s, channel); err != nil {
			return err
		}
	}
	// Point the process at the new channel's session file.
	if err := p.switchTo(ctx, s, channel); err != nil {
		return err
	}
	return nil
}

func (p *Pool) respawn(ctx context.Context, s *slot, channel ChannelID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old := s.client
	c, err := p.newClient(p.rpcConfigForChannel(channel))
	if err != nil {
		return fmt.Errorf("pool: respawn client: %w", err)
	}
	if old != nil {
		_ = old.Close()
	}
	s.client = c
	s.clientClosed.Store(false)
	s.streaming.Store(false)
	p.startForwarder(s)
	return nil
}

func (p *Pool) invalidateClient(s *slot) {
	if s.client != nil {
		_ = s.client.Close()
	}
	s.clientClosed.Store(true)
	s.streaming.Store(false)
}

func (p *Pool) rpcConfigForChannel(channel ChannelID) rpc.Config {
	cfg := p.cfg.RPCConfig
	env := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["WALLE_CHANNEL"] = channel.Address()
	cfg.Env = env
	return cfg
}

// switchTo sends switch_session to the channel's current session file. The
// rpc.Client resyncs its cached sessionFile via get_state automatically.
func (p *Pool) switchTo(ctx context.Context, s *slot, channel ChannelID) error {
	path, _ := p.cfg.Sessions.Current(channel)
	if _, _, err := s.client.SwitchSession(ctx, path); err != nil {
		return fmt.Errorf("pool: switch_session: %w", err)
	}
	return nil
}

// drain waits for the slot's agent to stop streaming. It waits up to drainTo
// for a natural agent_end; if none arrives it aborts the agent and waits a
// further drainTo for the resulting agent_end.
func (p *Pool) drain(ctx context.Context, s *slot) error {
	if !s.streaming.Load() {
		return nil
	}
	if p.waitNotStreaming(ctx, s, p.drainTo) {
		return nil
	}
	// Timed out waiting for natural agent_end → abort.
	if _, err := s.client.Abort(ctx); err != nil && !errors.Is(err, rpc.ErrPiExit) {
		// Abort failed for a non-exit reason; keep waiting anyway.
	}
	// Wait again for agent_end after abort.
	if p.waitNotStreaming(ctx, s, p.drainTo) {
		return nil
	}
	return ErrDrainTimeout
}

// waitNotStreaming polls the slot's streaming flag until false or the timeout
// expires (or ctx/pool is done). Returns true if the stream ended.
func (p *Pool) waitNotStreaming(ctx context.Context, s *slot, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !s.streaming.Load() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-p.doneCh:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// revertReuse undoes a failed reuse: mark the slot idle and rebind it to its
// previous channel so it stays warm and usable.
func (p *Pool) revertReuse(s *slot, oldCh ChannelID) {
	p.mu.Lock()
	delete(p.slots, s.channel)
	s.channel = oldCh
	if oldCh != "" {
		p.slots[oldCh] = s
	}
	p.mu.Unlock()
	s.mu.Lock()
	s.busy = false
	s.lastUsed = time.Now()
	s.mu.Unlock()
	p.broadcastIdle()
}

// broadcastIdle signals any waiter that a slot became idle. Caller should NOT
// hold p.mu (it closes+recreates idleSignal under the lock).
func (p *Pool) broadcastIdle() {
	p.mu.Lock()
	close(p.idleSignal)
	p.idleSignal = make(chan struct{})
	p.mu.Unlock()
}

// NewSessionPath returns a fresh wall-e typed session path for channel without
// storing it as current.
func (p *Pool) NewSessionPath(channel ChannelID) string {
	return p.cfg.Sessions.NewSessionPath(channel)
}

// CopySessionFile copies an existing transcript to targetPath. Both paths must
// live under the configured session dir.
func (p *Pool) CopySessionFile(sourcePath, targetPath string) error {
	return p.cfg.Sessions.CopySessionFile(sourcePath, targetPath)
}

// RemoveSessionFile removes a transcript under the configured session dir.
func (p *Pool) RemoveSessionFile(path string) error {
	return p.cfg.Sessions.RemoveSessionFile(path)
}

// ResyncFromState updates the session manager's current file for channel after
// a session-mutating RPC command such as new_session or clone.
func (p *Pool) ResyncFromState(channel ChannelID, sessionFile string) error {
	return p.cfg.Sessions.ResyncFromState(channel, sessionFile)
}

// Release returns the slot bound to `channel` to the pool. The process stays
// warm (alive) for reuse; it is NOT killed. Safe to call multiple times or
// after Shutdown (no-op).
func (p *Pool) Release(channel ChannelID) {
	if p.closed.Load() {
		return
	}
	p.mu.Lock()
	s := p.slots[channel]
	p.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := s.busy
	s.busy = false
	s.lastUsed = time.Now()
	s.mu.Unlock()
	if changed {
		p.broadcastIdle()
	}
}

// Shutdown drains all streaming slots (abort after DrainTimeout), kills all
// pi processes, and marks the pool closed. Subsequent Acquire returns
// ErrPoolClosed. Safe to call multiple times.
func (p *Pool) Shutdown(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.doneCh)

	p.mu.Lock()
	slots := make([]*slot, 0, len(p.slots))
	for _, s := range p.slots {
		slots = append(slots, s)
	}
	p.mu.Unlock()

	for _, s := range slots {
		s.mu.Lock()
		streaming := s.streaming.Load()
		s.mu.Unlock()
		if streaming {
			// Best-effort abort; tolerate a dead/exiting process.
			_, _ = s.client.Abort(ctx)
			p.waitNotStreaming(ctx, s, p.drainTo)
		}
		if s.client != nil {
			_ = s.client.Close()
		}
	}
	return nil
}

// startForwarder launches a goroutine that consumes the client's Events,
// tracks the streaming flag (agent_start/agent_end), and forwards events into
// the slot's buffered events channel for the active Slot consumer. The
// goroutine exits when the client's Events channel closes (process exit).
func (p *Pool) startForwarder(s *slot) {
	go func() {
		ch := s.client.Events()
		for {
			ev, ok := <-ch
			if !ok {
				s.clientClosed.Store(true)
				// A closed stream is not "streaming".
				s.streaming.Store(false)
				return
			}
			switch ev.Type {
			case rpc.EventAgentStart:
				s.streaming.Store(true)
			case rpc.EventAgentEnd:
				s.streaming.Store(false)
			}
			select {
			case s.events <- ev:
			default:
				// Drop if the consumer is slow or the slot is idle: streaming
				// state is already updated above, which is what the pool needs.
			}
		}
	}()
}

// flushEvents discards any buffered events so a freshly acquired Slot only
// sees events from its own turn.
func (p *Pool) flushEvents(s *slot) {
	for {
		select {
		case <-s.events:
		default:
			return
		}
	}
}

// Client returns the underlying pi RPC client for this slot.
func (s *Slot) Client() *rpc.Client { return s.slot.client }

// Channel returns the channel this slot is bound to.
func (s *Slot) Channel() ChannelID { return s.channel }

// Events returns a channel of streaming events forwarded from the pi process
// (agent_start, message_update, agent_end, ...). Events are best-effort: if
// the consumer falls behind, events may be dropped (but streaming state
// tracking is unaffected).
func (s *Slot) Events() <-chan rpc.Event { return s.slot.events }
