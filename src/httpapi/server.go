package httpapi

// server.go implements the wall-e HTTP gateway: /health (no auth) and
// /v1/prompt (bearer auth, SSE stream from a pooled pi process).
//
// Concurrency / lifecycle
// ------------------------
// /v1/prompt routes typed channels through PromptAdapters. A shared turn.Manager
// owns active-turn state across HTTP/CLI/chat front-ends: same-channel messages
// during an in-flight turn are steered, while new turns acquire the pool. The
// HTTP layer bounds the initial acquire/steer wait with WALLE_HTTP_QUEUE_TIMEOUT
// (default 60s).
//
// Once accepted, the handler streams the turn subscription as SSE until it sees
// agent_end. If the client disconnects, the handler detaches from the shared
// turn without aborting it, because an external chat delivery adapter may still
// be streaming the same assistant response.
//
// SSE event mapping
// -----------------
//   pi EventAgentStart  → event: agent_start\ndata: {}\n\n
//   pi EventMessageUpdate with text_delta → event: delta\ndata: {"text":...}\n\n
//   pi EventAgentEnd    → event: agent_end\ndata: {}\n\n
//   turn complete       → event: done\ndata: {}\n\n
//   error mid-stream    → event: error\ndata: {"message":"..."}\n\n then close

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wall-e/media"
	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
	"wall-e/turn"
)

// Config configures the HTTP server.
type Config struct {
	// Token is the required bearer token for /v1/prompt. Empty disables auth
	// (dev/test only); New() from env still requires it via WALLE_TOKEN.
	Token string
	// Addr is the listen address (e.g. ":6007").
	Addr string
	// QueueTimeout bounds how long Acquire may block on a busy channel before
	// returning 503. Defaults to 60s.
	QueueTimeout time.Duration
	// SiteDir is the directory served at /. Empty disables static serving.
	SiteDir string
	// Sessions is the session manager used by the read-only debug endpoints.
	Sessions *session.Manager
	// RPCConfig is used by the default exporter to spawn a short-lived pi RPC
	// process for export_html.
	RPCConfig rpc.Config
	// ExportTimeout bounds one session HTML export. Defaults to 30s.
	ExportTimeout time.Duration
	// Exporter writes exported session HTML to an output path. If nil and
	// Sessions is configured, New installs the RPC-backed exporter.
	Exporter SessionExporter
	// Turns coordinates active prompt turns across HTTP/CLI/chat front-ends. If
	// nil, New installs a manager over the supplied pool.
	Turns *turn.Manager
	// PromptAdapters route typed /v1/prompt requests by channelType. New always
	// installs/overwrites the "http" adapter. Chat front-ends such as Telegram
	// register additional adapters from main.
	PromptAdapters map[string]PromptAdapter
	// MaxPromptBytes bounds JSON prompt request bodies. Defaults to 8 MiB.
	MaxPromptBytes int64
	// SendAdapters route typed /v1/send requests by channelType. Chat front-ends
	// such as Telegram register their direct-delivery adapter from main.
	SendAdapters map[string]SendAdapter
}

// Server is the wall-e HTTP gateway.
type Server struct {
	cfg  Config
	pool *pool.Pool
	mux  *http.ServeMux
}

// PromptAdapter handles one typed channel target for /v1/prompt.
type PromptAdapter interface {
	Prompt(ctx context.Context, channel string, message string) (*turn.Subscription, error)
}

type httpPromptAdapter struct{ turns *turn.Manager }

func (a httpPromptAdapter) Prompt(ctx context.Context, channel string, message string) (*turn.Subscription, error) {
	chID := pool.ChannelID(session.NewChannelID("http", channel))
	sub, _, err := a.turns.Submit(ctx, chID, message, turn.SubmitOptions{SubscribeOnSteer: true})
	return sub, err
}

// SendRequest asks a channel adapter to deliver text and/or one local file
// directly without creating an agent turn.
type SendRequest struct {
	Channel   string
	Text      string
	MediaPath string
	Caption   string
}

// SentItem describes one delivered item in a direct send result.
type SentItem struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
}

// SendResult is returned by a direct delivery adapter.
type SendResult struct {
	Sent []SentItem `json:"sent"`
}

// SendAdapter handles one typed channel target for /v1/send.
type SendAdapter interface {
	Send(ctx context.Context, req SendRequest) (SendResult, error)
}

// SessionExporter exports a session file to an HTML output path.
type SessionExporter interface {
	ExportHTML(ctx context.Context, sessionPath string, outputPath string) error
}

// RPCSessionExporter implements SessionExporter using a short-lived pi RPC
// process, so debug exports do not disturb warm pool slots.
type RPCSessionExporter struct{ RPCConfig rpc.Config }

func (e RPCSessionExporter) ExportHTML(ctx context.Context, sessionPath string, outputPath string) error {
	c, err := rpc.New(e.RPCConfig)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, _, err := c.SwitchSession(ctx, sessionPath); err != nil {
		return err
	}
	_, err = c.ExportHTML(ctx, outputPath)
	return err
}

// New builds a Server over the given pool. The pool owns per-channel
// serialization; the HTTP layer only bounds the wait.
func New(cfg Config, p *pool.Pool) *Server {
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 60 * time.Second
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 30 * time.Second
	}
	if cfg.Exporter == nil {
		cfg.Exporter = RPCSessionExporter{RPCConfig: cfg.RPCConfig}
	}
	if cfg.Turns == nil {
		cfg.Turns = turn.NewManager(context.Background(), p)
	}
	if cfg.PromptAdapters == nil {
		cfg.PromptAdapters = make(map[string]PromptAdapter)
	}
	cfg.PromptAdapters["http"] = httpPromptAdapter{turns: cfg.Turns}
	if cfg.MaxPromptBytes <= 0 {
		cfg.MaxPromptBytes = 8 << 20
	}
	s := &Server{cfg: cfg, pool: p, mux: http.NewServeMux()}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/v1/prompt", s.handlePrompt)
	s.mux.HandleFunc("/v1/send", s.handleSend)
	s.mux.HandleFunc("/v1/sessions", s.handleSessions)
	s.mux.HandleFunc("/v1/sessions/", s.handleSessionDetail)
	if cfg.SiteDir != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(cfg.SiteDir)))
	}
	return s
}

// Handler returns the http.Handler (for httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the HTTP server (blocks).
func (s *Server) ListenAndServe() error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":6007"
	}
	srv := &http.Server{Addr: addr, Handler: s.mux}
	return srv.ListenAndServe()
}

// ConfigFromEnv builds an httpapi.Config from WALLE_* env vars. Returns an
// error if WALLE_TOKEN is unset or empty.
func ConfigFromEnv() (Config, error) {
	cfg := Config{Token: os.Getenv("WALLE_TOKEN")}
	port := os.Getenv("WALLE_PORT")
	if port == "" {
		port = "6007"
	}
	cfg.Addr = ":" + port
	if cfg.Token == "" {
		return cfg, fmt.Errorf("httpapi: WALLE_TOKEN is required")
	}
	if qt := os.Getenv("WALLE_HTTP_QUEUE_TIMEOUT"); qt != "" {
		d, err := time.ParseDuration(qt)
		if err != nil {
			return cfg, fmt.Errorf("httpapi: invalid WALLE_HTTP_QUEUE_TIMEOUT %q: %w", qt, err)
		}
		cfg.QueueTimeout = d
	}
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 60 * time.Second
	}
	cfg.SiteDir = os.Getenv("WALLE_SITE")
	if cfg.SiteDir == "" {
		cfg.SiteDir = "/opt/wall-e/www"
	}
	if et := os.Getenv("WALLE_SESSION_EXPORT_TIMEOUT"); et != "" {
		d, err := time.ParseDuration(et)
		if err != nil {
			return cfg, fmt.Errorf("httpapi: invalid WALLE_SESSION_EXPORT_TIMEOUT %q: %w", et, err)
		}
		cfg.ExportTimeout = d
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 30 * time.Second
	}
	return cfg, nil
}

// --- Handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/sessions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, 405, "method not allowed")
		return
	}
	if s.cfg.Sessions == nil {
		writeJSONError(w, 404, "sessions unavailable")
		return
	}
	sessions, err := s.cfg.Sessions.ListSessionFiles()
	if err != nil {
		writeJSONError(w, 500, fmt.Sprintf("list sessions failed: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": sessions})
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, 405, "method not allowed")
		return
	}
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	key, action, ok := strings.Cut(rest, "/")
	if !ok || key == "" || strings.Contains(key, "/") {
		http.NotFound(w, r)
		return
	}
	if s.cfg.Sessions == nil {
		writeJSONError(w, 404, "sessions unavailable")
		return
	}
	sf, found, err := s.cfg.Sessions.ResolveSessionKey(key)
	if err != nil {
		writeJSONError(w, 500, fmt.Sprintf("resolve session failed: %v", err))
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "export.html":
		s.handleSessionExport(w, r, sf)
	case "messages":
		s.handleSessionMessages(w, r, sf)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request, sf session.SessionFile) {
	messages, err := s.cfg.Sessions.ReadTranscriptMessages(sf.Path)
	if err != nil {
		writeJSONError(w, 500, fmt.Sprintf("read session messages failed: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"session": sf, "messages": messages})
}

func (s *Server) handleSessionExport(w http.ResponseWriter, r *http.Request, sf session.SessionFile) {
	tmp, err := os.CreateTemp("", "walle-session-*.html")
	if err != nil {
		writeJSONError(w, 500, fmt.Sprintf("create temp file failed: %v", err))
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	exportCtx, cancel := context.WithTimeout(r.Context(), s.cfg.ExportTimeout)
	defer cancel()
	if err := s.cfg.Exporter.ExportHTML(exportCtx, sf.Path, tmpPath); err != nil {
		writeJSONError(w, 502, fmt.Sprintf("export session failed: %v", err))
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		writeJSONError(w, 500, fmt.Sprintf("open export failed: %v", err))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="session-%s.html"`, safeDownloadName(sf.Datestamp)))
	w.WriteHeader(200)
	_, _ = io.Copy(w, f)
}

// promptRequest is the body of POST /v1/prompt.
type promptRequest struct {
	ChannelType string             `json:"channelType"`
	Channel     string             `json:"channel"`
	Message     string             `json:"message"`
	Attachments []promptAttachment `json:"attachments,omitempty"`
}

type promptAttachment struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data"`
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, 405, "method not allowed")
		return
	}
	if !authorize(r, s.cfg.Token) {
		writeJSONError(w, 401, "unauthorized")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxPromptBytes)
	var req promptRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, 413, fmt.Sprintf("prompt request too large (max %d bytes)", s.cfg.MaxPromptBytes))
			return
		}
		writeJSONError(w, 400, "invalid JSON body")
		return
	}
	req.ChannelType = strings.TrimSpace(req.ChannelType)
	req.Channel = strings.TrimSpace(req.Channel)
	if req.ChannelType == "" {
		writeJSONError(w, 400, "missing channelType")
		return
	}
	if strings.ContainsAny(req.ChannelType, ":/\\") {
		writeJSONError(w, 400, "invalid channelType")
		return
	}
	if req.Message == "" && len(req.Attachments) == 0 {
		writeJSONError(w, 400, "missing message")
		return
	}
	if req.Channel == "" {
		writeJSONError(w, 400, "missing channel")
		return
	}
	adapter := s.cfg.PromptAdapters[req.ChannelType]
	if adapter == nil {
		writeJSONError(w, 400, fmt.Sprintf("unsupported channelType %q", req.ChannelType))
		return
	}

	message := req.Message
	if len(req.Attachments) > 0 {
		store, err := s.mediaStore()
		if err != nil {
			writeJSONError(w, 500, err.Error())
			return
		}
		files := make([]media.SavedFile, 0, len(req.Attachments))
		for i, att := range req.Attachments {
			if strings.TrimSpace(att.Data) == "" {
				writeJSONError(w, 400, fmt.Sprintf("attachment %d missing data", i))
				return
			}
			name := strings.TrimSpace(att.FileName)
			if name == "" {
				name = fmt.Sprintf("attachment-%d", i+1)
			}
			decoded, err := base64.StdEncoding.DecodeString(att.Data)
			if err != nil {
				writeJSONError(w, 400, fmt.Sprintf("attachment %d invalid base64 data", i))
				return
			}
			saved, err := store.Save(r.Context(), name, bytes.NewReader(decoded))
			if err != nil {
				writeJSONError(w, 500, fmt.Sprintf("save attachment %d failed: %v", i, err))
				return
			}
			if strings.TrimSpace(att.MimeType) != "" {
				saved.MimeType = strings.TrimSpace(att.MimeType)
			}
			files = append(files, saved)
		}
		message = media.FormatAttachmentPrompt(req.Message, files)
	}

	promptCtx, promptCancel := context.WithTimeout(r.Context(), s.cfg.QueueTimeout)
	defer promptCancel()
	sub, err := adapter.Prompt(promptCtx, req.Channel, message)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeJSONError(w, 503, "channel busy")
			return
		}
		if strings.Contains(err.Error(), "not allowed") {
			writeJSONError(w, 403, err.Error())
			return
		}
		writeJSONError(w, 502, fmt.Sprintf("prompt failed: %v", err))
		return
	}
	if sub == nil {
		writeJSONError(w, 502, "prompt adapter returned no stream")
		return
	}
	defer sub.Close()

	// Stream SSE.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher.Flush()

	turnDone := false
streamLoop:
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events:
			if !ok {
				break streamLoop
			}
			switch ev.Type {
			case rpc.EventAgentStart:
				writeSSE(w, "agent_start", "{}")
			case rpc.EventAgentEnd:
				writeSSE(w, "agent_end", "{}")
				turnDone = true
			case rpc.EventMessageUpdate:
				text, ok := decodeTextDelta(ev.Raw)
				if !ok {
					continue
				}
				b, _ := json.Marshal(map[string]string{"text": text})
				writeSSE(w, "delta", string(b))
			default:
				// Forward other event types as a generic "delta"? No — only text
				// deltas become `delta` SSE events per the v1 format. Ignore the
				// rest (tool execution, thinking, etc.) for v1.
			}
			flusher.Flush()
			if turnDone {
				break streamLoop
			}
		}
	}

	// Emit done only when we observed a clean agent_end.
	if turnDone {
		writeSSE(w, "done", "{}")
		flusher.Flush()
	} else if r.Context().Err() != nil {
		// Client disconnect: nothing more to send.
		return
	} else {
		// Events channel closed without agent_end: the process died. Surface
		// as an error event.
		writeSSE(w, "error", `{"message":"agent stream ended unexpectedly"}`)
		flusher.Flush()
	}
}

// sendRequest is the body of POST /v1/send.
type sendRequest struct {
	ChannelType string `json:"channelType"`
	Channel     string `json:"channel"`
	Text        string `json:"text,omitempty"`
	MediaPath   string `json:"mediaPath,omitempty"`
	Caption     string `json:"caption,omitempty"`
}

type sendResponse struct {
	OK      bool       `json:"ok"`
	Channel string     `json:"channel,omitempty"`
	Sent    []SentItem `json:"sent,omitempty"`
	Error   string     `json:"error,omitempty"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, 405, sendResponse{OK: false, Error: "method not allowed"})
		return
	}
	if !authorize(r, s.cfg.Token) {
		writeJSON(w, 401, sendResponse{OK: false, Error: "unauthorized"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxPromptBytes)
	var req sendRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, 413, sendResponse{OK: false, Error: fmt.Sprintf("send request too large (max %d bytes)", s.cfg.MaxPromptBytes)})
			return
		}
		writeJSON(w, 400, sendResponse{OK: false, Error: "invalid JSON body"})
		return
	}
	req.ChannelType = strings.TrimSpace(req.ChannelType)
	req.Channel = strings.TrimSpace(req.Channel)
	req.MediaPath = strings.TrimSpace(req.MediaPath)
	if req.ChannelType == "" {
		writeJSON(w, 400, sendResponse{OK: false, Error: "missing channelType"})
		return
	}
	if strings.ContainsAny(req.ChannelType, ":/\\") {
		writeJSON(w, 400, sendResponse{OK: false, Error: "invalid channelType"})
		return
	}
	if req.Channel == "" {
		writeJSON(w, 400, sendResponse{OK: false, Error: "missing channel"})
		return
	}
	if req.Text == "" && req.MediaPath == "" {
		writeJSON(w, 400, sendResponse{OK: false, Error: "missing text or mediaPath"})
		return
	}
	if req.MediaPath != "" {
		clean := filepath.Clean(req.MediaPath)
		if !filepath.IsAbs(clean) || clean != req.MediaPath {
			writeJSON(w, 400, sendResponse{OK: false, Error: "mediaPath must be absolute and clean"})
			return
		}
		info, err := os.Stat(clean)
		if err != nil {
			writeJSON(w, 400, sendResponse{OK: false, Error: fmt.Sprintf("mediaPath unavailable: %v", err)})
			return
		}
		if !info.Mode().IsRegular() {
			writeJSON(w, 400, sendResponse{OK: false, Error: "mediaPath is not a regular file"})
			return
		}
	}
	adapter := s.cfg.SendAdapters[req.ChannelType]
	if adapter == nil {
		writeJSON(w, 400, sendResponse{OK: false, Error: fmt.Sprintf("unsupported channelType %q", req.ChannelType)})
		return
	}
	res, err := adapter.Send(r.Context(), SendRequest{Channel: req.Channel, Text: req.Text, MediaPath: req.MediaPath, Caption: req.Caption})
	if err != nil {
		code := 502
		if strings.Contains(err.Error(), "not allowed") {
			code = 403
		}
		writeJSON(w, code, sendResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, 200, sendResponse{OK: true, Channel: req.ChannelType + ":" + req.Channel, Sent: res.Sent})
}

func (s *Server) mediaStore() (*media.Store, error) {
	if s.cfg.Sessions != nil {
		return media.NewStore(s.cfg.Sessions.SessionDir()), nil
	}
	if s.cfg.RPCConfig.SessionDir != "" {
		return media.NewStore(s.cfg.RPCConfig.SessionDir), nil
	}
	return nil, errors.New("media store unavailable: session dir is not configured")
}

// decodeTextDelta extracts the text delta from a message_update event's
// assistantMessageEvent, returning (delta, true) for text_delta deltas and
// (..., false) for anything else.
func decodeTextDelta(raw json.RawMessage) (string, bool) {
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

// writeSSE writes one SSE event: "event: <name>\ndata: <data>\n\n".
func writeSSE(w http.ResponseWriter, name, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

func safeDownloadName(s string) string {
	out := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == 'T' || r == 'Z' {
			return r
		}
		return '-'
	}, s)
	if out == "" {
		return "session"
	}
	return filepath.Base(out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
