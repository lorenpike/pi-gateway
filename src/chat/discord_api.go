package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// DiscordIntent is wall-e's transport-neutral Gateway intent mask.
type DiscordIntent uint64

const (
	DiscordIntentGuilds DiscordIntent = 1 << iota
	DiscordIntentGuildMessages
	DiscordIntentDirectMessages
	DiscordIntentMessageContent
)

const discordRequiredIntents = DiscordIntentGuilds | DiscordIntentGuildMessages | DiscordIntentDirectMessages | DiscordIntentMessageContent

// DiscordHandlers are installed before the Gateway is opened.
type DiscordHandlers struct {
	Ready             func(DiscordReady)
	MessageCreate     func(DiscordMessage)
	InteractionCreate func(DiscordInteraction)
}

type DiscordReady struct {
	BotUserID     string
	BotUsername   string
	ApplicationID string
}

type DiscordUser struct {
	ID       string
	Username string
	Bot      bool
}

type DiscordAttachment struct {
	Filename    string
	ContentType string
	Size        int64
	URL         string
}

type DiscordMessage struct {
	ID          string
	ChannelID   string
	GuildID     string
	Author      *DiscordUser
	Content     string
	WebhookID   string
	Attachments []DiscordAttachment
}

// DiscordAllowedMentions is explicit in wall-e-owned payloads so tests can
// verify that no model or command output can ping Discord users or roles.
type DiscordAllowedMentions struct {
	Parse       []string
	Users       []string
	Roles       []string
	RepliedUser bool
}

func noDiscordMentions() DiscordAllowedMentions {
	return DiscordAllowedMentions{Parse: []string{}, Users: []string{}, Roles: []string{}, RepliedUser: false}
}

type DiscordSend struct {
	ChannelID       string
	Content         string
	ReplyTo         string
	FilePath        string
	AllowedMentions DiscordAllowedMentions
}

type DiscordEdit struct {
	ChannelID       string
	MessageID       string
	Content         string
	AllowedMentions DiscordAllowedMentions
}

type DiscordCommandOption struct {
	Name        string
	Description string
	Required    bool
	MaxLength   int
}

type DiscordCommand struct {
	Name        string
	Description string
	Options     []DiscordCommandOption
}

type DiscordInteraction struct {
	ID            string
	ApplicationID string
	Token         string
	ChannelID     string
	Name          string
	Options       map[string]string
}

type DiscordInteractionResponseType int

const (
	DiscordInteractionMessage DiscordInteractionResponseType = iota + 1
	DiscordInteractionDeferred
)

type DiscordInteractionResponse struct {
	Type            DiscordInteractionResponseType
	Content         string
	Ephemeral       bool
	AllowedMentions DiscordAllowedMentions
}

// DiscordAPI is the complete fakeable Gateway/REST seam used by wall-e.
type DiscordAPI interface {
	SetHandlers(DiscordHandlers)
	SetIntents(DiscordIntent)
	Open(context.Context) error
	Close() error
	BulkOverwriteGlobalCommands(context.Context, string, []DiscordCommand) error
	SendMessage(context.Context, DiscordSend) (DiscordMessage, error)
	EditMessage(context.Context, DiscordEdit) error
	DeleteMessage(context.Context, string, string) error
	TriggerTyping(context.Context, string) error
	RespondInteraction(context.Context, DiscordInteraction, DiscordInteractionResponse) error
	EditInteractionResponse(context.Context, DiscordInteraction, DiscordEdit) error
	DeleteInteractionResponse(context.Context, DiscordInteraction) error
	CreateInteractionFollowup(context.Context, DiscordInteraction, DiscordSend) (DiscordMessage, error)
}

type discordGoAPI struct {
	session *discordgo.Session

	mu       sync.Mutex
	handlers DiscordHandlers
}

func newDiscordGoAPI(token string) (DiscordAPI, error) {
	if token == "" {
		return nil, errors.New("discord: token is required")
	}
	// discordgo v0.29.0 uses API v9. Discord's supported-version table was
	// rechecked on 2026-07-14 and still lists v9 and v10 as available.
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}
	a := &discordGoAPI{session: s}
	s.AddHandler(func(_ *discordgo.Session, event *discordgo.Ready) {
		if event == nil || event.User == nil {
			return
		}
		appID := ""
		if event.Application != nil {
			appID = event.Application.ID
		}
		a.mu.Lock()
		h := a.handlers.Ready
		a.mu.Unlock()
		if h != nil {
			h(DiscordReady{BotUserID: event.User.ID, BotUsername: event.User.Username, ApplicationID: appID})
		}
	})
	s.AddHandler(func(_ *discordgo.Session, event *discordgo.MessageCreate) {
		if event == nil || event.Message == nil {
			return
		}
		m := DiscordMessage{ID: event.ID, ChannelID: event.ChannelID, GuildID: event.GuildID, Content: event.Content, WebhookID: event.WebhookID}
		if event.Author != nil {
			m.Author = &DiscordUser{ID: event.Author.ID, Username: event.Author.Username, Bot: event.Author.Bot}
		}
		m.Attachments = make([]DiscordAttachment, 0, len(event.Attachments))
		for _, attachment := range event.Attachments {
			if attachment != nil {
				m.Attachments = append(m.Attachments, DiscordAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Size: int64(attachment.Size), URL: attachment.URL})
			}
		}
		a.mu.Lock()
		h := a.handlers.MessageCreate
		a.mu.Unlock()
		if h != nil {
			h(m)
		}
	})
	s.AddHandler(func(_ *discordgo.Session, event *discordgo.InteractionCreate) {
		if event == nil || event.Interaction == nil || event.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := event.ApplicationCommandData()
		i := DiscordInteraction{ID: event.ID, ApplicationID: event.AppID, Token: event.Token, ChannelID: event.ChannelID, Name: data.Name, Options: make(map[string]string)}
		for _, option := range data.Options {
			if option != nil && option.Type == discordgo.ApplicationCommandOptionString {
				if value, ok := option.Value.(string); ok {
					i.Options[option.Name] = value
				}
			}
		}
		a.mu.Lock()
		h := a.handlers.InteractionCreate
		a.mu.Unlock()
		if h != nil {
			h(i)
		}
	})
	return a, nil
}

func (a *discordGoAPI) SetHandlers(handlers DiscordHandlers) {
	a.mu.Lock()
	a.handlers = handlers
	a.mu.Unlock()
}

func (a *discordGoAPI) SetIntents(intents DiscordIntent) {
	var got discordgo.Intent
	if intents&DiscordIntentGuilds != 0 {
		got |= discordgo.IntentsGuilds
	}
	if intents&DiscordIntentGuildMessages != 0 {
		got |= discordgo.IntentsGuildMessages
	}
	if intents&DiscordIntentDirectMessages != 0 {
		got |= discordgo.IntentsDirectMessages
	}
	if intents&DiscordIntentMessageContent != 0 {
		got |= discordgo.IntentsMessageContent
	}
	a.session.Identify.Intents = got
}

func (a *discordGoAPI) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- a.session.Open() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("discord: open gateway: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = a.session.Close()
		return ctx.Err()
	}
}

func (a *discordGoAPI) Close() error { return a.session.Close() }

func (a *discordGoAPI) BulkOverwriteGlobalCommands(ctx context.Context, appID string, commands []DiscordCommand) error {
	payload := make([]*discordgo.ApplicationCommand, 0, len(commands))
	for _, command := range commands {
		options := make([]*discordgo.ApplicationCommandOption, 0, len(command.Options))
		for _, option := range command.Options {
			options = append(options, &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: option.Name, Description: option.Description, Required: option.Required, MaxLength: option.MaxLength})
		}
		payload = append(payload, &discordgo.ApplicationCommand{Type: discordgo.ChatApplicationCommand, Name: command.Name, Description: command.Description, Options: options})
	}
	_, err := a.session.ApplicationCommandBulkOverwrite(appID, "", payload, discordgo.WithContext(ctx))
	return err
}

func discordgoMentions(in DiscordAllowedMentions) *discordgo.MessageAllowedMentions {
	parse := make([]discordgo.AllowedMentionType, 0, len(in.Parse))
	for _, value := range in.Parse {
		parse = append(parse, discordgo.AllowedMentionType(value))
	}
	return &discordgo.MessageAllowedMentions{Parse: parse, Users: append([]string(nil), in.Users...), Roles: append([]string(nil), in.Roles...), RepliedUser: in.RepliedUser}
}

func discordgoFile(path string) (*os.File, *discordgo.File, error) {
	if path == "" {
		return nil, nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("discord: open upload: %w", err)
	}
	return file, &discordgo.File{Name: filepath.Base(path), Reader: file}, nil
}

func (a *discordGoAPI) SendMessage(ctx context.Context, send DiscordSend) (DiscordMessage, error) {
	file, upload, err := discordgoFile(send.FilePath)
	if err != nil {
		return DiscordMessage{}, err
	}
	if file != nil {
		defer file.Close()
	}
	payload := &discordgo.MessageSend{Content: send.Content, AllowedMentions: discordgoMentions(send.AllowedMentions)}
	if upload != nil {
		payload.Files = []*discordgo.File{upload}
	}
	if send.ReplyTo != "" {
		payload.Reference = &discordgo.MessageReference{MessageID: send.ReplyTo, ChannelID: send.ChannelID}
	}
	message, err := a.session.ChannelMessageSendComplex(send.ChannelID, payload, discordgo.WithContext(ctx))
	if err != nil {
		return DiscordMessage{}, err
	}
	return DiscordMessage{ID: message.ID, ChannelID: message.ChannelID, Content: message.Content}, nil
}

func (a *discordGoAPI) EditMessage(ctx context.Context, edit DiscordEdit) error {
	content := edit.Content
	_, err := a.session.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: edit.MessageID, Channel: edit.ChannelID, Content: &content, AllowedMentions: discordgoMentions(edit.AllowedMentions)}, discordgo.WithContext(ctx))
	return err
}

func (a *discordGoAPI) DeleteMessage(ctx context.Context, channelID, messageID string) error {
	return a.session.ChannelMessageDelete(channelID, messageID, discordgo.WithContext(ctx))
}

func (a *discordGoAPI) TriggerTyping(ctx context.Context, channelID string) error {
	return a.session.ChannelTyping(channelID, discordgo.WithContext(ctx))
}

func discordgoInteraction(interaction DiscordInteraction) *discordgo.Interaction {
	return &discordgo.Interaction{ID: interaction.ID, AppID: interaction.ApplicationID, Token: interaction.Token, ChannelID: interaction.ChannelID}
}

func (a *discordGoAPI) RespondInteraction(ctx context.Context, interaction DiscordInteraction, response DiscordInteractionResponse) error {
	typ := discordgo.InteractionResponseChannelMessageWithSource
	if response.Type == DiscordInteractionDeferred {
		typ = discordgo.InteractionResponseDeferredChannelMessageWithSource
	}
	flags := discordgo.MessageFlags(0)
	if response.Ephemeral {
		flags |= discordgo.MessageFlagsEphemeral
	}
	return a.session.InteractionRespond(discordgoInteraction(interaction), &discordgo.InteractionResponse{Type: typ, Data: &discordgo.InteractionResponseData{Content: response.Content, Flags: flags, AllowedMentions: discordgoMentions(response.AllowedMentions)}}, discordgo.WithContext(ctx))
}

func (a *discordGoAPI) EditInteractionResponse(ctx context.Context, interaction DiscordInteraction, edit DiscordEdit) error {
	content := edit.Content
	_, err := a.session.InteractionResponseEdit(discordgoInteraction(interaction), &discordgo.WebhookEdit{Content: &content, AllowedMentions: discordgoMentions(edit.AllowedMentions)}, discordgo.WithContext(ctx))
	return err
}

func (a *discordGoAPI) DeleteInteractionResponse(ctx context.Context, interaction DiscordInteraction) error {
	return a.session.InteractionResponseDelete(discordgoInteraction(interaction), discordgo.WithContext(ctx))
}

func (a *discordGoAPI) CreateInteractionFollowup(ctx context.Context, interaction DiscordInteraction, send DiscordSend) (DiscordMessage, error) {
	file, upload, err := discordgoFile(send.FilePath)
	if err != nil {
		return DiscordMessage{}, err
	}
	if file != nil {
		defer file.Close()
	}
	payload := &discordgo.WebhookParams{Content: send.Content, AllowedMentions: discordgoMentions(send.AllowedMentions)}
	if upload != nil {
		payload.Files = []*discordgo.File{upload}
	}
	message, err := a.session.FollowupMessageCreate(discordgoInteraction(interaction), true, payload, discordgo.WithContext(ctx))
	if err != nil {
		return DiscordMessage{}, err
	}
	return DiscordMessage{ID: message.ID, ChannelID: message.ChannelID, Content: message.Content}, nil
}
