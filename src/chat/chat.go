// Package chat implements the wall-e chat front-ends (Telegram, and later
// Discord). Each front-end reads messages from a chat platform, routes every
// chat to the shared worker pool (one live `pi --mode rpc` process per chat,
// bound via pool.Acquire/Release), and streams the assistant reply back by
// editing a single message in place (throttled to the platform's rate limit).
//
// A mid-stream message from a chat that already has an in-flight turn is
// forwarded as a `steer` (NOT a new Acquire) — the pool's per-channel
// serialization would otherwise block the second message until the first turn
// Releases, which is not the desired chat UX.
//
// The platform-facing surface is a small interface (TelegramAPI / future
// DiscordAPI) so the real adapter (telegram.go, hand-rolled over net/http) is a
// thin shim and unit tests inject a fake — no network is hit in tests.
//
// The bot owns NO per-channel serialization: that lives in the pool. The one
// piece of per-chat state the bot does keep is a map[chatID]*turnState
// (guarded by a mutex) used to decide Acquire-vs-Steer for an incoming message
// and to hand the in-flight slot to a steering message. Documented in the Phase
// 6 log.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"wall-e/pool"
	"wall-e/rpc"
)

// Frontend is a chat front-end (Telegram, Discord, ...) the gateway runs
// alongside the HTTP server. Start launches its event loop; Stop cancels it and
// drains in-flight turns. main treats a Frontend like the HTTP server: start in
// a goroutine, Stop on shutdown signal.
type Frontend interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// TelegramAPI is the subset of the Telegram Bot API the bot uses. The real
// adapter (telegram.go) calls api.telegram.org over net/http; tests inject a
// fake implementation so no network is hit.
type TelegramAPI interface {
	// GetMe returns the bot's own user (for self-message suppression).
	GetMe(ctx context.Context) (User, error)
	// GetUpdates long-polls for incoming updates. offset is the next update_id
	// to fetch (0 = all pending); timeout is the long-poll seconds.
	GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error)
	// SendMessage posts a text message to a chat, optionally as a reply to
	// replyTo (0 = no reply). Returns the created message.
	SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (Message, error)
	// EditMessageText replaces the text of an existing message.
	EditMessageText(ctx context.Context, chatID int64, messageID int64, text string) error
}

// User is a Telegram user (the bot itself, or a message author).
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	UserName  string `json:"username,omitempty"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type,omitempty"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      User   `json:"from"`
	Text      string `json:"text"`
}

// Update is a single Telegram update (we only consume message updates in v1).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// TelegramMaxMessageLen is Telegram's per-message text limit (4096 characters).
// We chunk on rune boundaries to stay safe for multi-byte text; see
// finalizeMessage.
const TelegramMaxMessageLen = 4096

// Config configures the Telegram front-end.
type Config struct {
	// Token is the Telegram bot token (from @BotFather). If empty, the
	// gateway skips the Telegram front-end entirely (HTTP still serves).
	Token string
	// AllowedChats is an optional allowlist of chat ids. If non-empty, only
	// messages from these chats are processed; others are ignored. Mirrors the
	// Discord plan's WALLE_DISCORD_ALLOWED_CHANNELS.
	AllowedChats []int64
}

// Bot is the Telegram front-end. It is a Frontend: Start launches the
// getUpdates long-poll loop; Stop cancels it and drains in-flight turns.
type Bot struct {
	api     TelegramAPI
	pool    *pool.Pool
	allowed map[int64]bool

	// editInterval is the throttle for edit-in-place (Telegram's rate limit is
	// ~30 edits/min ≈ 1 edit/2s; we use 1 edit/sec as a safe default). The
	// final agent_end edit bypasses the throttle.
	editInterval time.Duration
	// idleTimeout bounds how long a turn may go without any event before the
	// bot gives up and finalizes with whatever text it has. Guards against a
	// stuck pi process (the pool's Slot.Events() is never closed — see Phase 5
	// log — so without a watchdog a dead process would hang the turn forever).
	// 0 = disabled (tests).
	idleTimeout time.Duration

	botID int64

	ctx     context.Context
	cancel  context.CancelFunc
	pollDone chan struct{}

	// turnsWG tracks in-flight turn goroutines so Stop can drain them.
	turnsWG sync.WaitGroup

	// turns maps chatID → the active turn for that chat. Guarded by turnsMu.
	// An incoming message with an active turn steers instead of acquiring.
	turnsMu sync.Mutex
	turns   map[int64]*turnState
}

// turnState is the chat-layer's one piece of per-chat state: enough to hand a
// steering message to the in-flight turn's slot.
//
// Lifecycle: created (and stored in turns) BEFORE pool.Acquire so a concurrent
// message for the same chat sees an active turn and steers. slotReady is closed
// once the Acquire completes (slot or err set); readers wait on it before
// touching slot/acquireErr (the close happens-after those writes per Go's
// memory model). done is closed when the turn has fully finished (released).
type turnState struct {
	slotReady chan struct{}
	done      chan struct{}

	slot       *pool.Slot
	acquireErr error
}

// NewTelegram builds a Telegram front-end. If api is nil, a real net/http
// adapter over api.telegram.org is used; tests pass a fake.
func NewTelegram(cfg Config, p *pool.Pool, api TelegramAPI) (*Bot, error) {
	if cfg.Token == "" {
		return nil, errors.New("chat: telegram token is required")
	}
	if p == nil {
		return nil, errors.New("chat: pool is required")
	}
	if api == nil {
		api = newHTTPTelegramAPI(cfg.Token, defaultTelegramBaseURL)
	}
	allowed := make(map[int64]bool, len(cfg.AllowedChats))
	for _, id := range cfg.AllowedChats {
		allowed[id] = true
	}
	return &Bot{
		api:          api,
		pool:         p,
		allowed:      allowed,
		editInterval: 1 * time.Second,
		idleTimeout:  5 * time.Minute,
		turns:        make(map[int64]*turnState),
	}, nil
}

// Start validates the bot token (GetMe), records the bot's user id (for
// self-message suppression), and launches the getUpdates long-poll loop. It
// returns once the loop is running; the loop runs until Stop or ctx is
// cancelled.
func (b *Bot) Start(ctx context.Context) error {
	me, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("chat: telegram getMe: %w", err)
	}
	b.botID = me.ID
	log.Printf("telegram: connected as @%s (id=%d)", me.UserName, me.ID)

	b.ctx, b.cancel = context.WithCancel(ctx)
	b.pollDone = make(chan struct{})
	go b.pollLoop()
	return nil
}

// pollLoop runs the getUpdates long-poll until b.ctx is cancelled. Each received
// update is dispatched to handleMessage in its own goroutine so a slow turn
// doesn't block polling. The offset is advanced past the last update_id so a
// restart doesn't replay old messages (in-memory only for v1; persistence is
// deferred).
func (b *Bot) pollLoop() {
	defer close(b.pollDone)
	var offset int64
	for {
		if b.ctx.Err() != nil {
			return
		}
		ups, err := b.api.GetUpdates(b.ctx, offset, 30)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			log.Printf("telegram: getUpdates: %v (retrying in 2s)", err)
			select {
			case <-time.After(2 * time.Second):
			case <-b.ctx.Done():
				return
			}
			continue
		}
		for i := range ups {
			u := ups[i]
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil {
				continue
			}
			msg := *u.Message
			b.turnsWG.Add(1)
			go func() {
				defer b.turnsWG.Done()
				b.handleMessage(msg)
			}()
		}
	}
}

// Stop cancels the poll loop and waits for it plus in-flight turns to drain,
// bounded by ctx (best-effort: does NOT block forever on a stuck pi).
func (b *Bot) Stop(ctx context.Context) error {
	if b.cancel != nil {
		b.cancel()
	}
	if b.pollDone != nil {
		select {
		case <-b.pollDone:
		case <-ctx.Done():
		}
	}
	// Wait for in-flight turns (bounded). A stuck turn is dropped — its ctx is
	// cancelled so Acquire/Prompt/Steer return promptly and the slot Releases.
	done := make(chan struct{})
	go func() {
		b.turnsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// handleMessage routes one incoming message: ignore self / non-text / disallowed
// chats; if a turn is already active for the chat, steer it; else acquire a slot
// and run a fresh turn.
func (b *Bot) handleMessage(msg Message) {
	// Ignore our own messages (avoid loops).
	if msg.From.ID == b.botID {
		return
	}
	chatID := msg.Chat.ID
	if len(b.allowed) > 0 && !b.allowed[chatID] {
		return
	}
	text := msg.Text
	if text == "" {
		return // v1: only text messages
	}

	for {
		b.turnsMu.Lock()
		ts := b.turns[chatID]
		if ts == nil {
			// No active turn → start one. Register the turnState BEFORE
			// Acquire so a concurrent message for the same chat steers.
			ts = &turnState{
				slotReady: make(chan struct{}),
				done:      make(chan struct{}),
			}
			b.turns[chatID] = ts
			b.turnsMu.Unlock()
			b.runTurn(chatID, text, ts) // runs in this goroutine
			return
		}
		b.turnsMu.Unlock()

		// Active turn → steer. If the turn turned out stale (finished between
		// our map check and here), loop and start a fresh turn.
		if b.steer(chatID, text, ts) {
			return
		}
	}
}

// steer forwards a mid-stream message to an active turn's slot as a `steer`
// command (NOT a new Acquire). Returns true if handled (steered or dropped on
// error/shutdown); false if the turn turned out stale and the caller should
// retry as a fresh turn.
func (b *Bot) steer(chatID int64, text string, ts *turnState) (handled bool) {
	select {
	case <-ts.slotReady:
	case <-b.ctx.Done():
		return true // shutting down; drop
	}
	// Re-check the turn is still active (runTurn removes it from the map when
	// done). If it finished, retry as a fresh turn.
	b.turnsMu.Lock()
	active := b.turns[chatID] == ts
	b.turnsMu.Unlock()
	if !active {
		return false
	}
	if ts.acquireErr != nil {
		// The turn never got a slot; it will clean up and unregister. Drop the
		// steer rather than racing a fresh acquire.
		log.Printf("telegram: steer skipped for chat %d (acquire failed earlier)", chatID)
		return true
	}
	if _, err := ts.slot.Client().Steer(b.ctx, text); err != nil {
		log.Printf("telegram: steer chat %d: %v", chatID, err)
	}
	return true
}

// runTurn acquires a slot for the chat, sends the prompt, streams the reply,
// edits a single message in place (throttled), finalizes on agent_end (split if
// >4096 chars), and releases the slot. runTurn owns the slot for the turn's
// lifetime (matching the pool's "slot stays bound until Release").
func (b *Bot) runTurn(chatID int64, text string, ts *turnState) {
	defer close(ts.done)
	defer func() {
		b.turnsMu.Lock()
		if b.turns[chatID] == ts {
			delete(b.turns, chatID)
		}
		b.turnsMu.Unlock()
	}()

	chID := pool.ChannelID(strconv.FormatInt(chatID, 10))
	slot, err := b.pool.Acquire(b.ctx, chID)
	ts.slot = slot
	ts.acquireErr = err
	close(ts.slotReady)
	if err != nil {
		log.Printf("telegram: acquire chat %d: %v", chatID, err)
		_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ no agent available: "+err.Error(), 0)
		return
	}
	defer b.pool.Release(chID)

	if _, err := slot.Client().Prompt(b.ctx, text, false); err != nil {
		log.Printf("telegram: prompt chat %d: %v", chatID, err)
		return
	}
	b.streamTurn(chatID, slot)
}

// streamTurn consumes the slot's event stream, accumulating text deltas into a
// buffer and editing a single message in place (throttled to editInterval). On
// agent_end it finalizes: a final edit (or split into multiple messages if the
// text exceeds Telegram's 4096-char limit).
func (b *Bot) streamTurn(chatID int64, slot *pool.Slot) {
	var buf strings.Builder
	var msgID int64 // 0 = no message sent yet
	var lastSent string
	dirty := false
	turnDone := false

	ticker := time.NewTicker(b.editInterval)
	defer ticker.Stop()

	// idle watchdog: if no event arrives for idleTimeout, finalize and bail
	// (guards against a stuck pi process; Slot.Events() is never closed).
	var idle *time.Timer
	var idleC <-chan time.Time
	if b.idleTimeout > 0 {
		idle = time.NewTimer(b.idleTimeout)
		defer idle.Stop()
		idleC = idle.C
	}
	resetIdle := func() {
		if idle != nil {
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(b.idleTimeout)
		}
	}

	// flush emits a throttled edit of the current buffer (capped to the
	// message limit). Skipped if nothing new or no message yet.
	flush := func() {
		if !dirty || msgID == 0 {
			return
		}
		s := truncateRunes(buf.String(), TelegramMaxMessageLen)
		if s == lastSent {
			dirty = false
			return
		}
		if err := b.api.EditMessageText(b.ctx, chatID, msgID, s); err != nil {
			// "message is not modified" and rate-limit errors are logged but
			// non-fatal; we keep accumulating and retry on the next tick.
			log.Printf("telegram: editMessage chat %d: %v", chatID, err)
			return
		}
		lastSent = s
		dirty = false
	}

	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-slot.Events():
			if !ok {
				// Stream closed (process died). Finalize with what we have.
				b.finalizeMessage(chatID, msgID, buf.String(), lastSent)
				return
			}
			resetIdle()
			switch ev.Type {
			case rpc.EventAgentStart:
				// nothing
			case rpc.EventMessageUpdate:
				if d, ok := decodeTextDelta(ev.Raw); ok && d != "" {
					buf.WriteString(d)
					dirty = true
					if msgID == 0 {
						// First delta: send the initial message so the user
						// sees the reply start immediately.
						m, err := b.api.SendMessage(b.ctx, chatID,
							truncateRunes(buf.String(), TelegramMaxMessageLen), 0)
						if err == nil {
							msgID = m.MessageID
							lastSent = truncateRunes(buf.String(), TelegramMaxMessageLen)
							dirty = false
						}
					}
				}
			case rpc.EventAgentEnd:
				turnDone = true
			}
		case <-ticker.C:
			flush()
		case <-idleC:
			log.Printf("telegram: turn idle for %s, finalizing chat %d", b.idleTimeout, chatID)
			b.finalizeMessage(chatID, msgID, buf.String(), lastSent)
			return
		}
		if turnDone {
			b.finalizeMessage(chatID, msgID, buf.String(), lastSent)
			return
		}
	}
}

// finalizeMessage writes the final assistant text. If a streaming message
// exists (msgID != 0) and the text fits, it's a final edit; if it exceeds the
// limit, the first chunk pins the existing message and the rest are sent as
// replies. If no streaming message exists (empty response), the whole text is
// sent fresh (split if needed).
//
// Chunking is rune-safe: we split on rune boundaries at TelegramMaxMessageLen
// so multi-byte text never produces an invalid UTF-8 boundary. Telegram's
// limit is 4096 "characters" (UTF-16 code units for the API); for BMP text
// runes == chars, which is the common case. Non-BMP (emoji) may occasionally
// allow one fewer chunk than the limit permits — acceptable for v1.
func (b *Bot) finalizeMessage(chatID int64, msgID int64, final string, lastSent string) {
	runes := []rune(final)

	if msgID == 0 {
		text := final
		if text == "" {
			text = "(no response)"
		}
		b.sendChunks(chatID, []rune(text), 0)
		return
	}

	if len(runes) <= TelegramMaxMessageLen {
		if final != lastSent {
			_ = b.api.EditMessageText(b.ctx, chatID, msgID, final)
		}
		return
	}

	// Final text exceeds the limit: pin the first chunk in the existing
	// message, then send the rest as replies.
	first := string(runes[:TelegramMaxMessageLen])
	if first != lastSent {
		_ = b.api.EditMessageText(b.ctx, chatID, msgID, first)
	}
	b.sendChunks(chatID, runes[TelegramMaxMessageLen:], msgID)
}

// sendChunks sends runes as a sequence of <=TelegramMaxMessageLen-char messages,
// each replying to replyTo (0 = no reply).
func (b *Bot) sendChunks(chatID int64, runes []rune, replyTo int64) {
	for len(runes) > 0 {
		n := len(runes)
		if n > TelegramMaxMessageLen {
			n = TelegramMaxMessageLen
		}
		_, _ = b.api.SendMessage(b.ctx, chatID, string(runes[:n]), replyTo)
		runes = runes[n:]
	}
}

// decodeTextDelta extracts the text delta from a message_update event's
// assistantMessageEvent. Returns (delta, true) for text_delta deltas and
// (..., false) for anything else. Mirrors httpapi's helper; duplicated to keep
// chat/ self-contained (httpapi's is unexported).
func decodeTextDelta(raw []byte) (string, bool) {
	var ev struct {
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", false
	}
	if ev.AssistantMessageEvent.Type != "text_delta" {
		return "", false
	}
	return ev.AssistantMessageEvent.Delta, true
}

// truncateRunes returns s truncated to at most n runes.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
