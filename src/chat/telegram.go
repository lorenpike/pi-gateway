package chat

// telegram.go is the real TelegramAPI adapter: a thin shim over the Telegram
// Bot API (https://api.telegram.org/bot<token>/<method>) using only net/http.
// This keeps the gateway stdlib-only (no go-telegram-bot-api / telebot dep):
// the small set of methods wall-e needs (getMe, getUpdates, sendMessage,
// sendChatAction, setMyCommands, and media operations) are straightforward.
//
// Tradeoff note (Phase 6 decision): hand-rolling preserves the module's
// zero-third-party-dep invariant and the plan's "stdlib-only" framing, at the
// cost of re-implementing request/response envelopes and (later) retry/backoff
// that a library would provide. For v1's four calls + long-poll getUpdates the
// surface is small enough that hand-rolling is the lighter choice; revisit if we
// need inline keyboards, file uploads, webhook handling, or sophisticated rate
// limiting.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

const (
	telegramHTTPTimeout       = 60 * time.Second
	telegramHTTP2PingInterval = 15 * time.Second
	telegramHTTP2PingTimeout  = 5 * time.Second
)

// httpAPI is the real TelegramAPI over net/http.
type httpAPI struct {
	token  string
	base   string
	client *http.Client
}

func newHTTPTelegramAPI(token, base string) TelegramAPI {
	// Telegram long-polling keeps one HTTP/2 connection active indefinitely.
	// Health-check that connection so a silently blackholed connection is
	// closed instead of making getUpdates and unrelated sends all wait for the
	// full client timeout while a fresh curl connection works normally.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: telegramHTTP2PingInterval,
		PingTimeout:     telegramHTTP2PingTimeout,
	}
	return &httpAPI{
		token: token,
		base:  base,
		// Long-poll getUpdates may block ~30s; allow generous headroom.
		client: &http.Client{Transport: transport, Timeout: telegramHTTPTimeout},
	}
}

// tgResponse is the common envelope of every Telegram API response.
type tgResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// telegramAPIError represents a definitive JSON error returned by Telegram.
// Keeping it distinct from transport errors lets callers safely fall back only
// when Telegram rejected an operation, rather than retrying an upload whose
// outcome is unknown after a timeout.
type telegramAPIError struct {
	Method      string
	Description string
	Code        int
}

func (e *telegramAPIError) Error() string {
	return fmt.Sprintf("telegram: %s failed: %s (code %d)", e.Method, e.Description, e.Code)
}

func isTelegramPhotoRejection(err error) bool {
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == http.StatusBadRequest || apiErr.Code == http.StatusRequestEntityTooLarge
}

// telegramTransportError strips net/url.Error's URL before wrapping its cause.
// Telegram API URLs contain the bot token, so returning url.Error directly
// would expose the token in logs and /v1/send error JSON.
func telegramTransportError(operation string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	return fmt.Errorf("telegram: %s: %w", operation, err)
}

func (h *httpAPI) call(ctx context.Context, method string, payload map[string]any, result any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("telegram: marshal %s: %w", method, err)
		}
		body = bytes.NewReader(b)
	}
	url := fmt.Sprintf("%s/bot%s/%s", h.base, h.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("telegram: request %s: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return telegramTransportError("call "+method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: read %s: %w", method, err)
	}
	var env tgResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("telegram: decode %s: %w", method, err)
	}
	if !env.OK {
		return &telegramAPIError{Method: method, Description: env.Description, Code: env.ErrorCode}
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("telegram: decode %s result: %w", method, err)
		}
	}
	return nil
}

func (h *httpAPI) GetMe(ctx context.Context) (User, error) {
	var u User
	if err := h.call(ctx, "getMe", nil, &u); err != nil {
		return User{}, err
	}
	return u, nil
}

func (h *httpAPI) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	payload := map[string]any{
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	}
	if offset > 0 {
		payload["offset"] = offset
	}
	// Bound this long-poll through its context. Never mutate the shared
	// http.Client: SendMessage/uploads run concurrently with polling, and
	// changing Client.Timeout races with those requests and gives them the
	// poll-specific deadline.
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+10*time.Second)
	defer cancel()

	var ups []Update
	if err := h.call(pollCtx, "getUpdates", payload, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

func (h *httpAPI) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (Message, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       renderTelegramMarkdown(text),
		"parse_mode": telegramParseModeHTML,
	}
	if replyTo > 0 {
		payload["reply_to_message_id"] = replyTo
	}
	var msg Message
	if err := h.call(ctx, "sendMessage", payload, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func (h *httpAPI) SendChatAction(ctx context.Context, chatID int64, action string) error {
	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	if err := h.call(ctx, "sendChatAction", payload, nil); err != nil {
		return err
	}
	return nil
}

func (h *httpAPI) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	payload := map[string]any{"commands": commands}
	if err := h.call(ctx, "setMyCommands", payload, nil); err != nil {
		return err
	}
	return nil
}

func (h *httpAPI) GetFile(ctx context.Context, fileID string) (File, error) {
	var f File
	if err := h.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return File{}, err
	}
	return f, nil
}

func (h *httpAPI) DownloadFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	if filePath == "" {
		return nil, fmt.Errorf("telegram: empty file_path")
	}
	url := fmt.Sprintf("%s/file/bot%s/%s", h.base, h.token, filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: file request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, telegramTransportError("download file", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("telegram: download file failed: %s %s", resp.Status, string(data))
	}
	return resp.Body, nil
}

func (h *httpAPI) SendPhoto(ctx context.Context, chatID int64, path string, caption string) (Message, error) {
	var msg Message
	if err := h.callMultipart(ctx, "sendPhoto", path, "photo", chatID, caption, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func (h *httpAPI) SendDocument(ctx context.Context, chatID int64, path string, caption string) (Message, error) {
	var msg Message
	if err := h.callMultipart(ctx, "sendDocument", path, "document", chatID, caption, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func (h *httpAPI) callMultipart(ctx context.Context, method string, path string, field string, chatID int64, caption string, result any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("telegram: open upload: %w", err)
	}
	defer file.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = mw.WriteField("caption", renderTelegramMarkdown(caption))
		_ = mw.WriteField("parse_mode", telegramParseModeHTML)
	}
	part, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("telegram: create upload part: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("telegram: copy upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("telegram: close upload: %w", err)
	}
	url := fmt.Sprintf("%s/bot%s/%s", h.base, h.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return fmt.Errorf("telegram: request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err != nil {
		return telegramTransportError("call "+method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: read %s: %w", method, err)
	}
	var env tgResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("telegram: decode %s: %w", method, err)
	}
	if !env.OK {
		return &telegramAPIError{Method: method, Description: env.Description, Code: env.ErrorCode}
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("telegram: decode %s result: %w", method, err)
		}
	}
	return nil
}
