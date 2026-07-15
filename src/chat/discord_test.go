package chat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"wall-e/httpapi"
	"wall-e/media"
	"wall-e/pool"
	"wall-e/rpc"
)

type fakeDiscordAPI struct {
	mu                   sync.Mutex
	handlers             DiscordHandlers
	intents              DiscordIntent
	openErr              error
	open                 bool
	closed               bool
	ready                DiscordReady
	commands             []DiscordCommand
	registerErr          error
	sends                []DiscordSend
	edits                []DiscordEdit
	deletes              [][2]string
	typing               []string
	responses            []DiscordInteractionResponse
	interactionEdits     []DiscordEdit
	interactionDeletes   []DiscordInteraction
	followups            []DiscordSend
	sendErrorAt          map[int]error
	interactionEditErr   error
	interactionDeleteErr error
	sequence             []string
}

func newFakeDiscordAPI() *fakeDiscordAPI {
	return &fakeDiscordAPI{ready: DiscordReady{BotUserID: "999", BotUsername: "wall-e", ApplicationID: "888"}, sendErrorAt: make(map[int]error)}
}

func (a *fakeDiscordAPI) SetHandlers(h DiscordHandlers) { a.mu.Lock(); a.handlers = h; a.mu.Unlock() }
func (a *fakeDiscordAPI) SetIntents(i DiscordIntent)    { a.mu.Lock(); a.intents = i; a.mu.Unlock() }
func (a *fakeDiscordAPI) Open(context.Context) error {
	a.mu.Lock()
	a.open = true
	err := a.openErr
	h := a.handlers.Ready
	ready := a.ready
	a.mu.Unlock()
	if err == nil && h != nil && ready.BotUserID != "" {
		h(ready)
	}
	return err
}
func (a *fakeDiscordAPI) Close() error { a.mu.Lock(); a.closed = true; a.mu.Unlock(); return nil }
func (a *fakeDiscordAPI) BulkOverwriteGlobalCommands(_ context.Context, _ string, commands []DiscordCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commands = append([]DiscordCommand(nil), commands...)
	return a.registerErr
}
func (a *fakeDiscordAPI) SendMessage(_ context.Context, send DiscordSend) (DiscordMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := len(a.sends)
	a.sends = append(a.sends, send)
	a.sequence = append(a.sequence, "send")
	if err := a.sendErrorAt[index]; err != nil {
		return DiscordMessage{}, err
	}
	return DiscordMessage{ID: "m" + itoaSmall(index+1), ChannelID: send.ChannelID, Content: send.Content}, nil
}
func (a *fakeDiscordAPI) EditMessage(_ context.Context, edit DiscordEdit) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.edits = append(a.edits, edit)
	return nil
}
func (a *fakeDiscordAPI) DeleteMessage(_ context.Context, channel, message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deletes = append(a.deletes, [2]string{channel, message})
	return nil
}
func (a *fakeDiscordAPI) TriggerTyping(_ context.Context, channel string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.typing = append(a.typing, channel)
	return nil
}
func (a *fakeDiscordAPI) RespondInteraction(_ context.Context, _ DiscordInteraction, response DiscordInteractionResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.responses = append(a.responses, response)
	a.sequence = append(a.sequence, "respond")
	return nil
}
func (a *fakeDiscordAPI) EditInteractionResponse(_ context.Context, _ DiscordInteraction, edit DiscordEdit) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interactionEdits = append(a.interactionEdits, edit)
	a.sequence = append(a.sequence, "interaction-edit")
	return a.interactionEditErr
}
func (a *fakeDiscordAPI) DeleteInteractionResponse(_ context.Context, interaction DiscordInteraction) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interactionDeletes = append(a.interactionDeletes, interaction)
	a.sequence = append(a.sequence, "interaction-delete")
	return a.interactionDeleteErr
}
func (a *fakeDiscordAPI) CreateInteractionFollowup(_ context.Context, _ DiscordInteraction, send DiscordSend) (DiscordMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followups = append(a.followups, send)
	return DiscordMessage{ID: "f" + itoaSmall(len(a.followups))}, nil
}
func (a *fakeDiscordAPI) emitMessage(message DiscordMessage) {
	a.mu.Lock()
	h := a.handlers.MessageCreate
	a.mu.Unlock()
	h(message)
}
func (a *fakeDiscordAPI) emitInteraction(interaction DiscordInteraction) {
	a.mu.Lock()
	h := a.handlers.InteractionCreate
	a.mu.Unlock()
	h(interaction)
}
func (a *fakeDiscordAPI) snapshot() (sends []DiscordSend, edits []DiscordEdit, deletes [][2]string, typing []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]DiscordSend(nil), a.sends...), append([]DiscordEdit(nil), a.edits...), append([][2]string(nil), a.deletes...), append([]string(nil), a.typing...)
}

type fakeDiscordFetcher struct {
	mu    sync.Mutex
	data  map[string]string
	calls []string
	err   error
}

func (f *fakeDiscordFetcher) Fetch(_ context.Context, u string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, u)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.data[u])), nil
}

// newStartedDiscordBot uses the chat package's shared fake-pi pool helper.
func newStartedDiscordBot(t *testing.T, p *pool.Pool, api *fakeDiscordAPI, cfg DiscordConfig) *DiscordBot {
	t.Helper()
	cfg.Token = "fake-token"
	cfg.ReadyTimeout = time.Second
	cfg.EditInterval = 5 * time.Millisecond
	cfg.TypingInterval = 5 * time.Millisecond
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Second
	}
	bot, err := NewDiscord(cfg, p, api)
	if err != nil {
		t.Fatalf("NewDiscord: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := bot.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = bot.Stop(stopCtx)
	})
	return bot
}

func waitDiscord(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("timed out waiting for Discord operation")
	}
}

func mentionsDisabled(t *testing.T, mentions DiscordAllowedMentions) {
	t.Helper()
	if mentions.Parse == nil || len(mentions.Parse) != 0 || len(mentions.Users) != 0 || len(mentions.Roles) != 0 || mentions.RepliedUser {
		t.Fatalf("allowed mentions not disabled: %+v", mentions)
	}
}

func TestDiscordStartLifecycleIntentsAndCommands(t *testing.T) {
	api := newFakeDiscordAPI()
	p, _ := testPool(t, makeScriptedHandler(nil, nil))
	provider := func(context.Context) ([]rpc.Command, error) {
		return []rpc.Command{{Name: "fix-tests", Description: "Fix tests", Source: "prompt"}, {Name: "skill:brave-search", Source: "skill"}}, nil
	}
	bot := newStartedDiscordBot(t, p, api, DiscordConfig{CommandProvider: provider})
	api.mu.Lock()
	if api.intents != discordRequiredIntents {
		t.Errorf("intents=%v want %v", api.intents, discordRequiredIntents)
	}
	if !api.open || len(api.commands) != 8 {
		t.Errorf("open=%v commands=%v", api.open, api.commands)
	}
	for _, command := range api.commands {
		if len(command.Name) > 32 || len([]rune(command.Description)) > 100 {
			t.Fatalf("invalid command %+v", command)
		}
	}
	api.mu.Unlock()
	bot.identityMu.RLock()
	defer bot.identityMu.RUnlock()
	if bot.botID != "999" || bot.applicationID != "888" {
		t.Fatalf("identity=%q/%q", bot.botID, bot.applicationID)
	}
}

func TestDiscordStartOpenAndReadyFailures(t *testing.T) {
	p, _ := testPool(t, makeScriptedHandler(nil, nil))
	api := newFakeDiscordAPI()
	api.openErr = errors.New("gateway down")
	bot, err := NewDiscord(DiscordConfig{Token: "x", ReadyTimeout: 20 * time.Millisecond}, p, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.Start(context.Background()); err == nil {
		t.Fatal("expected Open error")
	}
	api = newFakeDiscordAPI()
	api.ready = DiscordReady{}
	bot, err = NewDiscord(DiscordConfig{Token: "x", ReadyTimeout: 20 * time.Millisecond}, p, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "Ready") {
		t.Fatalf("Ready error=%v", err)
	}
}

func TestDiscordGuildDMAndThreadUseEventChannelIdentity(t *testing.T) {
	for name, channelID := range map[string]string{"guild": "100", "dm": "200", "thread": "300"} {
		t.Run(name, func(t *testing.T) {
			if got := discordChannelID(channelID).Address(); got != "discord:"+channelID {
				t.Fatalf("channel address = %q", got)
			}
		})
	}
}

func TestDiscordMessageBuffersUntilCompletionIdentityTypingAndMentions(t *testing.T) {
	api := newFakeDiscordAPI()
	script := []scriptedEvent{{kind: "delta", text: "Hello @everyone"}, {kind: "delta", text: " **world**", delay: 15 * time.Millisecond}, {kind: "agent_end", delay: 10 * time.Millisecond}}
	p, ff, sm := testPoolWithManager(t, makeScriptedHandler(script, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{AllowedChannels: []string{"123"}})
	api.emitMessage(DiscordMessage{ID: "1", ChannelID: "123", GuildID: "55", Author: &DiscordUser{ID: "7"}, Content: "hi"})
	waitDiscord(t, func() bool { sends, _, _, _ := api.snapshot(); return len(sends) == 1 })
	fake := ff.first()
	if fake == nil || !fake.contains(`"type":"prompt"`) {
		t.Fatal("prompt missing")
	}
	cur, _ := sm.Current(discordChannelID("123"))
	if !strings.HasPrefix(filepath.Base(cur), "discord--123--") {
		t.Fatalf("session=%s", cur)
	}
	sends, edits, deletes, typing := api.snapshot()
	if len(sends) != 1 || sends[0].Content != "Hello @everyone **world**" {
		t.Fatalf("sends=%+v", sends)
	}
	if len(edits) != 0 || len(deletes) != 0 {
		t.Fatalf("ordinary buffered delivery made preview edits/deletes: edits=%v deletes=%v", edits, deletes)
	}
	if len(typing) == 0 {
		t.Fatalf("typing=%v", typing)
	}
	for _, send := range sends {
		mentionsDisabled(t, send.AllowedMentions)
	}
	for _, edit := range edits {
		mentionsDisabled(t, edit.AllowedMentions)
	}
}

func TestDiscordMessageFiltersAndAllowlistBeforeDownloads(t *testing.T) {
	api := newFakeDiscordAPI()
	fetch := &fakeDiscordFetcher{data: map[string]string{"https://cdn/x": "x"}}
	p, ff := testPool(t, makeScriptedHandler(nil, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{AllowedChannels: []string{"1"}, AttachmentFetcher: fetch, MediaStore: media.NewStore(t.TempDir())})
	messages := []DiscordMessage{
		{ChannelID: "2", Author: &DiscordUser{ID: "7"}, Content: "no", Attachments: []DiscordAttachment{{URL: "https://cdn/x"}}},
		{ChannelID: "1", Author: &DiscordUser{ID: "999"}, Content: "self"},
		{ChannelID: "1", Author: &DiscordUser{ID: "8", Bot: true}, Content: "bot"},
		{ChannelID: "1", Author: &DiscordUser{ID: "8"}, WebhookID: "w", Content: "webhook"},
		{ChannelID: "1", Author: nil, Content: "bad"},
		{ChannelID: "1", Author: &DiscordUser{ID: "8"}},
	}
	for _, message := range messages {
		api.emitMessage(message)
	}
	time.Sleep(30 * time.Millisecond)
	fetch.mu.Lock()
	calls := len(fetch.calls)
	fetch.mu.Unlock()
	if calls != 0 || ff.first() != nil {
		t.Fatalf("calls=%d fake=%v", calls, ff.first())
	}
}

func TestDiscordMidTurnMessageSteersAndThreadIsIndependent(t *testing.T) {
	api := newFakeDiscordAPI()
	done := make(chan struct{})
	p, ff := testPool(t, makeScriptedHandler(nil, done))
	newStartedDiscordBot(t, p, api, DiscordConfig{AllowedChannels: []string{"100", "101"}})
	api.emitMessage(DiscordMessage{ChannelID: "100", Author: &DiscordUser{ID: "7"}, Content: "first"})
	fake := ff.waitForFirst(time.Second)
	if fake == nil || !fake.waitForCommand(`"type":"prompt"`, time.Second) {
		t.Fatal("prompt missing")
	}
	api.emitMessage(DiscordMessage{ChannelID: "100", Author: &DiscordUser{ID: "7"}, Content: "second"})
	waitDiscord(t, func() bool { return fake.contains(`"type":"steer"`) })
	if fake.count(`"type":"prompt"`) != 1 {
		t.Fatalf("prompt count=%d", fake.count(`"type":"prompt"`))
	}
	api.emitMessage(DiscordMessage{ChannelID: "101", Author: &DiscordUser{ID: "7"}, Content: "thread"})
	waitDiscord(t, func() bool { return ff.count() == 2 })
	close(done)
}

func TestDiscordChunkingPreservesTextAndCountsNonBMP(t *testing.T) {
	text := strings.Repeat("a", 1800) + " \n" + strings.Repeat("😀", 1100) + strings.Repeat("z", 2001)
	chunks := splitDiscordText(text)
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks lost or duplicated text")
	}
	for _, chunk := range chunks {
		units := 0
		for _, r := range chunk {
			units++
			if r > 0xffff {
				units++
			}
		}
		if units > DiscordMaxMessageLen {
			t.Fatalf("chunk units=%d", units)
		}
		if !utf8.ValidString(chunk) {
			t.Fatal("invalid UTF-8")
		}
	}
}

func TestDiscordFinalFailureAttemptsRemainingChunksWithoutPreview(t *testing.T) {
	api := newFakeDiscordAPI()
	api.sendErrorAt[0] = errors.New("final failed")
	big := strings.Repeat("x", 2500)
	p, _ := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: big}, {kind: "agent_end"}}, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{})
	api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Content: "go"})
	waitDiscord(t, func() bool { sends, _, _, _ := api.snapshot(); return len(sends) == 2 })
	_, edits, deletes, _ := api.snapshot()
	if len(edits) != 0 || len(deletes) != 0 {
		t.Fatalf("final failure used preview fallback: edits=%v deletes=%v", edits, deletes)
	}
}

func TestDiscordHTTPSAttachmentFetcherIsBoundedAndUnauthenticated(t *testing.T) {
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()
	fetcher := &httpsDiscordAttachmentFetcher{client: server.Client(), maxBytes: 3}
	body, err := fetcher.Fetch(context.Background(), server.URL+"/file?signed=secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(body)
	_ = body.Close()
	if err == nil || !strings.Contains(err.Error(), "download limit") {
		t.Fatalf("bounded read error = %v", err)
	}
	if authorization != "" {
		t.Fatalf("CDN request Authorization = %q", authorization)
	}
	if _, err := fetcher.Fetch(context.Background(), "http://cdn.example/file"); err == nil {
		t.Fatal("non-HTTPS attachment accepted")
	}
}

func TestDiscordAttachmentsDownloadedInOrderBeforePrompt(t *testing.T) {
	api := newFakeDiscordAPI()
	fetch := &fakeDiscordFetcher{data: map[string]string{"https://cdn/1": "one", "https://cdn/2": "two"}}
	p, ff, sm := testPoolWithManager(t, makeScriptedHandler([]scriptedEvent{{kind: "agent_end"}}, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{AttachmentFetcher: fetch, MediaStore: media.NewStore(sm.SessionDir())})
	api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Attachments: []DiscordAttachment{{Filename: "../one.txt", URL: "https://cdn/1"}, {Filename: "two.txt", URL: "https://cdn/2"}}})
	fake := ff.waitForFirst(time.Second)
	if fake == nil || !fake.waitForCommand(`"type":"prompt"`, time.Second) {
		t.Fatal("prompt missing")
	}
	got := strings.Join(fake.Got(), "")
	if strings.Index(got, "one.txt") < 0 || strings.Index(got, "two.txt") < strings.Index(got, "one.txt") {
		t.Fatalf("prompt order: %s", got)
	}
	fetch.mu.Lock()
	calls := append([]string(nil), fetch.calls...)
	fetch.mu.Unlock()
	if strings.Join(calls, ",") != "https://cdn/1,https://cdn/2" {
		t.Fatalf("calls=%v", calls)
	}
	matches, _ := filepath.Glob(filepath.Join(sm.SessionDir(), "media", "*--*.txt"))
	if len(matches) != 2 {
		t.Fatalf("saved=%v", matches)
	}
}

func TestDiscordAttachmentFailureDoesNotStartTurn(t *testing.T) {
	api := newFakeDiscordAPI()
	fetch := &fakeDiscordFetcher{err: errors.New("cdn failed")}
	p, ff := testPool(t, makeScriptedHandler(nil, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{AttachmentFetcher: fetch, MediaStore: media.NewStore(t.TempDir())})
	api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Attachments: []DiscordAttachment{{Filename: "x", URL: "https://cdn/secret?signature=hidden"}}})
	waitDiscord(t, func() bool { sends, _, _, _ := api.snapshot(); return len(sends) > 0 })
	if ff.first() != nil {
		t.Fatal("attachment failure started pi")
	}
	sends, _, _, _ := api.snapshot()
	if strings.Contains(sends[0].Content, "signature") {
		t.Fatal("warning leaked URL")
	}
}

func TestDiscordPromptAndDirectSend(t *testing.T) {
	api := newFakeDiscordAPI()
	p, ff := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: "ok"}, {kind: "agent_end"}}, nil))
	bot := newStartedDiscordBot(t, p, api, DiscordConfig{AllowedChannels: []string{"123"}})
	sub, err := bot.Prompt(context.Background(), "123", "from HTTP")
	if err != nil || sub == nil {
		t.Fatalf("Prompt=%v %v", sub, err)
	}
	defer sub.Close()
	fake := ff.waitForFirst(time.Second)
	if fake == nil || !fake.waitForCommand("from HTTP", time.Second) {
		t.Fatal("HTTP prompt missing")
	}
	waitDiscord(t, func() bool { sends, _, _, _ := api.snapshot(); return len(sends) >= 1 })
	if _, err := bot.Prompt(context.Background(), "999", "no"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("disallowed prompt=%v", err)
	}
	file := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(file, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.sends = nil
	api.mu.Unlock()
	result, err := bot.Send(context.Background(), httpapi.SendRequest{Channel: "123", Text: strings.Repeat("a", 2100), MediaPath: file, Caption: strings.Repeat("b", 2100)})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	sends := append([]DiscordSend(nil), api.sends...)
	api.mu.Unlock()
	if len(sends) != 4 || sends[3].FilePath != file || len(result.Sent) != 4 {
		t.Fatalf("sends=%+v result=%+v", sends, result)
	}
	for _, send := range sends {
		mentionsDisabled(t, send.AllowedMentions)
	}
}

func TestDiscordInteractionsDeferDenyStreamAndFallback(t *testing.T) {
	api := newFakeDiscordAPI()
	final := strings.Repeat("x", 2100)
	p, _ := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: final}, {kind: "agent_end", delay: 100 * time.Millisecond}}, nil))
	bot := newStartedDiscordBot(t, p, api, DiscordConfig{AllowedChannels: []string{"123"}, CommandProvider: func(context.Context) ([]rpc.Command, error) {
		return []rpc.Command{{Name: "fix-tests", Source: "prompt"}}, nil
	}})
	api.emitInteraction(DiscordInteraction{ID: "deny", ApplicationID: "888", Token: "t", ChannelID: "999", Name: "session"})
	api.mu.Lock()
	if len(api.responses) != 1 || !api.responses[0].Ephemeral || api.responses[0].Type != DiscordInteractionMessage {
		t.Fatalf("denial=%+v", api.responses)
	}
	api.mu.Unlock()
	api.emitInteraction(DiscordInteraction{ID: "ok", ApplicationID: "888", Token: "t", ChannelID: "123", Name: "fix_tests", Options: map[string]string{"args": "now"}})
	time.Sleep(30 * time.Millisecond)
	api.mu.Lock()
	if len(api.responses) != 2 || api.responses[1].Type != DiscordInteractionDeferred || len(api.interactionEdits) != 0 {
		api.mu.Unlock()
		t.Fatalf("interaction was not deferred and buffered: responses=%+v edits=%v", api.responses, api.interactionEdits)
	}
	api.mu.Unlock()
	waitDiscord(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.interactionEdits) > 0 && len(api.followups) > 0
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.responses[1].Type != DiscordInteractionDeferred || api.sequence[1] != "respond" {
		t.Fatalf("responses=%+v sequence=%v", api.responses, api.sequence)
	}
	combined := api.interactionEdits[len(api.interactionEdits)-1].Content + api.followups[len(api.followups)-1].Content
	if combined != final {
		t.Fatalf("interaction final len=%d", len(combined))
	}
	for _, edit := range api.interactionEdits {
		mentionsDisabled(t, edit.AllowedMentions)
	}
	_ = bot
}

func TestDiscordActivePiInteractionUsesPromptSteerAndAcknowledges(t *testing.T) {
	api := newFakeDiscordAPI()
	release := make(chan struct{})
	var prompts int
	handler := func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		typ, _ := cmd["type"].(string)
		switch typ {
		case "switch_session":
			f.writeResp(id, typ, true)
		case "prompt":
			prompts++
			f.writeResp(id, typ, true)
			if prompts == 1 {
				f.writeJSON(map[string]any{"type": "agent_start"})
				go func() { <-release; f.writeJSON(map[string]any{"type": "agent_end"}) }()
			}
		case "abort", "steer":
			f.writeResp(id, typ, true)
		default:
			f.writeResp(id, typ, true)
		}
	}
	p, ff := testPool(t, handler)
	newStartedDiscordBot(t, p, api, DiscordConfig{CommandProvider: func(context.Context) ([]rpc.Command, error) {
		return []rpc.Command{{Name: "fix-tests", Source: "prompt"}}, nil
	}})
	api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Content: "first"})
	fake := ff.waitForFirst(time.Second)
	if fake == nil || !fake.waitForCommand(`"type":"prompt"`, time.Second) {
		t.Fatal("first prompt missing")
	}
	api.emitInteraction(DiscordInteraction{ID: "i", ApplicationID: "888", Token: "t", ChannelID: "123", Name: "fix_tests", Options: map[string]string{"args": "now"}})
	waitDiscord(t, func() bool { api.mu.Lock(); defer api.mu.Unlock(); return len(api.interactionEdits) > 0 })
	if fake.count(`"type":"prompt"`) != 2 || !fake.contains(`"streamingBehavior":"steer"`) {
		t.Fatalf("commands=%v", fake.Got())
	}
	api.mu.Lock()
	ack := api.interactionEdits[len(api.interactionEdits)-1].Content
	api.mu.Unlock()
	if ack != "Command applied to the active response." {
		t.Fatalf("ack=%q", ack)
	}
	close(release)
}

func TestDiscordOrdinaryMessageDoesNotDeliverBeforeCompletion(t *testing.T) {
	api := newFakeDiscordAPI()
	p, _ := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: "buffered"}, {kind: "agent_end", delay: 150 * time.Millisecond}}, nil))
	newStartedDiscordBot(t, p, api, DiscordConfig{})
	api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Content: "go"})
	time.Sleep(40 * time.Millisecond)
	sends, edits, _, typing := api.snapshot()
	if len(sends) != 0 || len(edits) != 0 {
		t.Fatalf("delivery before completion: sends=%v edits=%v", sends, edits)
	}
	if len(typing) == 0 {
		t.Fatal("typing did not start while reply was buffered")
	}
	waitDiscord(t, func() bool { sends, _, _, _ := api.snapshot(); return len(sends) == 1 })
	_, _, _, typing = api.snapshot()
	count := len(typing)
	time.Sleep(20 * time.Millisecond)
	_, _, _, typing = api.snapshot()
	if len(typing) != count {
		t.Fatalf("typing continued after completion: before=%d after=%d", count, len(typing))
	}
}

func TestDiscordOrdinaryNoReplySuppression(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want int
	}{
		{"exact", "NO_REPLY", 0},
		{"whitespace", " \u2003NO_REPLY\n", 0},
		{"prose", "Do not quote NO_REPLY here.", 1},
		{"lowercase", "no_reply", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeDiscordAPI()
			p, _ := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: tt.text}, {kind: "agent_end"}}, nil))
			bot := newStartedDiscordBot(t, p, api, DiscordConfig{})
			api.emitMessage(DiscordMessage{ChannelID: "123", Author: &DiscordUser{ID: "7"}, Content: "go"})
			waitDiscord(t, func() bool {
				if tt.want > 0 {
					sends, _, _, _ := api.snapshot()
					return len(sends) == tt.want
				}
				return !bot.turns.Active(discordChannelID("123"))
			})
			sends, edits, deletes, _ := api.snapshot()
			if len(sends) != tt.want || len(edits) != 0 || len(deletes) != 0 {
				t.Fatalf("sends=%v edits=%v deletes=%v", sends, edits, deletes)
			}
			if tt.want > 0 && sends[0].Content != tt.text {
				t.Fatalf("content=%q want=%q", sends[0].Content, tt.text)
			}
		})
	}
}

func TestDiscordInteractionNoReplyDeletesDeferredResponse(t *testing.T) {
	for _, deleteErr := range []error{nil, errors.New("delete failed")} {
		name := "success"
		if deleteErr != nil {
			name = "delete_failure"
		}
		t.Run(name, func(t *testing.T) {
			api := newFakeDiscordAPI()
			api.interactionDeleteErr = deleteErr
			p, _ := testPool(t, makeScriptedHandler([]scriptedEvent{{kind: "delta", text: "NO_REPLY"}, {kind: "agent_end"}}, nil))
			newStartedDiscordBot(t, p, api, DiscordConfig{CommandProvider: func(context.Context) ([]rpc.Command, error) {
				return []rpc.Command{{Name: "silent", Source: "prompt"}}, nil
			}})
			api.emitInteraction(DiscordInteraction{ID: "silent", ApplicationID: "888", Token: "token", ChannelID: "123", Name: "silent"})
			waitDiscord(t, func() bool {
				api.mu.Lock()
				defer api.mu.Unlock()
				return len(api.interactionDeletes) == 1
			})
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.responses) != 1 || api.responses[0].Type != DiscordInteractionDeferred {
				t.Fatalf("responses=%+v", api.responses)
			}
			if len(api.interactionEdits) != 0 || len(api.followups) != 0 {
				t.Fatalf("suppression produced visible output: edits=%v followups=%v", api.interactionEdits, api.followups)
			}
		})
	}
}

func TestDiscordCommandRegistrySanitizeCollisionAndCap(t *testing.T) {
	commands := []rpc.Command{{Name: "fix-tests", Source: "prompt"}, {Name: "fix_tests", Source: "extension"}, {Name: strings.Repeat("long-", 20), Description: strings.Repeat("d", 200), Source: "prompt"}}
	for i := 0; i < 120; i++ {
		commands = append(commands, rpc.Command{Name: "cmd-" + itoaSmall(i), Source: "prompt"})
	}
	registry := newDiscordCommandRegistry(commands)
	if command, ok := registry.lookup("fix_tests_2"); !ok || command.PiName != "fix_tests" {
		t.Fatalf("collision=%+v %v", command, ok)
	}
	if len(registry.commands()) != 100 {
		t.Fatalf("catalog=%d", len(registry.commands()))
	}
	for _, command := range registry.commands() {
		if len(command.Name) > 32 || len([]rune(command.Description)) > 100 {
			t.Fatalf("command=%+v", command)
		}
	}
}

var _ httpapi.PromptAdapter = (*DiscordBot)(nil)
var _ httpapi.SendAdapter = (*DiscordBot)(nil)
var _ Frontend = (*DiscordBot)(nil)
