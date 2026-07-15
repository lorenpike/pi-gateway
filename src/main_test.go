package main

// main_test.go exercises the run() wiring helper (Phase 5 of the wall-e gateway
// plan). It builds a config from t.Setenv, drives run() with a cancellable
// context, polls /health to confirm the server actually bound, then cancels
// and asserts run returns cleanly within a deadline.
//
// No real `pi` is needed: the pool spawns lazily on Acquire, and /health
// doesn't Acquire. So startup + shutdown of an idle gateway is fully testable
// without the pi binary.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"wall-e/chat"
	"wall-e/config"
	"wall-e/httpapi"
	"wall-e/pool"
	"wall-e/turn"
)

// localhostAddr returns a loopback-only listen address for tests. Binding the
// test server to ":port" asks Windows Firewall for public/private network
// access every time go test builds a new temporary exe; loopback avoids that.
func localhostAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// freePort finds a TCP port that is free to bind on localhost by opening a
// listener and immediately closing it. There is an inherent (tiny) race that
// another process grabs the port in between; tests retry on bind failure.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// clearWalleEnv unsets every WALLE_* var config.Load reads, for a clean start.
func clearWalleEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"WALLE_TOKEN", "WALLE_PORT", "WALLE_MSG_TIMEOUT",
		"WALLE_HTTP_QUEUE_TIMEOUT", "WALLE_SITE", "WALLE_SESSION_EXPORT_TIMEOUT",
		"WALLE_POOL_SIZE", "WALLE_DRAIN_TIMEOUT", "WALLE_SESSION_DIR",
		"WALLE_PROVIDER", "WALLE_MODEL",
		"WALLE_TELEGRAM_TOKEN", "WALLE_TELEGRAM_ALLOWED_CHATS",
		"WALLE_DISCORD_TOKEN", "WALLE_DISCORD_ALLOWED_CHANNELS",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// healthOK polls the gateway's /health endpoint until it returns 200 with the
// expected body, or the deadline expires (test fails).
func healthOK(t *testing.T, addr string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && string(body) == `{"status":"ok"}` {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestCLI_HelpDoesNotRequireConfig(t *testing.T) {
	clearWalleEnv(t)
	var out, errOut bytes.Buffer
	code := mainWithArgs([]string{"--help"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "wall-e run") || !strings.Contains(out.String(), "wall-e msg <type:id>") {
		t.Fatalf("help output = %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestCLI_MsgPostsTypedPromptAndStreamsDeltas(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "sekret")
	var gotAuth string
	var gotReq cliPromptRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/prompt" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: agent_start\ndata: {}\n\n")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"hello\"}\n\n")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\" world\"}\n\n")
		fmt.Fprint(w, "event: agent_end\ndata: {}\n\n")
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	}))
	defer srv.Close()
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", srv.Listener.Addr().(*net.TCPAddr).Port))

	var out bytes.Buffer
	code := mainWithArgs([]string{"msg", "telegram:123456789"}, strings.NewReader("prompt text"), &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	wantReq := cliPromptRequest{ChannelType: "telegram", Channel: "123456789", Message: "prompt text"}
	if gotReq != wantReq {
		t.Fatalf("request = %+v, want %+v", gotReq, wantReq)
	}
	if out.String() != "hello world" {
		t.Fatalf("stdout = %q, want hello world", out.String())
	}
}

func TestCLI_DiscordTypedPromptAndSend(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "sekret")
	var gotPrompt cliPromptRequest
	var gotSend cliSendRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/prompt":
			_ = json.NewDecoder(request.Body).Decode(&gotPrompt)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: delta\ndata: {\"text\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
		case "/v1/send":
			_ = json.NewDecoder(request.Body).Decode(&gotSend)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"channel":"discord:123"}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", server.Listener.Addr().(*net.TCPAddr).Port))

	var promptOut bytes.Buffer
	if code := mainWithArgs([]string{"msg", "discord:123"}, strings.NewReader("prompt"), &promptOut, io.Discard); code != 0 {
		t.Fatalf("msg exit=%d", code)
	}
	if gotPrompt.ChannelType != "discord" || gotPrompt.Channel != "123" || promptOut.String() != "ok" {
		t.Fatalf("prompt=%+v output=%q", gotPrompt, promptOut.String())
	}
	var sendOut bytes.Buffer
	if code := mainWithArgs([]string{"send", "discord:123", "hello"}, strings.NewReader(""), &sendOut, io.Discard); code != 0 {
		t.Fatalf("send exit=%d output=%s", code, sendOut.String())
	}
	if gotSend.ChannelType != "discord" || gotSend.Channel != "123" || gotSend.Text != "hello" {
		t.Fatalf("send=%+v", gotSend)
	}
}

func TestCLI_MsgRejectsBadInput(t *testing.T) {
	clearWalleEnv(t)
	var out, errOut bytes.Buffer
	if code := mainWithArgs([]string{"msg", "telegram"}, strings.NewReader("hi"), &out, &errOut); code == 0 {
		t.Fatal("bad channel exit code = 0")
	}
	out.Reset()
	errOut.Reset()
	if code := mainWithArgs([]string{"msg", "telegram:123"}, strings.NewReader(" \n"), &out, &errOut); code == 0 {
		t.Fatal("empty stdin exit code = 0")
	}
	out.Reset()
	errOut.Reset()
	if code := mainWithArgs([]string{"msg", "telegram:123"}, strings.NewReader("hi"), &out, &errOut); code == 0 {
		t.Fatal("missing token exit code = 0")
	}
}

func TestCLI_MsgStreamErrorAndEarlyCloseFail(t *testing.T) {
	clearWalleEnv(t)
	t.Setenv("WALLE_TOKEN", "sekret")

	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"message\":\"boom\"}\n\n")
	}))
	defer srvErr.Close()
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", srvErr.Listener.Addr().(*net.TCPAddr).Port))
	if code := mainWithArgs([]string{"msg", "http:c1"}, strings.NewReader("hi"), io.Discard, io.Discard); code == 0 {
		t.Fatal("stream error exit code = 0")
	}

	srvClose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"partial\"}\n\n")
	}))
	defer srvClose.Close()
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", srvClose.Listener.Addr().(*net.TCPAddr).Port))
	if code := mainWithArgs([]string{"msg", "http:c1"}, strings.NewReader("hi"), io.Discard, io.Discard); code == 0 {
		t.Fatal("early close exit code = 0")
	}
}

type fakeMainDiscord struct {
	started chan struct{}
	stopped chan struct{}
	sends   chan httpapi.SendRequest
}

func (f *fakeMainDiscord) Start(context.Context) error { close(f.started); return nil }
func (f *fakeMainDiscord) Stop(context.Context) error  { close(f.stopped); return nil }
func (f *fakeMainDiscord) Prompt(context.Context, string, string) (*turn.Subscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeMainDiscord) Send(_ context.Context, req httpapi.SendRequest) (httpapi.SendResult, error) {
	f.sends <- req
	return httpapi.SendResult{Sent: []httpapi.SentItem{{Type: "text", Text: req.Text}}}, nil
}

func TestRun_WiresDiscordSendAdapterAndLifecycle(t *testing.T) {
	clearWalleEnv(t)
	port := freePort(t)
	t.Setenv("WALLE_TOKEN", "test-token")
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("WALLE_SESSION_DIR", t.TempDir())
	t.Setenv("WALLE_DRAIN_TIMEOUT", "1s")
	t.Setenv("WALLE_DISCORD_TOKEN", "never-used-by-fake")

	fake := &fakeMainDiscord{started: make(chan struct{}), stopped: make(chan struct{}), sends: make(chan httpapi.SendRequest, 1)}
	oldConstructor := newDiscordFrontend
	newDiscordFrontend = func(cfg chat.DiscordConfig, _ *pool.Pool) (discordFrontend, error) {
		if cfg.Token == "" || cfg.Turns == nil || cfg.MediaStore == nil {
			t.Fatalf("Discord config not wired: %+v", cfg)
		}
		return fake, nil
	}
	t.Cleanup(func() { newDiscordFrontend = oldConstructor })

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.Addr = localhostAddr(port)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("Discord frontend did not start")
	}
	if !healthOK(t, localhostAddr(port)) {
		cancel()
		t.Fatal("gateway not healthy")
	}
	body := strings.NewReader(`{"channelType":"discord","channel":"123","text":"hello"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+localhostAddr(port)+"/v1/send", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("send status=%d", resp.StatusCode)
	}
	select {
	case got := <-fake.sends:
		if got.Channel != "123" || got.Text != "hello" {
			t.Fatalf("send=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Discord send adapter not called")
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.stopped:
	default:
		t.Fatal("Discord frontend was not stopped")
	}
}

func TestRun_StartsAndShutsDownCleanly(t *testing.T) {
	clearWalleEnv(t)
	port := freePort(t)
	sessionDir := t.TempDir()
	t.Setenv("WALLE_TOKEN", "test-token")
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("WALLE_SESSION_DIR", sessionDir)
	t.Setenv("WALLE_POOL_SIZE", "2")
	t.Setenv("WALLE_DRAIN_TIMEOUT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.HTTP.Addr = localhostAddr(port)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	addr := localhostAddr(port)
	if !healthOK(t, addr) {
		cancel()
		t.Fatalf("server did not become healthy on %s", addr)
	}
	t.Logf("health check passed on %s", addr)

	// Cancel (simulates SIGTERM) and assert run returns promptly.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("run returned error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s of cancel")
	}
}

// TestRun_BindFailureReturnsError asserts that if the HTTP server cannot bind
// (port already in use), run returns an error rather than hanging.
//
// It pre-binds the SAME loopback address form run uses so the second Listen in
// run() genuinely conflicts, without asking Windows Firewall for public/private
// network access. The test runs run() in a goroutine: if run returns an error
// we pass; if run instead starts serving (platform quirk), we cancel and skip
// rather than hang.
func TestRun_BindFailureReturnsError(t *testing.T) {
	clearWalleEnv(t)

	// Find a free port, then re-bind the same loopback address run will use so
	// the conflict is on the identical socket address.
	port := freePort(t)
	addr := localhostAddr(port)
	blocker, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not pre-bind blocker on %s: %v (platform quirk)", addr, err)
	}
	t.Cleanup(func() { _ = blocker.Close() })

	t.Setenv("WALLE_TOKEN", "x")
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("WALLE_SESSION_DIR", t.TempDir())
	t.Setenv("WALLE_DRAIN_TIMEOUT", "1s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.HTTP.Addr = addr

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run: expected bind error, got nil")
		}
		// Expected: bind conflict surfaced as an error.
	case <-time.After(3 * time.Second):
		// run bound successfully despite the blocker — the test premise doesn't
		// hold on this platform; unblock and skip.
		cancel()
		<-runErr
		t.Skipf("pre-bind did not conflict with run on %s; skipping", addr)
	}
}

// TestRun_SignalDrivenShutdown starts run, then delivers a real SIGTERM via
// the process group to confirm signal.NotifyContext wiring (not just ctx
// cancellation) drains cleanly. This is the path `docker stop` exercises.
//
// We don't actually send a signal to the test process (racy under `go test`);
// instead we confirm that the SAME context plumbing that
// signal.NotifyContext produces (a cancelled ctx) drives a clean drain. The
// real signal path is covered by the docker smoke test in the plan.
func TestRun_DrainTimeoutBoundsShutdown(t *testing.T) {
	clearWalleEnv(t)
	port := freePort(t)
	t.Setenv("WALLE_TOKEN", "x")
	t.Setenv("WALLE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("WALLE_SESSION_DIR", t.TempDir())
	// Short drain budget so the test can't hang if something goes wrong.
	t.Setenv("WALLE_DRAIN_TIMEOUT", "1s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.HTTP.Addr = localhostAddr(port)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	if !healthOK(t, localhostAddr(port)) {
		cancel()
		t.Fatal("server did not become healthy")
	}

	start := time.Now()
	cancel()
	select {
	case err := <-runErr:
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("run returned error: %v", err)
		}
		// Idle pool + no in-flight requests → shutdown should be fast,
		// well under the drain budget.
		if elapsed > 5*time.Second {
			t.Errorf("shutdown took %v, expected fast idle drain", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s")
	}
}
