// Package chat implements the wall-e chat front-ends (Telegram, and later
// Discord). Each front-end reads messages from a chat platform, routes every
// chat to the shared worker pool (one live `pi --mode rpc` process per chat,
// bound via pool.Acquire/Release), and delivers the assistant reply using the
// platform's preferred response behavior.
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
// The bot owns NO per-channel serialization: that lives in the pool. Active
// turn state is shared with HTTP/CLI injection through package turn, so a cron
// prompt and a human Telegram message steer the same in-flight turn.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wall-e/httpapi"
	"wall-e/media"
	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
	"wall-e/turn"
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
	// SendChatAction shows Telegram's transient "bot is typing…" indicator.
	SendChatAction(ctx context.Context, chatID int64, action string) error
	// SetMyCommands registers the bot's Telegram slash-command menu. Failures
	// are non-fatal to the bot; Start logs and continues.
	SetMyCommands(ctx context.Context, commands []BotCommand) error
	// GetFile resolves a Telegram file_id to a downloadable file_path.
	GetFile(ctx context.Context, fileID string) (File, error)
	// DownloadFile downloads a Telegram file_path returned by GetFile.
	DownloadFile(ctx context.Context, filePath string) (io.ReadCloser, error)
	// SendPhoto sends an image file by path with an optional caption.
	SendPhoto(ctx context.Context, chatID int64, path string, caption string) (Message, error)
	// SendDocument sends a general file by path with an optional caption.
	SendDocument(ctx context.Context, chatID int64, path string, caption string) (Message, error)
}

// BotCommand is a Telegram bot-command menu entry.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
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

// File is Telegram file metadata returned by getFile.
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

// PhotoSize is one Telegram photo rendition.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Voice struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Video struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Message is a Telegram message.
type Message struct {
	MessageID    int64       `json:"message_id"`
	Chat         Chat        `json:"chat"`
	From         User        `json:"from"`
	Text         string      `json:"text"`
	Caption      string      `json:"caption,omitempty"`
	Photo        []PhotoSize `json:"photo,omitempty"`
	Document     *Document   `json:"document,omitempty"`
	Voice        *Voice      `json:"voice,omitempty"`
	Audio        *Audio      `json:"audio,omitempty"`
	Video        *Video      `json:"video,omitempty"`
	MediaGroupID string      `json:"media_group_id,omitempty"`
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

const (
	telegramTypingAction   = "typing"
	telegramTypingInterval = 4 * time.Second
)

// Config configures the Telegram front-end.
type Config struct {
	// Token is the Telegram bot token (from @BotFather). If empty, the
	// gateway skips the Telegram front-end entirely (HTTP still serves).
	Token string
	// AllowedChats is an optional allowlist of chat ids. If non-empty, only
	// messages from these chats are processed; others are ignored. Mirrors the
	// Discord plan's WALLE_DISCORD_ALLOWED_CHANNELS.
	AllowedChats []int64
	// DisableCommandRegistration controls whether Start skips Telegram
	// setMyCommands. Command parsing still uses CommandProvider when disabled.
	// The gateway leaves this false; the option is retained for direct users
	// and focused tests of the Telegram adapter.
	DisableCommandRegistration bool
	// CommandProvider discovers pi RPC commands (extensions, prompt templates,
	// and skills) for Telegram aliases. It may be nil.
	CommandProvider func(context.Context) ([]rpc.Command, error)
	// Turns coordinates active turns shared with HTTP/CLI injection. If nil,
	// NewTelegram creates a private manager over Pool (tests/back-compat); main
	// passes the gateway-wide manager so CLI and Telegram steer each other.
	Turns *turn.Manager
	// MediaStore saves inbound Telegram files before the formatted prompt is
	// submitted. If nil, text-only behavior remains available.
	MediaStore *media.Store
}

// Bot is the Telegram front-end. It is a Frontend: Start launches the
// getUpdates long-poll loop; Stop cancels it and drains in-flight turns.
type Bot struct {
	api     TelegramAPI
	pool    *pool.Pool
	turns   *turn.Manager
	allowed map[int64]bool

	// idleTimeout bounds how long a turn may go without any event before the
	// bot stops typing and reports that the response did not complete. Guards
	// against a stuck pi process (the pool's Slot.Events() is never closed — see
	// Phase 5 log — so without a watchdog a dead process would hang forever).
	// 0 = disabled (tests).
	idleTimeout time.Duration

	botID   int64
	botName string

	registerCommands bool
	commandProvider  func(context.Context) ([]rpc.Command, error)
	commands         *telegramCommandRegistry
	mediaStore       *media.Store

	mediaMu            sync.Mutex
	mediaGroups        map[string]*pendingMediaGroup
	mediaGroupDebounce time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	pollDone chan struct{}

	// turnsWG tracks in-flight turn goroutines so Stop can drain them.
	turnsWG sync.WaitGroup
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
	turns := cfg.Turns
	if turns == nil {
		turns = turn.NewManager(context.Background(), p)
	}
	return &Bot{
		api:                api,
		pool:               p,
		turns:              turns,
		allowed:            allowed,
		idleTimeout:        5 * time.Minute,
		registerCommands:   !cfg.DisableCommandRegistration,
		commandProvider:    cfg.CommandProvider,
		commands:           newTelegramCommandRegistry(nil),
		mediaStore:         cfg.MediaStore,
		mediaGroups:        make(map[string]*pendingMediaGroup),
		mediaGroupDebounce: 500 * time.Millisecond,
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
	b.botName = me.UserName
	log.Printf("telegram: connected as @%s (id=%d)", me.UserName, me.ID)

	b.initCommands(ctx)

	b.ctx, b.cancel = context.WithCancel(ctx)
	b.pollDone = make(chan struct{})
	go b.pollLoop()
	return nil
}

func (b *Bot) initCommands(ctx context.Context) {
	if b.commandProvider != nil {
		commands, err := b.commandProvider(ctx)
		if err != nil {
			log.Printf("telegram: command discovery failed: %v", err)
		} else {
			b.commands = newTelegramCommandRegistry(commands)
		}
	}
	botCommands := b.commands.botCommands()
	if !b.registerCommands {
		log.Printf("telegram: command registration disabled (%d aliases available)", len(b.commands.all))
		return
	}
	if err := b.api.SetMyCommands(ctx, botCommands); err != nil {
		log.Printf("telegram: setMyCommands failed: %v (continuing)", err)
		return
	}
	log.Printf("telegram: registered %d command aliases (%d total aliases)", len(botCommands), len(b.commands.all))
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

// handleMessage routes one incoming message: ignore self / unsupported /
// disallowed chats; if a turn is already active for the chat, steer it; else
// acquire a slot and run a fresh turn. Text/caption plus media from one user
// action is formatted into one initial prompt after files are saved.
func (b *Bot) handleMessage(msg Message) {
	// Ignore our own messages (avoid loops).
	if msg.From.ID == b.botID {
		return
	}
	chatID := msg.Chat.ID
	if len(b.allowed) > 0 && !b.allowed[chatID] {
		return
	}
	if msg.MediaGroupID != "" && messageHasMedia(msg) {
		b.enqueueMediaGroup(msg)
		return
	}
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	files, err := b.saveTelegramMedia(b.ctx, []Message{msg})
	if err != nil {
		_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ could not save attachment: "+err.Error(), 0)
		return
	}
	if text == "" && len(files) == 0 {
		return
	}
	hasFiles := len(files) > 0
	if hasFiles {
		text = media.FormatAttachmentPrompt(text, files)
	}
	cmdName, cmdArgs, isSlash, addressedToOtherBot := parseTelegramCommandText(text, b.botName)
	if addressedToOtherBot {
		return
	}
	if isSlash && !hasFiles {
		if handled, rewritten := b.handleSlashCommand(chatID, cmdName, cmdArgs); handled {
			return
		} else if rewritten != "" {
			text = rewritten
		} else {
			text, _, _ = rewriteTelegramCommandText(text, b.botName, b.commands)
		}
	}

	if _, _, err := b.submitTelegramTurn(chatID, text, isSlash, false); err != nil {
		_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ no agent available: "+err.Error(), 0)
	}
}

type pendingMediaGroup struct {
	chatID int64
	msgs   []Message
}

func (b *Bot) enqueueMediaGroup(msg Message) {
	key := fmt.Sprintf("%d:%s", msg.Chat.ID, msg.MediaGroupID)
	b.mediaMu.Lock()
	pg := b.mediaGroups[key]
	if pg == nil {
		pg = &pendingMediaGroup{chatID: msg.Chat.ID}
		b.mediaGroups[key] = pg
		b.turnsWG.Add(1)
		time.AfterFunc(b.mediaGroupDebounce, func() {
			defer b.turnsWG.Done()
			b.flushMediaGroup(key)
		})
	}
	pg.msgs = append(pg.msgs, msg)
	b.mediaMu.Unlock()
}

func (b *Bot) flushMediaGroup(key string) {
	b.mediaMu.Lock()
	pg := b.mediaGroups[key]
	delete(b.mediaGroups, key)
	b.mediaMu.Unlock()
	if pg == nil || len(pg.msgs) == 0 {
		return
	}
	text := ""
	for _, msg := range pg.msgs {
		if msg.Caption != "" {
			text = msg.Caption
			break
		}
		if msg.Text != "" {
			text = msg.Text
			break
		}
	}
	files, err := b.saveTelegramMedia(b.ctx, pg.msgs)
	if err != nil {
		_, _ = b.api.SendMessage(b.ctx, pg.chatID, "⚠️ could not save attachment: "+err.Error(), 0)
		return
	}
	if len(files) == 0 && text == "" {
		return
	}
	if len(files) > 0 {
		text = media.FormatAttachmentPrompt(text, files)
	}
	if _, _, err := b.submitTelegramTurn(pg.chatID, text, false, false); err != nil {
		_, _ = b.api.SendMessage(b.ctx, pg.chatID, "⚠️ no agent available: "+err.Error(), 0)
	}
}

func messageHasMedia(msg Message) bool {
	return len(msg.Photo) > 0 || msg.Document != nil || msg.Voice != nil || msg.Audio != nil || msg.Video != nil
}

type telegramMediaRef struct {
	fileID   string
	name     string
	mimeType string
}

func telegramMediaRefs(msg Message) []telegramMediaRef {
	var out []telegramMediaRef
	if len(msg.Photo) > 0 {
		best := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			if p.FileSize > best.FileSize || (p.FileSize == best.FileSize && p.Width*p.Height > best.Width*best.Height) {
				best = p
			}
		}
		out = append(out, telegramMediaRef{fileID: best.FileID, name: "photo.jpg"})
	}
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "document"
		}
		out = append(out, telegramMediaRef{fileID: msg.Document.FileID, name: name, mimeType: msg.Document.MimeType})
	}
	if msg.Voice != nil {
		out = append(out, telegramMediaRef{fileID: msg.Voice.FileID, name: "voice.ogg", mimeType: msg.Voice.MimeType})
	}
	if msg.Audio != nil {
		name := msg.Audio.FileName
		if name == "" {
			name = "audio"
		}
		out = append(out, telegramMediaRef{fileID: msg.Audio.FileID, name: name, mimeType: msg.Audio.MimeType})
	}
	if msg.Video != nil {
		name := msg.Video.FileName
		if name == "" {
			name = "video.mp4"
		}
		out = append(out, telegramMediaRef{fileID: msg.Video.FileID, name: name, mimeType: msg.Video.MimeType})
	}
	return out
}

func (b *Bot) saveTelegramMedia(ctx context.Context, msgs []Message) ([]media.SavedFile, error) {
	var refs []telegramMediaRef
	for _, msg := range msgs {
		refs = append(refs, telegramMediaRefs(msg)...)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if b.mediaStore == nil {
		return nil, errors.New("media store is not configured")
	}
	files := make([]media.SavedFile, 0, len(refs))
	for _, ref := range refs {
		if ref.fileID == "" {
			continue
		}
		info, err := b.api.GetFile(ctx, ref.fileID)
		if err != nil {
			return nil, err
		}
		name := ref.name
		if name == "" || name == "document" || name == "audio" || name == "video.mp4" {
			if info.FilePath != "" {
				if base := filepath.Base(info.FilePath); base != "." && base != string(filepath.Separator) && base != "" {
					name = base
				}
			}
		}
		rc, err := b.api.DownloadFile(ctx, info.FilePath)
		if err != nil {
			return nil, err
		}
		saved, saveErr := b.mediaStore.Save(ctx, name, rc)
		closeErr := rc.Close()
		if saveErr != nil {
			return nil, saveErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if ref.mimeType != "" {
			saved.MimeType = ref.mimeType
		}
		files = append(files, saved)
	}
	return files, nil
}

func isImageFile(path string) bool {
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		if strings.HasPrefix(http.DetectContentType(buf[:n]), "image/") {
			return true
		}
	}
	if mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); strings.HasPrefix(mt, "image/") {
		return true
	}
	return false
}

func telegramChannelID(chatID int64) pool.ChannelID {
	return pool.ChannelID(session.NewChannelID("telegram", strconv.FormatInt(chatID, 10)))
}

func (b *Bot) submitTelegramTurn(chatID int64, text string, usePromptSteer bool, subscribeOnSteer bool) (*turn.Subscription, turn.SubmitResult, error) {
	chID := telegramChannelID(chatID)
	opts := turn.SubmitOptions{UsePromptSteer: usePromptSteer, SubscribeOnSteer: subscribeOnSteer}
	if subscribeOnSteer {
		opts.ExtraNewSubscribers = 1
	}
	sub, res, err := b.turns.Submit(b.ctx, chID, text, opts)
	if err != nil {
		log.Printf("telegram: prompt/steer chat %d: %v", chatID, err)
		return nil, res, err
	}
	if res.Started {
		if subscribeOnSteer {
			// Caller needs its own stream (HTTP/CLI). Use an extra subscription
			// that was attached before the prompt was sent for Telegram delivery.
			if len(res.ExtraSubscriptions) > 0 {
				tgSub := res.ExtraSubscriptions[0]
				b.turnsWG.Add(1)
				go func() {
					defer b.turnsWG.Done()
					stopTyping := b.startTypingIndicator(chatID)
					b.streamSubscription(chatID, tgSub, stopTyping)
				}()
			}
		} else {
			stopTyping := b.startTypingIndicator(chatID)
			b.streamSubscription(chatID, sub, stopTyping)
		}
	}
	return sub, res, nil
}

// Prompt implements httpapi.PromptAdapter for channelType "telegram". It starts
// or steers the target Telegram chat and returns an SSE subscription for the
// HTTP/CLI caller. On a newly-started turn, it also starts Telegram delivery for
// the assistant response; the injected user prompt is not mirrored to Telegram.
func (b *Bot) Prompt(ctx context.Context, channel string, message string) (*turn.Subscription, error) {
	chatID, err := strconv.ParseInt(strings.TrimSpace(channel), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid telegram channel %q", channel)
	}
	if len(b.allowed) > 0 && !b.allowed[chatID] {
		return nil, fmt.Errorf("telegram chat %d is not allowed", chatID)
	}
	if b.ctx == nil {
		b.ctx = ctx
	}
	sub, _, err := b.submitTelegramTurn(chatID, message, false, true)
	return sub, err
}

// Send implements httpapi.SendAdapter for direct Telegram delivery. It does not
// create an agent turn.
func (b *Bot) Send(ctx context.Context, req httpapi.SendRequest) (httpapi.SendResult, error) {
	chatID, err := strconv.ParseInt(strings.TrimSpace(req.Channel), 10, 64)
	if err != nil {
		return httpapi.SendResult{}, fmt.Errorf("invalid telegram channel %q", req.Channel)
	}
	if len(b.allowed) > 0 && !b.allowed[chatID] {
		return httpapi.SendResult{}, fmt.Errorf("telegram chat %d is not allowed", chatID)
	}
	var out httpapi.SendResult
	if req.Text != "" {
		if _, err := b.api.SendMessage(ctx, chatID, req.Text, 0); err != nil {
			return out, err
		}
		out.Sent = append(out.Sent, httpapi.SentItem{Type: "text", Text: req.Text})
	}
	if req.MediaPath != "" {
		caption := req.Caption
		if len([]rune(caption)) > 1024 {
			if _, err := b.api.SendMessage(ctx, chatID, caption, 0); err != nil {
				return out, err
			}
			out.Sent = append(out.Sent, httpapi.SentItem{Type: "text", Text: caption})
			caption = ""
		}
		var photoErr error
		if isImageFile(req.MediaPath) {
			if _, err := b.api.SendPhoto(ctx, chatID, req.MediaPath, caption); err == nil {
				out.Sent = append(out.Sent, httpapi.SentItem{Type: "media", Path: req.MediaPath})
				return out, nil
			} else if !isTelegramPhotoRejection(err) {
				// A timeout or transport error has an unknown delivery outcome.
				// Retrying as a document may duplicate a photo Telegram accepted.
				return out, err
			} else {
				photoErr = err
			}
		}
		if _, err := b.api.SendDocument(ctx, chatID, req.MediaPath, caption); err != nil {
			if photoErr != nil {
				return out, fmt.Errorf("%v; sendDocument fallback failed: %w", photoErr, err)
			}
			return out, err
		}
		out.Sent = append(out.Sent, httpapi.SentItem{Type: "media", Path: req.MediaPath})
	}
	return out, nil
}

func (b *Bot) handleSlashCommand(chatID int64, cmdName, args string) (handled bool, rewritten string) {
	tc, ok := b.commands.lookup(cmdName)
	if !ok || tc.Source != "gateway" {
		return false, ""
	}
	args = strings.TrimSpace(args)
	if cmdName == "abort" {
		b.handleAbortCommand(chatID)
		return true, ""
	}
	if cmdName == "skill" {
		if args == "" {
			_, _ = b.api.SendMessage(b.ctx, chatID, b.commands.skillListText(), 0)
			return true, ""
		}
		name, rest, _ := strings.Cut(args, " ")
		skill, ok := b.commands.lookupSkill(name)
		if !ok {
			_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ unknown skill: "+name+"\n\n"+b.commands.skillListText(), 0)
			return true, ""
		}
		if strings.TrimSpace(rest) == "" {
			return false, "/" + skill.PiName
		}
		return false, "/" + skill.PiName + " " + strings.TrimSpace(rest)
	}

	if b.chatHasActiveTurn(chatID) {
		_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ /"+cmdName+" is unavailable while pi is responding. Try again after this turn finishes.", 0)
		return true, ""
	}
	b.handleGatewayCommand(chatID, cmdName, args)
	return true, ""
}

func (b *Bot) chatHasActiveTurn(chatID int64) bool {
	return b.turns.Active(telegramChannelID(chatID))
}

func (b *Bot) handleAbortCommand(chatID int64) {
	resp, err := b.turns.Abort(b.ctx, telegramChannelID(chatID))
	if errors.Is(err, turn.ErrNoActiveTurn) {
		_, _ = b.api.SendMessage(b.ctx, chatID, "No active pi turn to abort.", 0)
		return
	}
	if err != nil || !resp.Success {
		b.sendCommandError(chatID, "abort", resp, err)
		return
	}
	_, _ = b.api.SendMessage(b.ctx, chatID, "Aborted current pi turn.", 0)
}

func (b *Bot) handleGatewayCommand(chatID int64, cmdName, args string) {
	chID := telegramChannelID(chatID)
	slot, err := b.pool.Acquire(b.ctx, chID)
	if err != nil {
		log.Printf("telegram: /%s acquire chat %d: %v", cmdName, chatID, err)
		_, _ = b.api.SendMessage(b.ctx, chatID, "⚠️ no agent available: "+err.Error(), 0)
		return
	}
	defer b.pool.Release(chID)

	client := slot.Client()
	switch cmdName {
	case "name":
		resp, err := client.SetSessionName(b.ctx, strings.TrimSpace(args))
		if err != nil || !resp.Success {
			b.sendCommandError(chatID, "name", resp, err)
			return
		}
		if strings.TrimSpace(args) == "" {
			_, _ = b.api.SendMessage(b.ctx, chatID, "Session name cleared.", 0)
		} else {
			_, _ = b.api.SendMessage(b.ctx, chatID, "Session name set to: "+strings.TrimSpace(args), 0)
		}
	case "session":
		st, err := client.GetState(b.ctx)
		if err != nil {
			b.sendCommandError(chatID, "session", rpc.Response{}, err)
			return
		}
		text := fmt.Sprintf("Session\nID: %s\nName: %s\nMessages: %d\nStreaming: %v", emptyDash(st.SessionID), emptyDash(st.SessionName), st.MessageCount, st.IsStreaming)
		_, _ = b.api.SendMessage(b.ctx, chatID, text, 0)
	case "new":
		// Do not use pi's new_session directly here: it generates pi-default
		// filenames that /v1/sessions intentionally ignores. Switching to a
		// fresh wall-e path creates the same fresh context while preserving the
		// typed filename scheme used by the session viewer.
		newPath := b.pool.NewSessionPath(chID)
		resp, _, err := client.SwitchSession(b.ctx, newPath)
		if err != nil || !resp.Success {
			b.sendCommandError(chatID, "new", resp, err)
			return
		}
		if err := b.pool.ResyncFromState(chID, newPath); err != nil {
			b.sendCommandError(chatID, "new", rpc.Response{}, err)
			return
		}
		_, _ = b.api.SendMessage(b.ctx, chatID, "Started a new pi session.", 0)
	case "clone":
		resp, st, err := client.Clone(b.ctx)
		if err != nil || !resp.Success {
			b.sendCommandError(chatID, "clone", resp, err)
			return
		}
		clonePath := b.pool.NewSessionPath(chID)
		if st.SessionFile == "" {
			b.sendCommandError(chatID, "clone", rpc.Response{}, errors.New("clone returned empty session file"))
			return
		}
		if err := b.pool.CopySessionFile(st.SessionFile, clonePath); err != nil {
			b.sendCommandError(chatID, "clone", rpc.Response{}, err)
			return
		}
		resp, _, err = client.SwitchSession(b.ctx, clonePath)
		if err != nil || !resp.Success {
			b.sendCommandError(chatID, "clone", resp, err)
			return
		}
		if err := b.pool.ResyncFromState(chID, clonePath); err != nil {
			b.sendCommandError(chatID, "clone", rpc.Response{}, err)
			return
		}
		if st.SessionFile != clonePath {
			_ = b.pool.RemoveSessionFile(st.SessionFile)
		}
		_, _ = b.api.SendMessage(b.ctx, chatID, "Cloned this pi session branch.", 0)
	case "compact":
		resp, err := client.Compact(b.ctx, strings.TrimSpace(args))
		if err != nil || !resp.Success {
			b.sendCommandError(chatID, "compact", resp, err)
			return
		}
		_, _ = b.api.SendMessage(b.ctx, chatID, "Compacted this pi session.", 0)
	}
}

func (b *Bot) sendCommandError(chatID int64, name string, resp rpc.Response, err error) {
	msg := "⚠️ /" + name + " failed"
	if err != nil {
		msg += ": " + err.Error()
	} else if resp.Error != "" {
		msg += ": " + resp.Error
	}
	_, _ = b.api.SendMessage(b.ctx, chatID, msg, 0)
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (b *Bot) startTypingIndicator(chatID int64) func() {
	if b.ctx == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(b.ctx)
	go func() {
		b.sendTypingAction(ctx, chatID)

		ticker := time.NewTicker(telegramTypingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.sendTypingAction(ctx, chatID)
			}
		}
	}()
	return cancel
}

func (b *Bot) sendTypingAction(ctx context.Context, chatID int64) {
	if err := b.api.SendChatAction(ctx, chatID, telegramTypingAction); err != nil && ctx.Err() == nil {
		log.Printf("telegram: sendChatAction chat %d: %v", chatID, err)
	}
}

// streamSubscription consumes a turn subscription and accumulates text deltas
// while Telegram's typing indicator remains active. It sends no partial message;
// on agent_end it sends the complete response (split only when Telegram's
// 4096-character limit requires it).
func (b *Bot) streamSubscription(chatID int64, sub *turn.Subscription, stopTyping func()) {
	if sub == nil {
		return
	}
	defer sub.Close()
	var buf strings.Builder
	turnDone := false
	if stopTyping == nil {
		stopTyping = func() {}
	}

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

	finalize := func() {
		stopTyping()
		if sub.FinalText != nil {
			final, ok := <-sub.FinalText
			if !ok {
				b.sendIncompleteTurnMessage(chatID)
				return
			}
			buf.Reset()
			buf.WriteString(final)
		}
		b.finalizeMessage(chatID, buf.String())
	}

	for {
		select {
		case <-b.ctx.Done():
			stopTyping()
			return
		case ev, ok := <-sub.Events:
			if !ok {
				// A completed turn has an authoritative final value; a process
				// failure closes FinalText without one.
				finalize()
				return
			}
			resetIdle()
			switch ev.Type {
			case rpc.EventAgentStart:
				// nothing
			case rpc.EventMessageUpdate:
				if d, ok := decodeTextDelta(ev.Raw); ok && d != "" {
					buf.WriteString(d)
				}
			case rpc.EventAgentEnd:
				turnDone = true
			}
		case <-idleC:
			log.Printf("telegram: turn idle for %s, ending delivery for chat %d", b.idleTimeout, chatID)
			stopTyping()
			b.sendIncompleteTurnMessage(chatID)
			return
		}
		if turnDone {
			finalize()
			return
		}
	}
}

func (b *Bot) sendIncompleteTurnMessage(chatID int64) {
	if _, err := b.api.SendMessage(b.ctx, chatID, "⚠️ response ended before completion", 0); err != nil {
		log.Printf("telegram: send incomplete-turn notice chat %d: %v", chatID, err)
	}
}

// finalizeMessage sends the complete assistant text after streaming ends.
// Chunking is rune-safe: we split on rune boundaries at TelegramMaxMessageLen
// so multi-byte text never produces an invalid UTF-8 boundary. Telegram's
// limit is 4096 "characters" (UTF-16 code units for the API); for BMP text
// runes == chars, which is the common case. Non-BMP (emoji) may occasionally
// allow one fewer chunk than the limit permits — acceptable for v1.
func (b *Bot) finalizeMessage(chatID int64, final string) {
	if final == "" {
		return
	}
	b.sendChunks(chatID, []rune(final))
}

// sendChunks sends runes as a sequence of <=TelegramMaxMessageLen-character
// messages. Later chunks reply to the first chunk.
func (b *Bot) sendChunks(chatID int64, runes []rune) {
	var replyTo int64
	for len(runes) > 0 {
		n := len(runes)
		if n > TelegramMaxMessageLen {
			n = TelegramMaxMessageLen
		}
		m, err := b.api.SendMessage(b.ctx, chatID, string(runes[:n]), replyTo)
		if err != nil {
			log.Printf("telegram: send final message chat %d: %v", chatID, err)
			return
		}
		if replyTo == 0 {
			replyTo = m.MessageID
		}
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
