package httpapi

// server_test.go exercises the HTTP API against a pool wired with in-process
// fake pis (Phase 4 of the wall-e gateway plan). See archive/20260627--walle-gateway.md
// ┬º6 Phase 4 for the test list.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
)

// fakeHandlerCfg configures a per-slot fake pi handler.
type fakeHandlerCfg struct {
	sessionFile string
	// streamDone: if non-nil, a prompt emits agent_start then waits for this
	// channel to be closed before emitting agent_end (simulating a long
	// stream). If nil, the agent streams its assistant text and ends.
	streamDone chan struct{}
}

func makeHandler(cfg fakeHandlerCfg) func(f *fakePI, cmd map[string]any) {
	return func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		ctype, _ := cmd["type"].(string)
		switch ctype {
		case "switch_session":
			f.writeResp(id, "switch_session", true, map[string]any{"cancelled": false})
		case "get_state":
			f.writeResp(id, "get_state", true, map[string]any{
				"data": map[string]any{
					"sessionFile": cfg.sessionFile,
					"isStreaming": false,
				},
			})
		case "prompt":
			f.writeResp(id, "prompt", true, nil)
			if cfg.streamDone != nil {
				// Withhold agent_end until streamDone closes; abort will
				// short-circuit it.
				f.writeJSON(map[string]any{"type": "agent_start"})
				done := cfg.streamDone
				go func() {
					select {
					case <-done:
						f.emitAssistantText("a", "b")
					case <-f.stop:
					}
				}()
			} else {
				f.emitAssistantText("Hello ", "world")
			}
		case "abort":
			f.writeResp(id, "abort", true, nil)
			f.writeJSON(map[string]any{"type": "agent_end"})
		default:
			f.writeResp(id, ctype, true, nil)
		}
	}
}

// testPool builds a pool backed by a fakeFactory and a session.Manager over a
// temp dir, mirroring the pool package's test harness so httpapi tests exercise
// the real pool with in-process fakes.
func testPool(t *testing.T, size int, makeHandlerFor func(slotIdx int) fakeHandlerCfg) (*pool.Pool, *fakeFactory, *session.Manager) {
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
	var idxMu sync.Mutex
	idx := 0
	newClient := func(cfg rpc.Config) (*rpc.Client, error) {
		idxMu.Lock()
		i := idx
		idx++
		idxMu.Unlock()
		hcfg := makeHandlerFor(i)
		f := newFakePI()
		h := makeHandler(hcfg)
		f.start(func(pf *fakePI, cmd map[string]any) { h(pf, cmd) })
		c := rpc.NewClientFromStreams(f.stdinWriter, f.stdoutReader, cfg)
		ff.mu.Lock()
		id := ff.nextID
		ff.nextID++
		ff.fakes[id] = f
		ff.mu.Unlock()
		return c, nil
	}
	p, err := pool.New(pool.Config{
		Size:         size,
		DrainTimeout: 200 * time.Millisecond,
		Sessions:     sm,
		RPCConfig:    rpc.Config{UIPolicy: rpc.DefaultExtensionUIPolicy()},
		NewClient:    newClient,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
		ff.closeAll()
	})
	return p, ff, sm
}

// newServer builds a Server with the given pool and config.
func newServer(t *testing.T, p *pool.Pool, cfg Config) *Server {
	s := New(cfg, p)
	return s
}

// do posts/gets against a test server.
func do(t *testing.T, s *Server, method, path, auth string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// sseReader reads SSE events from a response body. Returns events as
// (name, data) pairs. It returns once the body reaches EOF or the timeout
// fires.
func readSSE(t *testing.T, body io.Reader, timeout time.Duration) []sseEvent {
	t.Helper()
	events := make(chan sseEvent, 64)
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var name, data string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if name != "" || data != "" {
					events <- sseEvent{name: name, data: data}
					name, data = "", ""
				}
				continue
			}
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		close(done)
	}()
	var out []sseEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				<-done
				return out
			}
			out = append(out, ev)
		case <-done:
			// Drain any remaining buffered events.
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						return out
					}
					out = append(out, ev)
				default:
					return out
				}
			}
		case <-deadline:
			t.Fatalf("readSSE: timed out after %v, got %d events", timeout, len(out))
		}
	}
}

type sseEvent struct {
	name string
	data string
}

type fakeExporter struct{}

func (fakeExporter) ExportHTML(ctx context.Context, sessionPath string, outputPath string) error {
	return os.WriteFile(outputPath, []byte("<html><body>exported "+filepath.Base(sessionPath)+"</body></html>"), 0o644)
}

func sseNames(ev []sseEvent) []string {
	out := make([]string, len(ev))
	for i, e := range ev {
		out[i] = e.name
	}
	return out
}

// --- Tests ----------------------------------------------------------------

func TestHealth_NoAuth_Returns200(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodGet, "/health", "", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("body = %v, want status=ok", got)
	}
}

func TestStaticSite_ServesIndex(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	site := t.TempDir()
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>debug</h1>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	s := newServer(t, p, Config{Token: "sekret", SiteDir: site})
	rr := do(t, s, http.MethodGet, "/", "", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "debug") {
		t.Fatalf("body = %q, want index contents", rr.Body.String())
	}
}

func TestSessions_ListAndExport_NoAuth(t *testing.T) {
	p, _, sm := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	name := "http--smoke--20260702T153012Z--abc123.jsonl"
	path := filepath.Join(sm.SessionDir(), name)
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"sid-1","timestamp":"2026-07-02T15:30:12Z","cwd":"/home/wall-e"}`,
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-07-02T15:30:13Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-07-02T15:30:14Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"session_info","id":"i1","parentId":"m1","timestamp":"2026-07-02T15:30:15Z","name":"Smoke test"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	s := newServer(t, p, Config{Token: "sekret", Sessions: sm, Exporter: fakeExporter{}})

	rr := do(t, s, http.MethodGet, "/v1/sessions", "", "")
	if rr.Code != 200 {
		t.Fatalf("list status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Sessions []struct {
			Key          string `json:"key"`
			ChannelType  string `json:"channelType"`
			Datestamp    string `json:"datestamp"`
			Name         string `json:"name"`
			MessageCount int    `json:"messageCount"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %v, want 1", got.Sessions)
	}
	if got.Sessions[0].ChannelType != "http" || got.Sessions[0].Datestamp != "20260702T153012Z" || got.Sessions[0].Name != "Smoke test" || got.Sessions[0].MessageCount != 2 {
		t.Fatalf("session metadata = %+v", got.Sessions[0])
	}

	messagesPath := "/v1/sessions/" + got.Sessions[0].Key + "/messages"
	rr = do(t, s, http.MethodGet, messagesPath, "", "")
	if rr.Code != 200 {
		t.Fatalf("messages status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	var msgResp struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &msgResp); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(msgResp.Messages) != 2 || msgResp.Messages[0].Role != "user" || msgResp.Messages[0].Content != "hi" || msgResp.Messages[1].Role != "assistant" || msgResp.Messages[1].Content != "hello" {
		t.Fatalf("messages = %+v, want user hi and assistant hello", msgResp.Messages)
	}

	exportPath := "/v1/sessions/" + got.Sessions[0].Key + "/export.html"
	rr = do(t, s, http.MethodGet, exportPath, "", "")
	if rr.Code != 200 {
		t.Fatalf("export status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), "exported "+name) {
		t.Fatalf("export body = %q", rr.Body.String())
	}
}

func TestPrompt_NoToken_401(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "", `{"channelType":"http","channel":"c1","message":"hi"}`)
	if rr.Code != 401 {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestPrompt_WrongToken_401(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer wrong", `{"channelType":"http","channel":"c1","message":"hi"}`)
	if rr.Code != 401 {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestPrompt_WrongToken_ConstantTime(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "0123456789abcdef0123456789abcdef"})
	body := `{"channelType":"http","channel":"c1","message":"hi"}`

	// Two wrong tokens, both same length as the correct one, but differing at
	// different positions: wrongFirst differs at byte 0, wrongLast differs only
	// at the final byte. A non-constant-time byte-by-byte compare would return
	// instantly for wrongFirst and only after scanning all 32 bytes for
	// wrongLast. subtle.ConstantTimeCompare reads all bytes regardless, so the
	// two timings should be within jitter.
	wrongFirst := "Bearer X123456789abcdef0123456789abcdef"
	wrongLast := "Bearer 0123456789abcdef0123456789abcdeX"

	measure := func(auth string) time.Duration {
		const n = 200
		var total time.Duration
		for i := 0; i < n; i++ {
			req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
			req.Header.Set("Authorization", auth)
			rr := httptest.NewRecorder()
			start := time.Now()
			s.Handler().ServeHTTP(rr, req)
			total += time.Since(start)
			if rr.Code != 401 {
				t.Fatalf("expected 401 for wrong token, got %d", rr.Code)
			}
		}
		return total / n
	}
	tf := measure(wrongFirst)
	tl := measure(wrongLast)
	// Both should be 401 and fast; assert the early-differing token is NOT
	// dramatically faster than the late-differing one (which would indicate a
	// position-dependent short-circuit). subtle.ConstantTimeCompare reads all
	// bytes regardless, so tf and tl should be within scheduler jitter. A true
	// byte-by-byte leak would show wrongFirst finishing in ~1 byte compare vs
	// wrongLast in ~32, a >10x gap. Allow 5x as a generous ceiling.
	if tf > 0 && tl > 0 {
		ratio := float64(tl) / float64(tf)
		if ratio > 5 {
			t.Fatalf("constant-time suspected leak: wrongFirst=%v wrongLast=%v (late-differing %.1fx slower)", tf, tl, ratio)
		}
	}
}

func TestPrompt_NoBody_400(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer sekret", "")
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPrompt_MissingChannelType_400(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer sekret", `{"channel":"c1","message":"hi"}`)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPrompt_MissingChannel_400(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer sekret", `{"channelType":"http","message":"hi"}`)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPrompt_UnsupportedChannelType_400(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer sekret", `{"channelType":"telegram","channel":"123","message":"hi"}`)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPrompt_BodyTooLarge_413(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret", MaxPromptBytes: 32})
	rr := do(t, s, http.MethodPost, "/v1/prompt", "Bearer sekret", `{"channelType":"http","channel":"c1","message":"this is too long"}`)
	if rr.Code != 413 {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestPrompt_OK_StreamsSSE(t *testing.T) {
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl"}
	})
	s := newServer(t, p, Config{Token: "sekret"})

	body := `{"channelType":"http","channel":"c1","message":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sekret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	events := readSSE(t, rr.Body, 2*time.Second)
	names := sseNames(events)
	// Expect: agent_start, delta, delta, agent_end, done.
	want := []string{"agent_start", "delta", "delta", "agent_end", "done"}
	if !equalStringSlices(names, want) {
		t.Fatalf("event names = %v, want %v", names, want)
	}
	// Concatenate delta data.text → "Hello world".
	var text string
	for _, ev := range events {
		if ev.name != "delta" {
			continue
		}
		var d struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
			t.Fatalf("decode delta data %q: %v", ev.data, err)
		}
		text += d.Text
	}
	if text != "Hello world" {
		t.Fatalf("concatenated delta text = %q, want %q", text, "Hello world")
	}
}

func TestPrompt_SameChannelActiveTurn_Steers(t *testing.T) {
	streamDone := make(chan struct{})
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl", streamDone: streamDone}
	})
	s := newServer(t, p, Config{Token: "sekret", QueueTimeout: 5 * time.Second})

	body := `{"channelType":"http","channel":"c1","message":"hi"}`

	// First request: blocks in streamDone (agent_start emitted, agent_end
	// withheld) until we close streamDone.
	var firstErr error
	var firstEvents []sseEvent
	firstStart := time.Now()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sekret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		firstErr = httpError(rr.Code)
		firstEvents = readSSE(t, rr.Body, 2*time.Second)
	}()

	// Give the first request a moment to acquire + start streaming.
	time.Sleep(150 * time.Millisecond)

	// Second request to the same channel: should steer the active turn, not queue
	// a separate turn or 503.
	var secondErr error
	var secondEvents []sseEvent
	secondDone := make(chan struct{})
	secondStart := time.Now()
	go func() {
		defer close(secondDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sekret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		secondErr = httpError(rr.Code)
		secondEvents = readSSE(t, rr.Body, 2*time.Second)
	}()

	// Let the second request attach/steer for a bit, then finish the turn.
	time.Sleep(150 * time.Millisecond)
	close(streamDone)

	<-firstDone
	<-secondDone

	if firstErr != nil {
		t.Fatalf("first request error: %v", firstErr)
	}
	if secondErr != nil {
		t.Fatalf("second request error: %v", secondErr)
	}
	// The second request subscribed to the same active turn and should also see
	// completion after the shared stream ends.
	_ = firstStart
	_ = secondStart
	// Both should have streamed the shared turn.
	if got := sseNames(firstEvents); !contains(got, "done") {
		t.Fatalf("first events = %v, want a done", got)
	}
	if got := sseNames(secondEvents); !contains(got, "done") {
		t.Fatalf("second events = %v, want a done", got)
	}
}

func TestPrompt_BusyChannel_QueueTimeout_503(t *testing.T) {
	streamDone := make(chan struct{})
	p, _, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl", streamDone: streamDone}
	})
	s := newServer(t, p, Config{Token: "sekret", QueueTimeout: 100 * time.Millisecond})

	body := `{"channelType":"http","channel":"c1","message":"hi"}`

	// First request holds the slot (agent_end withheld).
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sekret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		readSSE(t, rr.Body, 2*time.Second) // drain
	}()

	time.Sleep(150 * time.Millisecond)

	// Second request to a different channel: pool capacity is exhausted, so the
	// acquire queue timeout should fire → 503.
	body2 := `{"channelType":"http","channel":"c2","message":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body2))
	req.Header.Set("Authorization", "Bearer sekret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["error"] != "channel busy" {
		t.Fatalf("body = %v, want error=channel busy", got)
	}

	// Release the first so the test can clean up.
	close(streamDone)
	<-firstDone
}

func TestPrompt_ClientDisconnect_DetachesWithoutAbort(t *testing.T) {
	streamDone := make(chan struct{})
	p, ff, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/s.jsonl", streamDone: streamDone}
	})
	s := newServer(t, p, Config{Token: "sekret", QueueTimeout: 5 * time.Second})

	body := `{"channelType":"http","channel":"c1","message":"hi"}`

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body)).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sekret")

	// httptest.NewRecorder implements http.Flusher; the handler streams SSE to
	// it and blocks on the turn subscription. Cancelling ctx simulates a client
	// disconnect; the handler should detach without aborting the shared turn.
	rr := httptest.NewRecorder()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.Handler().ServeHTTP(rr, req)
	}()

	// Wait for the prompt to reach the fake (agent_start emitted, agent_end
	// withheld).
	var fake *fakePI
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range ff.all() {
			fake = f
			break
		}
		if fake != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if fake == nil {
		t.Fatalf("no fake spawned")
	}
	if !fake.waitForCommand("prompt", 1*time.Second) {
		t.Fatalf("prompt never reached fake")
	}

	// Client disconnects.
	cancel()

	<-serverDone

	if fake.Contains("abort") {
		t.Fatalf("unexpected abort after client disconnect")
	}

	// Finish the still-running shared turn, then a new prompt to the same channel
	// should succeed promptly.
	close(streamDone)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/prompt", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer sekret")
	rr2 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr2, req2)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("second prompt after disconnect did not complete; slot not released")
	}
	if rr2.Code != 200 {
		t.Fatalf("second prompt status = %d, want 200", rr2.Code)
	}
}

// httpError returns a non-nil error for non-2xx, nil for 2xx.
func httpError(code int) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return errHTTP(code)
}

type errHTTP int

func (e errHTTP) Error() string { return "http error" }

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
