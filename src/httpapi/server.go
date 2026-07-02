package httpapi

// server.go implements the wall-e HTTP gateway: /health (no auth) and
// /v1/prompt (bearer auth, SSE stream from a pooled pi process).
//
// Concurrency / lifecycle
// ------------------------
// /v1/prompt does NOT re-serialize per-channel requests itself: the pool already
// does (Acquire blocks while the channel's slot is busy). The HTTP layer
// bounds the wait with a queue-timeout context (WALLE_HTTP_QUEUE_TIMEOUT,
// default 60s): if Acquire doesn't succeed in time, it returns 503.
//
// Once acquired, the handler sends `prompt`, then streams events from
// Slot.Events() to the client as SSE until it sees agent_end (the turn is
// done). It also watches the request context: if the client disconnects
// mid-stream, it aborts the slot's pi process (so the next Acquire drains fast
// / the process is released) and Releases the slot.
//
// SSE event mapping
// -----------------
//   pi EventAgentStart  → event: agent_start\ndata: {}\n\n
//   pi EventMessageUpdate with text_delta → event: delta\ndata: {"text":...}\n\n
//   pi EventAgentEnd    → event: agent_end\ndata: {}\n\n
//   turn complete       → event: done\ndata: {}\n\n
//   error mid-stream    → event: error\ndata: {"message":"..."}\n\n then close

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"wall-e/pool"
	"wall-e/rpc"
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
}

// Server is the wall-e HTTP gateway.
type Server struct {
	cfg  Config
	pool *pool.Pool
	mux  *http.ServeMux
}

// New builds a Server over the given pool. The pool owns per-channel
// serialization; the HTTP layer only bounds the wait.
func New(cfg Config, p *pool.Pool) *Server {
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 60 * time.Second
	}
	s := &Server{cfg: cfg, pool: p, mux: http.NewServeMux()}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/v1/prompt", s.handlePrompt)
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

// ConfigFromEnv builds an httpapi.Config from WALLE_* env vars (Phase 4 cares
// about WALLE_TOKEN, WALLE_PORT, WALLE_HTTP_QUEUE_TIMEOUT). Returns an error
// if WALLE_TOKEN is unset or empty.
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
	return cfg, nil
}

// --- Handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// promptRequest is the body of POST /v1/prompt.
type promptRequest struct {
	Channel string `json:"channel"`
	Message string `json:"message"`
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
	var req promptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, 400, "invalid JSON body")
		return
	}
	if req.Message == "" {
		writeJSONError(w, 400, "missing message")
		return
	}
	if req.Channel == "" {
		writeJSONError(w, 400, "missing channel")
		return
	}

	// Bound the Acquire wait: the pool serializes same-channel requests; if
	// the channel is busy for longer than QueueTimeout, return 503.
	acqCtx, acqCancel := context.WithTimeout(r.Context(), s.cfg.QueueTimeout)
	defer acqCancel()
	chID := pool.ChannelID(req.Channel)
	slot, err := s.pool.Acquire(acqCtx, chID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// Distinguish client-disconnect (r.Context) from queue timeout.
			if r.Context().Err() != nil {
				return // client already gone
			}
			writeJSONError(w, 503, "channel busy")
			return
		}
		writeJSONError(w, 502, fmt.Sprintf("pool acquire failed: %v", err))
		return
	}
	defer s.pool.Release(chID)

	// Send the prompt. Use a context tied to the request so a client
	// disconnect propagates; the client's RPC RequestTimeout (if any) does
	// not bound streaming — only the acceptance response.
	if _, err := slot.Client().Prompt(r.Context(), req.Message, false); err != nil {
		writeJSONError(w, 502, fmt.Sprintf("prompt failed: %v", err))
		return
	}

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

	// A goroutine to abort the slot when the client disconnects. We watch
	// r.Context(); on cancel we send abort (best-effort) so the pool's
	// drain-on-reuse for the next Acquire completes promptly.
	abortCtx, abortCancel := context.WithCancel(context.Background())
	defer abortCancel()
	go func() {
		select {
		case <-r.Context().Done():
			// Client gone: abort the in-flight turn. Best-effort; tolerate a
			// process that has already exited.
			_, _ = slot.Client().Abort(abortCtx)
		case <-abortCtx.Done():
			// Handler finished normally.
		}
	}()

	turnDone := false
	for ev := range slot.Events() {
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
			break
		}
	}

	// If the client disconnected mid-stream, the Events channel may have
	// closed (process exited via abort→agent_end→forwarder exit) before we
	// saw agent_end. Emit done only when we observed a clean agent_end.
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

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}
