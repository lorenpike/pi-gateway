package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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

const (
	DiscordMaxMessageLen        = 2000
	defaultDiscordReadyTimeout  = 15 * time.Second
	defaultDiscordTyping        = 8 * time.Second
	defaultDiscordAttachmentMax = int64(32 << 20)
)

// DiscordConfig configures the Discord front-end.
type DiscordConfig struct {
	Token           string
	AllowedChannels []string
	Turns           *turn.Manager
	MediaStore      *media.Store
	CommandProvider func(context.Context) ([]rpc.Command, error)
	ReadyTimeout    time.Duration
	// EditInterval is retained for configuration compatibility. Buffered
	// Discord replies no longer perform periodic preview edits.
	EditInterval      time.Duration
	TypingInterval    time.Duration
	IdleTimeout       time.Duration
	AttachmentMax     int64
	AttachmentFetcher DiscordAttachmentFetcher
}

// DiscordAttachmentFetcher deliberately has no token argument. CDN requests
// are independent HTTPS requests and can never inherit the bot Authorization
// header through this seam.
type DiscordAttachmentFetcher interface {
	Fetch(context.Context, string) (io.ReadCloser, error)
}

type httpsDiscordAttachmentFetcher struct {
	client   *http.Client
	maxBytes int64
}

func newHTTPSDiscordAttachmentFetcher(maxBytes int64) DiscordAttachmentFetcher {
	if maxBytes <= 0 {
		maxBytes = defaultDiscordAttachmentMax
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" || request.URL.Host == "" || request.URL.User != nil {
				return errors.New("discord: attachment redirect must be HTTPS")
			}
			if len(via) >= 10 {
				return errors.New("discord: too many attachment redirects")
			}
			return nil
		},
	}
	return &httpsDiscordAttachmentFetcher{client: client, maxBytes: maxBytes}
}

func (f *httpsDiscordAttachmentFetcher) Fetch(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("discord: attachment URL must be HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("discord: invalid attachment request")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, errors.New("discord: attachment download failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("discord: attachment download returned HTTP %d", resp.StatusCode)
	}
	return &discordLimitReadCloser{ReadCloser: resp.Body, remaining: f.maxBytes}, nil
}

type discordLimitReadCloser struct {
	io.ReadCloser
	remaining int64
}

func (r *discordLimitReadCloser) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errors.New("discord: attachment exceeds download limit")
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}

// DiscordBot is a Gateway front-end and HTTP prompt/send adapter.
type DiscordBot struct {
	api   DiscordAPI
	pool  *pool.Pool
	turns *turn.Manager

	allowed         map[string]bool
	store           *media.Store
	fetcher         DiscordAttachmentFetcher
	attachmentMax   int64
	commandProvider func(context.Context) ([]rpc.Command, error)
	commands        *discordCommandRegistry

	readyTimeout   time.Duration
	typingInterval time.Duration
	idleTimeout    time.Duration

	identityMu    sync.RWMutex
	botID         string
	applicationID string
	ready         chan DiscordReady

	lifeMu   sync.Mutex
	stopping bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewDiscord(cfg DiscordConfig, p *pool.Pool, api DiscordAPI) (*DiscordBot, error) {
	if cfg.Token == "" {
		return nil, errors.New("chat: discord token is required")
	}
	if p == nil {
		return nil, errors.New("chat: pool is required")
	}
	for _, channel := range cfg.AllowedChannels {
		if !validDiscordSnowflake(channel) {
			return nil, fmt.Errorf("chat: invalid Discord channel %q", channel)
		}
	}
	if api == nil {
		var err error
		api, err = newDiscordGoAPI(cfg.Token)
		if err != nil {
			return nil, err
		}
	}
	turns := cfg.Turns
	if turns == nil {
		turns = turn.NewManager(context.Background(), p)
	}
	readyTimeout := cfg.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = defaultDiscordReadyTimeout
	}
	typingInterval := cfg.TypingInterval
	if typingInterval <= 0 {
		typingInterval = defaultDiscordTyping
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	attachmentMax := cfg.AttachmentMax
	if attachmentMax <= 0 {
		attachmentMax = defaultDiscordAttachmentMax
	}
	fetcher := cfg.AttachmentFetcher
	if fetcher == nil {
		fetcher = newHTTPSDiscordAttachmentFetcher(attachmentMax)
	}
	allowed := make(map[string]bool, len(cfg.AllowedChannels))
	for _, channel := range cfg.AllowedChannels {
		allowed[channel] = true
	}
	return &DiscordBot{
		api: api, pool: p, turns: turns, allowed: allowed, store: cfg.MediaStore,
		fetcher: fetcher, attachmentMax: attachmentMax, commandProvider: cfg.CommandProvider,
		commands: newDiscordCommandRegistry(nil), readyTimeout: readyTimeout,
		typingInterval: typingInterval, idleTimeout: idleTimeout,
		ready: make(chan DiscordReady, 1),
	}, nil
}

func validDiscordSnowflake(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func discordChannelID(channelID string) pool.ChannelID {
	return pool.ChannelID(session.NewChannelID("discord", channelID))
}

func (b *DiscordBot) Start(ctx context.Context) error {
	b.lifeMu.Lock()
	if b.ctx != nil {
		b.lifeMu.Unlock()
		return errors.New("discord: already started")
	}
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.lifeMu.Unlock()

	b.api.SetHandlers(DiscordHandlers{Ready: b.handleReady, MessageCreate: b.dispatchMessage, InteractionCreate: b.handleInteraction})
	b.api.SetIntents(discordRequiredIntents)
	if err := b.api.Open(b.ctx); err != nil {
		b.cancel()
		_ = b.api.Close()
		return err
	}
	readyCtx, cancel := context.WithTimeout(b.ctx, b.readyTimeout)
	defer cancel()
	var ready DiscordReady
	select {
	case ready = <-b.ready:
	case <-readyCtx.Done():
		_ = b.api.Close()
		b.cancel()
		return fmt.Errorf("discord: waiting for Ready: %w", readyCtx.Err())
	}
	log.Printf("discord: connected as @%s (id=%s)", ready.BotUsername, ready.BotUserID)
	b.initDiscordCommands(ctx, ready.ApplicationID)
	return nil
}

func (b *DiscordBot) handleReady(ready DiscordReady) {
	if ready.BotUserID == "" || ready.ApplicationID == "" {
		return
	}
	b.identityMu.Lock()
	b.botID = ready.BotUserID
	b.applicationID = ready.ApplicationID
	b.identityMu.Unlock()
	select {
	case b.ready <- ready:
	default:
	}
}

func (b *DiscordBot) initDiscordCommands(ctx context.Context, applicationID string) {
	if b.commandProvider != nil {
		commands, err := b.commandProvider(ctx)
		if err != nil {
			log.Printf("discord: command discovery failed: %v", err)
		} else {
			b.commands = newDiscordCommandRegistry(commands)
		}
	}
	if err := b.api.BulkOverwriteGlobalCommands(ctx, applicationID, b.commands.commands()); err != nil {
		log.Printf("discord: global command registration failed: %v (continuing)", err)
		return
	}
	log.Printf("discord: registered %d global commands", len(b.commands.commands()))
}

func (b *DiscordBot) Stop(ctx context.Context) error {
	b.lifeMu.Lock()
	b.stopping = true
	if b.cancel != nil {
		b.cancel()
	}
	b.lifeMu.Unlock()
	closeErr := b.api.Close()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return closeErr
}

func (b *DiscordBot) track(fn func()) bool {
	b.lifeMu.Lock()
	if b.stopping || b.ctx == nil || b.ctx.Err() != nil {
		b.lifeMu.Unlock()
		return false
	}
	b.wg.Add(1)
	b.lifeMu.Unlock()
	go func() {
		defer b.wg.Done()
		fn()
	}()
	return true
}

func (b *DiscordBot) dispatchMessage(message DiscordMessage) {
	b.track(func() { b.processMessage(message) })
}

func (b *DiscordBot) channelAllowed(channelID string) bool {
	return len(b.allowed) == 0 || b.allowed[channelID]
}

func (b *DiscordBot) processMessage(message DiscordMessage) {
	if message.ChannelID == "" || message.Author == nil || message.Author.ID == "" {
		return
	}
	b.identityMu.RLock()
	botID := b.botID
	b.identityMu.RUnlock()
	if message.Author.ID == botID || message.Author.Bot || message.WebhookID != "" {
		return
	}
	if !b.channelAllowed(message.ChannelID) {
		return
	}
	files, err := b.saveDiscordAttachments(b.ctx, message.Attachments)
	if err != nil {
		b.sendChannelText(b.ctx, message.ChannelID, "⚠️ could not save attachment.")
		return
	}
	text := message.Content
	if len(files) > 0 {
		text = media.FormatAttachmentPrompt(text, files)
	}
	if text == "" {
		return
	}
	if _, _, err := b.submitDiscordTurn(b.ctx, message.ChannelID, text, false, false); err != nil {
		b.sendChannelText(b.ctx, message.ChannelID, "⚠️ no agent available: "+err.Error())
	}
}

func (b *DiscordBot) saveDiscordAttachments(ctx context.Context, attachments []DiscordAttachment) ([]media.SavedFile, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if b.store == nil {
		return nil, errors.New("discord: media store is not configured")
	}
	files := make([]media.SavedFile, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Size > b.attachmentMax {
			return nil, errors.New("discord: attachment exceeds download limit")
		}
		body, err := b.fetcher.Fetch(ctx, attachment.URL)
		if err != nil {
			return nil, err
		}
		name := attachment.Filename
		if name == "" {
			name = "attachment"
		}
		saved, saveErr := b.store.Save(ctx, name, body)
		closeErr := body.Close()
		if saveErr != nil {
			return nil, saveErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if attachment.ContentType != "" {
			saved.MimeType = attachment.ContentType
		}
		files = append(files, saved)
	}
	return files, nil
}

func (b *DiscordBot) submitDiscordTurn(ctx context.Context, channelID, text string, promptSteer, externalSubscriber bool) (*turn.Subscription, turn.SubmitResult, error) {
	opts := turn.SubmitOptions{UsePromptSteer: promptSteer, SubscribeOnSteer: externalSubscriber}
	if externalSubscriber {
		opts.ExtraNewSubscribers = 1
	}
	sub, result, err := b.turns.Submit(ctx, discordChannelID(channelID), text, opts)
	if err != nil {
		return nil, result, err
	}
	if result.Started {
		if externalSubscriber {
			if len(result.ExtraSubscriptions) > 0 {
				delivery := result.ExtraSubscriptions[0]
				b.track(func() { b.streamDiscordSubscription(channelID, delivery) })
			}
		} else {
			b.streamDiscordSubscription(channelID, sub)
		}
	}
	return sub, result, nil
}

func (b *DiscordBot) startDiscordTyping(channelID string) func() {
	ctx, cancel := context.WithCancel(b.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.triggerTyping(ctx, channelID)
		ticker := time.NewTicker(b.typingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.triggerTyping(ctx, channelID)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (b *DiscordBot) triggerTyping(ctx context.Context, channelID string) {
	if err := b.api.TriggerTyping(ctx, channelID); err != nil && ctx.Err() == nil {
		log.Printf("discord: typing channel %s: %v", channelID, err)
	}
}

func (b *DiscordBot) streamDiscordSubscription(channelID string, sub *turn.Subscription) {
	if sub == nil {
		return
	}
	defer sub.Close()
	stopTyping := b.startDiscordTyping(channelID)

	reply, err := awaitBufferedReply(b.ctx, sub, b.idleTimeout)
	stopTyping()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errBufferedReplyIdle) {
			log.Printf("discord: turn idle for %s, ending delivery for channel %s", b.idleTimeout, channelID)
		}
		b.sendChannelText(b.ctx, channelID, "⚠️ response ended before completion")
		return
	}
	if reply.Suppressed {
		return
	}
	final := reply.Text
	if final == "" {
		final = "(no response)"
	}
	b.deliverDiscordFinal(channelID, final)
}

func (b *DiscordBot) deliverDiscordFinal(channelID, final string) {
	firstID := ""
	for _, chunk := range splitDiscordText(final) {
		replyTo := ""
		if firstID != "" {
			replyTo = firstID
		}
		message, err := b.api.SendMessage(b.ctx, DiscordSend{ChannelID: channelID, Content: chunk, ReplyTo: replyTo, AllowedMentions: noDiscordMentions()})
		if err != nil {
			log.Printf("discord: send final channel %s: %v", channelID, err)
			continue
		}
		if firstID == "" {
			firstID = message.ID
		}
	}
}

func (b *DiscordBot) sendChannelText(ctx context.Context, channelID, text string) {
	for _, chunk := range splitDiscordText(text) {
		if _, err := b.api.SendMessage(ctx, DiscordSend{ChannelID: channelID, Content: chunk, AllowedMentions: noDiscordMentions()}); err != nil {
			log.Printf("discord: send channel %s: %v", channelID, err)
			return
		}
	}
}

// Prompt implements httpapi.PromptAdapter for channelType "discord".
func (b *DiscordBot) Prompt(ctx context.Context, channel, message string) (*turn.Subscription, error) {
	channel = strings.TrimSpace(channel)
	if !validDiscordSnowflake(channel) {
		return nil, fmt.Errorf("invalid discord channel %q", channel)
	}
	if !b.channelAllowed(channel) {
		return nil, fmt.Errorf("discord channel %s is not allowed", channel)
	}
	sub, _, err := b.submitDiscordTurn(ctx, channel, message, false, true)
	return sub, err
}

// Send implements httpapi.SendAdapter without creating a pi turn.
func (b *DiscordBot) Send(ctx context.Context, req httpapi.SendRequest) (httpapi.SendResult, error) {
	channel := strings.TrimSpace(req.Channel)
	if !validDiscordSnowflake(channel) {
		return httpapi.SendResult{}, fmt.Errorf("invalid discord channel %q", req.Channel)
	}
	if !b.channelAllowed(channel) {
		return httpapi.SendResult{}, fmt.Errorf("discord channel %s is not allowed", channel)
	}
	var result httpapi.SendResult
	for _, chunk := range splitDiscordText(req.Text) {
		if req.Text == "" {
			break
		}
		if _, err := b.api.SendMessage(ctx, DiscordSend{ChannelID: channel, Content: chunk, AllowedMentions: noDiscordMentions()}); err != nil {
			return result, err
		}
		result.Sent = append(result.Sent, httpapi.SentItem{Type: "text", Text: chunk})
	}
	if req.MediaPath != "" {
		captionChunks := splitDiscordText(req.Caption)
		if req.Caption == "" {
			captionChunks = []string{""}
		}
		for i, chunk := range captionChunks {
			last := i == len(captionChunks)-1
			send := DiscordSend{ChannelID: channel, Content: chunk, AllowedMentions: noDiscordMentions()}
			if last {
				send.FilePath = req.MediaPath
			}
			if _, err := b.api.SendMessage(ctx, send); err != nil {
				return result, err
			}
			if last {
				result.Sent = append(result.Sent, httpapi.SentItem{Type: "media", Path: req.MediaPath})
			} else {
				result.Sent = append(result.Sent, httpapi.SentItem{Type: "text", Text: chunk})
			}
		}
	}
	return result, nil
}

func (b *DiscordBot) handleInteraction(interaction DiscordInteraction) {
	if interaction.ChannelID == "" || !validDiscordSnowflake(interaction.ChannelID) || !b.channelAllowed(interaction.ChannelID) {
		_ = b.api.RespondInteraction(b.context(), interaction, DiscordInteractionResponse{Type: DiscordInteractionMessage, Content: "This channel is not allowed to use wall-e.", Ephemeral: true, AllowedMentions: noDiscordMentions()})
		return
	}
	// Initial acknowledgement is intentionally synchronous and precedes all
	// command lookup, pool work, and RPC calls.
	if err := b.api.RespondInteraction(b.context(), interaction, DiscordInteractionResponse{Type: DiscordInteractionDeferred, AllowedMentions: noDiscordMentions()}); err != nil {
		return
	}
	b.track(func() { b.processInteraction(interaction) })
}

func (b *DiscordBot) context() context.Context {
	b.lifeMu.Lock()
	defer b.lifeMu.Unlock()
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}

func (b *DiscordBot) processInteraction(interaction DiscordInteraction) {
	command, ok := b.commands.lookup(interaction.Name)
	if !ok {
		b.editInteractionText(interaction, "⚠️ unknown command")
		return
	}
	args := strings.TrimSpace(interaction.Options["args"])
	if command.Source == "gateway" {
		if command.Alias == "skill" {
			name := strings.TrimSpace(interaction.Options["name"])
			if name == "" {
				b.editInteractionText(interaction, b.commands.skillListText())
				return
			}
			skill, ok := b.commands.lookupSkill(name)
			if !ok {
				b.editInteractionText(interaction, "⚠️ unknown skill: "+name+"\n\n"+b.commands.skillListText())
				return
			}
			text := "/" + skill.PiName
			if args != "" {
				text += " " + args
			}
			b.runInteractionPiCommand(interaction, text)
			return
		}
		if command.Alias != "abort" && b.turns.Active(discordChannelID(interaction.ChannelID)) {
			b.editInteractionText(interaction, "⚠️ /"+command.Alias+" is unavailable while pi is responding. Try again after this turn finishes.")
			return
		}
		switch command.Alias {
		case "name":
			args = interaction.Options["value"]
		case "compact":
			args = interaction.Options["instructions"]
		}
		text, err := executeGatewayCommand(b.ctx, b.pool, b.turns, discordChannelID(interaction.ChannelID), command.Alias, args)
		if err != nil {
			text = "⚠️ " + err.Error()
		}
		b.editInteractionText(interaction, text)
		return
	}
	text := "/" + command.PiName
	if args != "" {
		text += " " + args
	}
	b.runInteractionPiCommand(interaction, text)
}

func (b *DiscordBot) runInteractionPiCommand(interaction DiscordInteraction, text string) {
	wasActive := b.turns.Active(discordChannelID(interaction.ChannelID))
	sub, result, err := b.turns.Submit(b.ctx, discordChannelID(interaction.ChannelID), text, turn.SubmitOptions{UsePromptSteer: true})
	if err != nil {
		b.editInteractionText(interaction, "⚠️ no agent available: "+err.Error())
		return
	}
	if wasActive || result.Steered {
		b.editInteractionText(interaction, "Command applied to the active response.")
		return
	}
	b.streamInteraction(interaction, sub)
}

func (b *DiscordBot) editInteractionText(interaction DiscordInteraction, text string) {
	chunks := splitDiscordText(text)
	if text == "" {
		chunks = []string{"(no response)"}
	}
	edit := DiscordEdit{ChannelID: interaction.ChannelID, Content: chunks[0], AllowedMentions: noDiscordMentions()}
	if err := b.api.EditInteractionResponse(b.ctx, interaction, edit); err != nil {
		b.sendChannelText(b.ctx, interaction.ChannelID, text)
		return
	}
	for _, chunk := range chunks[1:] {
		if _, err := b.api.CreateInteractionFollowup(b.ctx, interaction, DiscordSend{ChannelID: interaction.ChannelID, Content: chunk, AllowedMentions: noDiscordMentions()}); err != nil {
			b.sendChannelText(b.ctx, interaction.ChannelID, chunk)
		}
	}
}

func (b *DiscordBot) streamInteraction(interaction DiscordInteraction, sub *turn.Subscription) {
	if sub == nil {
		b.editInteractionText(interaction, "⚠️ response stream unavailable")
		return
	}
	defer sub.Close()

	reply, err := awaitBufferedReply(b.ctx, sub, b.idleTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errBufferedReplyIdle) {
			log.Printf("discord: interaction turn idle for %s in channel %s", b.idleTimeout, interaction.ChannelID)
		}
		b.editInteractionText(interaction, "⚠️ response ended before completion")
		return
	}
	if reply.Suppressed {
		if err := b.api.DeleteInteractionResponse(b.ctx, interaction); err != nil {
			log.Printf("discord: delete suppressed interaction response %s: %v", interaction.ID, err)
		}
		return
	}
	b.editInteractionText(interaction, reply.Text)
}

// splitDiscordText counts UTF-16 code units conservatively, so non-BMP runes
// count as two toward Discord's 2,000-character limit. It preserves every rune
// and whitespace byte exactly; concatenating chunks reproduces text.
func splitDiscordText(text string) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		units := 0
		end := 0
		lastBoundary := -1
		for end < len(runes) {
			cost := 1
			if runes[end] > 0xffff {
				cost = 2
			}
			if units+cost > DiscordMaxMessageLen {
				break
			}
			units += cost
			end++
			if end > 0 && (runes[end-1] == '\n' || runes[end-1] == ' ' || runes[end-1] == '\t') && units >= DiscordMaxMessageLen*3/4 {
				lastBoundary = end
			}
		}
		if end < len(runes) && lastBoundary > 0 {
			end = lastBoundary
		}
		if end == 0 {
			end = 1
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}
