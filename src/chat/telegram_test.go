package chat

// telegram_test.go exercises the Telegram front-end (Phase 6 of the gateway
// plan) using a fake TelegramAPI and a real pool backed by chat's fake pi. No
// network is hit. Tests cover the plan's test list:
//   - OnMessage_AcquiresAndReplies
//   - Streaming_EditsSingleMessage (throttled edit-in-place, final text matches)
//   - MidStreamUserMessage_Steers (NOT a second Acquire)
//   - Over4096Chars_Splits
//   - IgnoresSelf
//   - OnlyRespondsInAllowedChats
// plus a light Start/Stop poll-loop test for the main.go wiring path.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
)

// --- fake Telegram API ----------------------------------------------------

type sentMsg struct {
	chatID  int64
	text    string
	replyTo int64
	msgID   int64
}
type editMsg struct {
	chatID    int64
	messageID int64
	text      string
}

type fakeTelegramAPI struct {
	mu      sync.Mutex
	me      User
	sends   []sentMsg
	edits   []editMsg
	updates chan Update // for GetUpdates (poll-loop test)
}

func newFakeTelegramAPI(botID int64) *fakeTelegramAPI {
	return &fakeTelegramAPI{
		me:      User{ID: botID, IsBot: true, UserName: "wall_e_test_bot"},
		updates: make(chan Update, 16),
	}
}

func (a *fakeTelegramAPI) GetMe(ctx context.Context) (User, error) {
	return a.me, nil
}

func (a *fakeTelegramAPI) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	select {
	case u := <-a.updates:
		return []Update{u}, nil
	case <-time.After(time.Duration(timeout) * time.Second):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *fakeTelegramAPI) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := int64(len(a.sends) + 1)
	a.sends = append(a.sends, sentMsg{chatID: chatID, text: text, replyTo: replyTo, msgID: id})
	return Message{MessageID: id, Chat: Chat{ID: chatID}}, nil
}

func (a *fakeTelegramAPI) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.edits = append(a.edits, editMsg{chatID: chatID, messageID: messageID, text: text})
	return nil
}

func (a *fakeTelegramAPI) sendCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sends)
}
func (a *fakeTelegramAPI) editCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.edits)
}
func (a *fakeTelegramAPI) lastEditText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.edits) == 0 {
		return ""
	}
	return a.edits[len(a.edits)-1].text
}
func (a *fakeTelegramAPI) lastSendText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sends) == 0 {
		return ""
	}
	return a.sends[len(a.sends)-1].text
}
func (a *fakeTelegramAPI) sendsFor(chatID int64) []sentMsg {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []sentMsg
	for _, s := range a.sends {
		if s.chatID == chatID {
			out = append(out, s)
		}
	}
	return out
}

// --- test pool + bot builders --------------------------------------------

// testPool builds a real pool backed by the fake factory + a per-slot handler.
func testPool(t *testing.T, handler func(f *fakePI, cmd map[string]any)) (*pool.Pool, *fakeFactory) {
	t.Helper()
	dir := t.TempDir()
	sm, err := session.New(session.Config{SessionDir: dir})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := sm.RebuildFromDir(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	ff := newFakeFactory()
	newClient := func(cfg rpc.Config) (*rpc.Client, error) {
		f := newFakePI()
		f.start(handler)
		c := rpc.NewClientFromStreams(f.stdinWriter, f.stdoutReader, cfg)
		ff.mu.Lock()
		ff.fakes = append(ff.fakes, f)
		ff.mu.Unlock()
		return c, nil
	}
	p, err := pool.New(pool.Config{
		Size:         4,
		DrainTimeout: 200 * time.Millisecond,
		Sessions:     sm,
		RPCConfig:    rpc.Config{UIPolicy: rpc.DefaultExtensionUIPolicy()},
		NewClient:    newClient,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		ff.closeAll()
	})
	return p, ff
}

// newTestBot builds a Bot wired for direct handleMessage calls (no Start/poll).
// editInterval is short so throttled edits are observable within a test;
// idleTimeout is disabled (0).
func newTestBot(t *testing.T, p *pool.Pool, api *fakeTelegramAPI, allowed ...int64) *Bot {
	t.Helper()
	bot, err := NewTelegram(Config{Token: "fake", AllowedChats: allowed}, p, api)
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	bot.editInterval = 20 * time.Millisecond
	bot.idleTimeout = 0
	bot.ctx, bot.cancel = context.WithCancel(context.Background())
	bot.botID = api.me.ID
	t.Cleanup(func() {
		if bot.cancel != nil {
			bot.cancel()
		}
	})
	return bot
}

// --- tests ---------------------------------------------------------------

// TestTelegram_OnMessage_AcquiresAndReplies: an incoming message acquires a
// pool slot for the chat, and on agent_end the bot sends a message whose text
// is the concatenated assistant text deltas.
func TestTelegram_OnMessage_AcquiresAndReplies(t *testing.T) {
	api := newFakeTelegramAPI(99)
	script := []scriptedEvent{
		{kind: "delta", text: "Hello", delay: 0},
		{kind: "delta", text: " world", delay: 10 * time.Millisecond},
		{kind: "agent_end", delay: 10 * time.Millisecond},
	}
	p, ff := testPool(t, makeScriptedHandler(script, nil))
	bot := newTestBot(t, p, api)

	bot.handleMessage(Message{
		Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "hi",
	})

	fake := ff.first()
	if fake == nil {
		t.Fatal("no fake pi spawned (Acquire not called?)")
	}
	// Acquire happened: switch_session + prompt received.
	if !fake.contains(`"type":"switch_session"`) {
		t.Error("expected switch_session command (slot acquired)")
	}
	if !fake.contains(`"type":"prompt"`) {
		t.Error("expected prompt command")
	}
	// The final message text == concatenated deltas "Hello world".
	finalText := api.lastEditText()
	if finalText == "" {
		// Possibly no throttled edit fired; the final send/edit must still be
		// the concatenated text.
		finalText = api.lastSendText()
	}
	if finalText != "Hello world" {
		t.Errorf("final message text = %q, want %q", finalText, "Hello world")
	}
}

// TestTelegram_Streaming_EditsSingleMessage: for a long turn the bot creates
// ONE message (sendMessage once) and edits it as deltas arrive (throttled to
// ~1 edit/editInterval); the final edit text == full concatenated text.
func TestTelegram_Streaming_EditsSingleMessage(t *testing.T) {
	api := newFakeTelegramAPI(99)
	script := []scriptedEvent{
		{kind: "delta", text: "a", delay: 0},
		{kind: "delta", text: "b", delay: 80 * time.Millisecond},
		{kind: "delta", text: "c", delay: 80 * time.Millisecond},
		{kind: "delta", text: "d", delay: 80 * time.Millisecond},
		{kind: "agent_end", delay: 80 * time.Millisecond},
	}
	p, _ := testPool(t, makeScriptedHandler(script, nil))
	bot := newTestBot(t, p, api)

	bot.handleMessage(Message{
		Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "go",
	})

	if got := api.sendCount(); got != 1 {
		t.Errorf("sendMessage calls = %d, want 1 (single message edited in place)", got)
	}
	if got := api.editCount(); got < 2 {
		t.Errorf("editMessageText calls = %d, want >= 2 (throttled edits)", got)
	}
	if got := api.lastEditText(); got != "abcd" {
		t.Errorf("final edit text = %q, want %q", got, "abcd")
	}
}

// TestTelegram_MidStreamUserMessage_Steers: while a turn is streaming for
// chat X, a second message from chat X issues Steer (NOT a new Acquire).
func TestTelegram_MidStreamUserMessage_Steers(t *testing.T) {
	api := newFakeTelegramAPI(99)
	streamDone := make(chan struct{})
	p, ff := testPool(t, makeScriptedHandler(nil, streamDone))
	bot := newTestBot(t, p, api)

	aDone := make(chan struct{})
	go func() {
		bot.handleMessage(Message{Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "first"})
		close(aDone)
	}()

	fake := ff.waitForFirst(2 * time.Second)
	if fake == nil {
		t.Fatal("no fake pi spawned")
	}
	// Wait for the first turn's prompt to reach the fake pi.
	if !fake.waitForCommand(`"type":"prompt"`, 2*time.Second) {
		t.Fatal("first prompt not received")
	}

	// Second message from the SAME chat while the first is streaming.
	bot.handleMessage(Message{Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "second message"})

	// Steer was sent (not a second prompt).
	if !fake.contains(`"type":"steer"`) {
		t.Error("expected steer command for mid-stream message")
	}
	if !fake.contains("second message") {
		t.Error("expected steer to carry the second message text")
	}
	if got := fake.count(`"type":"prompt"`); got != 1 {
		t.Errorf("prompt count = %d, want 1 (second message must NOT Acquire+Prompt)", got)
	}

	// Release the first turn and let it finish.
	close(streamDone)
	select {
	case <-aDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first turn did not finish after streamDone closed")
	}
}

// TestTelegram_Over4096Chars_Splits: assistant text > 4096 chars is split
// across multiple sendMessage calls on agent_end.
func TestTelegram_Over4096Chars_Splits(t *testing.T) {
	api := newFakeTelegramAPI(99)
	bigText := strings.Repeat("x", 5000)
	script := []scriptedEvent{
		{kind: "delta", text: bigText, delay: 0},
		{kind: "agent_end", delay: 10 * time.Millisecond},
	}
	p, _ := testPool(t, makeScriptedHandler(script, nil))
	bot := newTestBot(t, p, api)

	bot.handleMessage(Message{
		Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "big",
	})

	sends := api.sendsFor(42)
	if len(sends) < 2 {
		t.Fatalf("sendMessage calls = %d, want >= 2 (split)", len(sends))
	}
	// Concatenation of all sent messages == the full text.
	var b strings.Builder
	for _, s := range sends {
		b.WriteString(s.text)
	}
	if got := b.String(); got != bigText {
		if len(got) != len(bigText) {
			t.Errorf("concatenated sent text length = %d, want %d", len(got), len(bigText))
		} else {
			t.Errorf("concatenated sent text != bigText (lengths equal, content differs)")
		}
	}
	// The second chunk is a reply to the first message.
	if sends[len(sends)-1].replyTo != sends[0].msgID {
		t.Errorf("last chunk replyTo = %d, want %d (thread off the first chunk)",
			sends[len(sends)-1].replyTo, sends[0].msgID)
	}
}

// TestTelegram_IgnoresSelf: messages authored by the bot itself are ignored to
// avoid loops.
func TestTelegram_IgnoresSelf(t *testing.T) {
	api := newFakeTelegramAPI(99)
	script := []scriptedEvent{{kind: "agent_end", delay: 0}}
	p, ff := testPool(t, makeScriptedHandler(script, nil))
	bot := newTestBot(t, p, api)

	bot.handleMessage(Message{
		Chat: Chat{ID: 42}, From: User{ID: 99}, Text: "self", // From == bot id
	})

	if got := api.sendCount(); got != 0 {
		t.Errorf("sendMessage calls = %d, want 0 (self ignored)", got)
	}
	if ff.first() != nil {
		// A slot would only be Acquired if the bot had acted; ensure no prompt.
		if ff.first().contains(`"type":"prompt"`) {
			t.Error("expected no prompt for self-authored message")
		}
	}
}

// TestTelegram_OnlyRespondsInAllowedChats: if WALLE_TELEGRAM_ALLOWED_CHATS is
// set, messages from other chats are ignored.
func TestTelegram_OnlyRespondsInAllowedChats(t *testing.T) {
	api := newFakeTelegramAPI(99)
	script := []scriptedEvent{
		{kind: "delta", text: "ok", delay: 0},
		{kind: "agent_end", delay: 10 * time.Millisecond},
	}
	p, _ := testPool(t, makeScriptedHandler(script, nil))
	bot := newTestBot(t, p, api, 123) // allowlist: only chat 123

	// Disallowed chat → ignored.
	bot.handleMessage(Message{Chat: Chat{ID: 456}, From: User{ID: 7}, Text: "nope"})
	if got := api.sendCount(); got != 0 {
		t.Errorf("sendMessage for disallowed chat = %d, want 0", got)
	}

	// Allowed chat → processed.
	bot.handleMessage(Message{Chat: Chat{ID: 123}, From: User{ID: 7}, Text: "yes"})
	if got := api.sendCount(); got == 0 {
		t.Errorf("sendMessage for allowed chat = 0, want > 0")
	}
}

// TestTelegram_PollLoop_DispatchesAndStops: the Start/Stop path (getMe +
// getUpdates long-poll + dispatch + drain) works end-to-end with the fake API.
func TestTelegram_PollLoop_DispatchesAndStops(t *testing.T) {
	api := newFakeTelegramAPI(99)
	script := []scriptedEvent{
		{kind: "delta", text: "hi", delay: 0},
		{kind: "agent_end", delay: 10 * time.Millisecond},
	}
	p, _ := testPool(t, makeScriptedHandler(script, nil))
	bot, err := NewTelegram(Config{Token: "fake"}, p, api)
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	bot.editInterval = 20 * time.Millisecond
	bot.idleTimeout = 0

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Inject one incoming message via the poll loop's update channel.
	api.updates <- Update{
		UpdateID: 1,
		Message: &Message{Chat: Chat{ID: 42}, From: User{ID: 7}, Text: "hello"},
	}

	// Wait for the bot to send a reply (the turn finalizes the message).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && api.sendCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := api.sendCount(); got == 0 {
		t.Fatal("bot did not reply to the polled message")
	}

	// Stop drains the poll loop + in-flight turns.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := bot.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
