// Command wall-e provides the gateway CLI. `wall-e run` loads configuration
// from WALLE_* env vars, wires the session manager, worker pool, chat front-ends,
// and HTTP server, then runs until SIGINT/SIGTERM. `wall-e msg <type:id>` is a
// small local client that reads stdin and posts to the gateway.
//
// main() is intentionally thin: the server wiring lives in run() so it is unit-
// testable. Tests build a config from t.Setenv, drive run() with a cancellable
// context, poll /health to confirm the server is up, then cancel and assert a
// clean return.
//
// Phase 5 of the gateway plan (archive/20260627--walle-gateway.md ┬º6).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"wall-e/chat"
	"wall-e/config"
	"wall-e/httpapi"
	"wall-e/media"
	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
	"wall-e/turn"
	"wall-e/version"
)

func main() {
	os.Exit(mainWithArgs(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type discordFrontend interface {
	chat.Frontend
	httpapi.PromptAdapter
	httpapi.SendAdapter
}

// newDiscordFrontend is a process-level seam for main wiring tests. Production
// always constructs the discordgo-backed adapter; tests replace it with a
// network-free fake.
var newDiscordFrontend = func(cfg chat.DiscordConfig, p *pool.Pool) (discordFrontend, error) {
	return chat.NewDiscord(cfg, p, nil)
}

func mainWithArgs(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-V") {
		fmt.Fprintf(stdout, "wall-e %s\n", version.String())
		return 0
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "wall-e run takes no arguments")
			return 2
		}
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "wall-e: config: %v\n", err)
			return 1
		}
		// Catch SIGINT/SIGTERM. On cancel, run() drains the HTTP server and the
		// worker pool and returns. (On Windows only SIGINT is deliverable, but
		// the gateway runs in a Linux container where Docker sends SIGTERM on
		// `docker stop`.)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := run(ctx, cfg); err != nil {
			fmt.Fprintf(stderr, "wall-e: %v\n", err)
			return 1
		}
		return 0
	case "msg":
		if err := runMsgCommand(context.Background(), args[1:], stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "wall-e msg: %v\n", err)
			return 1
		}
		return 0
	case "send":
		return runSendCommand(context.Background(), args[1:], stdin, stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  wall-e run
  wall-e msg <type:id> [--file PATH ...]
  wall-e send <type:id> [text]
  wall-e send --media <type:id> <filepath> [--caption "..."]
  wall-e --version | -V
  wall-e --help

Commands:
  run             start the gateway server
  msg <type:id>   read a prompt from stdin and submit it to /v1/prompt
  send <type:id>  send text/media directly to a channel via /v1/send

Environment for msg:
  WALLE_PORT         local gateway port (default 6007)
  WALLE_TOKEN        required bearer token
  WALLE_MSG_TIMEOUT  overall timeout (default 30m)
`)
}

const (
	cliMaxPromptBytes = 8 << 20
	// HTTP attachment SSE events may contain 32 MiB of base64-encoded media.
	cliMaxSSEEventBytes = 48 << 20
)

type cliPromptAttachment struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data"`
}

type cliPromptRequest struct {
	ChannelType string                 `json:"channelType"`
	Channel     string                 `json:"channel"`
	Message     string                 `json:"message"`
	Attachments *[]cliPromptAttachment `json:"attachments,omitempty"`
}

func runMsgCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	channelArg, fileArgs, err := parseMsgArgs(args)
	if err != nil {
		return err
	}
	channelType, channel, err := parseChannelAddress(channelArg)
	if err != nil {
		return err
	}
	prompt, err := readPromptStdin(stdin)
	if err != nil {
		return err
	}
	attachments, err := readCLIAttachments(fileArgs)
	if err != nil {
		return err
	}
	token := os.Getenv("WALLE_TOKEN")
	if token == "" {
		return errors.New("WALLE_TOKEN is required")
	}
	port := os.Getenv("WALLE_PORT")
	if port == "" {
		port = "6007"
	}
	baseURL := "http://127.0.0.1:" + port
	timeout := 30 * time.Minute
	if v := os.Getenv("WALLE_MSG_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid WALLE_MSG_TIMEOUT %q: %w", v, err)
		}
		if d <= 0 {
			return fmt.Errorf("invalid WALLE_MSG_TIMEOUT %q: must be positive", v)
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(cliPromptRequest{ChannelType: channelType, Channel: channel, Message: prompt, Attachments: attachmentPtr(attachments)})
	if err != nil {
		return err
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/prompt"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("gateway returned %s: %s", resp.Status, msg)
	}
	return consumeSSE(resp.Body, stdout)
}

type cliSendRequest struct {
	ChannelType string `json:"channelType"`
	Channel     string `json:"channel"`
	Text        string `json:"text,omitempty"`
	MediaPath   string `json:"mediaPath,omitempty"`
	Caption     string `json:"caption,omitempty"`
}

type cliSendStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func runSendCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) int {
	req, err := parseSendArgs(args, stdin)
	if err != nil {
		writeCLIStatus(stdout, false, err.Error())
		return 2
	}
	token := os.Getenv("WALLE_TOKEN")
	if token == "" {
		writeCLIStatus(stdout, false, "WALLE_TOKEN is required")
		return 1
	}
	port := os.Getenv("WALLE_PORT")
	if port == "" {
		port = "6007"
	}
	timeout := 30 * time.Minute
	if v := os.Getenv("WALLE_MSG_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeCLIStatus(stdout, false, fmt.Sprintf("invalid WALLE_MSG_TIMEOUT %q", v))
			return 1
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		writeCLIStatus(stdout, false, err.Error())
		return 1
	}
	url := "http://127.0.0.1:" + port + "/v1/send"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeCLIStatus(stdout, false, err.Error())
		return 1
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		writeCLIStatus(stdout, false, err.Error())
		return 1
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(bytes.TrimSpace(data)) == 0 {
		writeCLIStatus(stdout, false, resp.Status)
		return 1
	}
	_, _ = stdout.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		_, _ = io.WriteString(stdout, "\n")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}
	return 0
}

func parseSendArgs(args []string, stdin io.Reader) (cliSendRequest, error) {
	if len(args) == 0 {
		return cliSendRequest{}, errors.New("usage: wall-e send <type:id> [text]")
	}
	if args[0] == "--media" {
		if len(args) < 3 {
			return cliSendRequest{}, errors.New("usage: wall-e send --media <type:id> <filepath> [--caption ...]")
		}
		channelType, channel, err := parseChannelAddress(args[1])
		if err != nil {
			return cliSendRequest{}, err
		}
		path, err := filepath.Abs(args[2])
		if err != nil {
			return cliSendRequest{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return cliSendRequest{}, fmt.Errorf("media unavailable: %w", err)
		}
		if !info.Mode().IsRegular() {
			return cliSendRequest{}, errors.New("media path is not a regular file")
		}
		var caption string
		for i := 3; i < len(args); i++ {
			if args[i] != "--caption" || i+1 >= len(args) {
				return cliSendRequest{}, errors.New("usage: wall-e send --media <type:id> <filepath> [--caption ...]")
			}
			caption = args[i+1]
			i++
		}
		return cliSendRequest{ChannelType: channelType, Channel: channel, MediaPath: filepath.Clean(path), Caption: caption}, nil
	}
	channelType, channel, err := parseChannelAddress(args[0])
	if err != nil {
		return cliSendRequest{}, err
	}
	text := ""
	if len(args) > 1 {
		text = strings.Join(args[1:], " ")
	} else {
		data, err := io.ReadAll(io.LimitReader(stdin, cliMaxPromptBytes+1))
		if err != nil {
			return cliSendRequest{}, err
		}
		if len(data) > cliMaxPromptBytes {
			return cliSendRequest{}, fmt.Errorf("text too large (max %d bytes)", cliMaxPromptBytes)
		}
		text = string(data)
	}
	if strings.TrimSpace(text) == "" {
		return cliSendRequest{}, errors.New("empty text")
	}
	return cliSendRequest{ChannelType: channelType, Channel: channel, Text: text}, nil
}

func writeCLIStatus(w io.Writer, ok bool, msg string) {
	st := cliSendStatus{OK: ok}
	if !ok {
		st.Error = msg
	}
	b, _ := json.Marshal(st)
	_, _ = w.Write(append(b, '\n'))
}

func attachmentPtr(in []cliPromptAttachment) *[]cliPromptAttachment {
	if len(in) == 0 {
		return nil
	}
	return &in
}

func parseMsgArgs(args []string) (channel string, files []string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--file", "--media":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a path", args[i])
			}
			files = append(files, args[i+1])
			i++
		default:
			if channel != "" {
				return "", nil, errors.New("usage: wall-e msg <type:id> [--file PATH ...]")
			}
			channel = args[i]
		}
	}
	if channel == "" {
		return "", nil, errors.New("usage: wall-e msg <type:id> [--file PATH ...]")
	}
	return channel, files, nil
}

func readCLIAttachments(paths []string) ([]cliPromptAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]cliPromptAttachment, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read attachment %q: %w", p, err)
		}
		out = append(out, cliPromptAttachment{FileName: filepath.Base(p), Data: base64.StdEncoding.EncodeToString(data)})
	}
	return out, nil
}

func parseChannelAddress(s string) (channelType, channel string, err error) {
	channelType, channel, ok := strings.Cut(s, ":")
	if !ok || strings.TrimSpace(channelType) == "" || strings.TrimSpace(channel) == "" {
		return "", "", fmt.Errorf("invalid channel %q: expected <type:id>", s)
	}
	return strings.TrimSpace(channelType), strings.TrimSpace(channel), nil
}

func readPromptStdin(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, cliMaxPromptBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > cliMaxPromptBytes {
		return "", fmt.Errorf("prompt too large (max %d bytes)", cliMaxPromptBytes)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errors.New("empty prompt on stdin")
	}
	return string(data), nil
}

func consumeSSE(r io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), cliMaxSSEEventBytes)
	event := "message"
	var dataLines []string
	dispatch := func() (bool, error) {
		if event == "" && len(dataLines) == 0 {
			return false, nil
		}
		data := strings.Join(dataLines, "\n")
		switch event {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return false, fmt.Errorf("invalid delta event: %w", err)
			}
			if _, err := io.WriteString(out, d.Text); err != nil {
				return false, err
			}
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(data), &e)
			if e.Message == "" {
				e.Message = data
			}
			return false, fmt.Errorf("gateway stream error: %s", e.Message)
		case "done":
			return true, nil
		}
		return false, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			done, err := dispatch()
			if err != nil || done {
				return err
			}
			event = "message"
			dataLines = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field, value = line, ""
		} else if strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			event = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 || event != "message" {
		done, err := dispatch()
		if err != nil || done {
			return err
		}
	}
	return errors.New("gateway stream ended before done")
}

// chatCommandProvider discovers pi RPC slash commands once and shares the
// result across enabled chat front-ends. It deliberately does not consume a
// worker slot or bind to a user session.
func chatCommandProvider(base rpc.Config) func(context.Context) ([]rpc.Command, error) {
	var once sync.Once
	var commands []rpc.Command
	var discoveryErr error
	return func(ctx context.Context) ([]rpc.Command, error) {
		once.Do(func() {
			discoverCfg := base
			discoverCfg.SessionDir = ""
			discoverCfg.NoSession = true
			if discoverCfg.RequestTimeout == 0 {
				discoverCfg.RequestTimeout = 30 * time.Second
			}
			client, err := rpc.New(discoverCfg)
			if err != nil {
				discoveryErr = err
				return
			}
			drainDone := make(chan struct{})
			go func() {
				defer close(drainDone)
				for range client.Events() {
				}
			}()
			defer func() {
				_ = client.Close()
				<-drainDone
			}()
			commands, discoveryErr = client.ListCommands(ctx)
		})
		return commands, discoveryErr
	}
}

// run wires the gateway components and serves until ctx is cancelled (signal)
// or the HTTP server fails to bind/start. On cancel it drains in-flight
// requests and the worker pool concurrently, bounded by DrainTimeout + a
// small slack, then returns.
//
// Shutdown ordering note: the plan (┬º6 Phase 5) says "HTTP first, then pool".
// We run httpServer.Shutdown and pool.Shutdown CONCURRENTLY under one grace
// timeout: the HTTP listener closes immediately (no new connections), the pool
// aborts/drains in-flight streams so turn subscriptions finish, and both
// complete within the bound. This achieves graceful drain + clean exit within
// WALLE_DRAIN_TIMEOUT.
func run(ctx context.Context, cfg config.Config) error {
	log.SetPrefix("wall-e: ")
	log.Printf("starting: http=%s pool=%d session_dir=%s",
		cfg.HTTP.Addr, cfg.Pool.Size, cfg.SessionDir)

	// 1. Session manager + startup recovery (best-effort). An empty/corrupt
	//    dir just means no channels are known yet; they generate fresh paths
	//    on first sight.
	mgr, err := session.New(cfg.Session)
	if err != nil {
		return err
	}
	if err := mgr.RebuildFromDir(); err != nil {
		log.Printf("session: rebuild from dir: %v (continuing)", err)
	}

	// 2. Worker pool. Wire the session manager + RPC config in now; pool.New
	//    copies them onto its internal Config.
	cfg.Pool.Sessions = mgr
	cfg.Pool.RPCConfig = cfg.RPC
	p, err := pool.New(cfg.Pool)
	if err != nil {
		return err
	}
	turns := turn.NewManager(ctx, p)

	mediaStore := media.NewStore(cfg.SessionDir)

	// 5. Optional chat front-ends. Startup failures are non-fatal so HTTP and
	//    any other configured front-end remain available.
	var frontends []chat.Frontend
	commandProvider := chatCommandProvider(cfg.RPC)
	if cfg.Chat.Telegram.Token != "" {
		tb, err := chat.NewTelegram(chat.Config{
			Token:           cfg.Chat.Telegram.Token,
			AllowedChats:    cfg.Chat.Telegram.AllowedChats,
			CommandProvider: commandProvider,
			Turns:           turns,
			MediaStore:      mediaStore,
		}, p, nil)
		if err != nil {
			log.Printf("telegram: disabled: %v", err)
		} else if err := tb.Start(ctx); err != nil {
			log.Printf("telegram: start failed: %v (HTTP still serves)", err)
		} else {
			frontends = append(frontends, tb)
			if cfg.HTTP.PromptAdapters == nil {
				cfg.HTTP.PromptAdapters = make(map[string]httpapi.PromptAdapter)
			}
			cfg.HTTP.PromptAdapters["telegram"] = tb
			if cfg.HTTP.SendAdapters == nil {
				cfg.HTTP.SendAdapters = make(map[string]httpapi.SendAdapter)
			}
			cfg.HTTP.SendAdapters["telegram"] = tb
			log.Printf("telegram: front-end started")
		}
	} else {
		log.Printf("telegram: disabled (WALLE_TELEGRAM_TOKEN unset)")
	}

	if cfg.Chat.Discord.Token != "" {
		db, err := newDiscordFrontend(chat.DiscordConfig{
			Token:           cfg.Chat.Discord.Token,
			AllowedChannels: cfg.Chat.Discord.AllowedChannels,
			CommandProvider: commandProvider,
			Turns:           turns,
			MediaStore:      mediaStore,
		}, p)
		if err != nil {
			log.Printf("discord: disabled: %v", err)
		} else if err := db.Start(ctx); err != nil {
			log.Printf("discord: start failed: %v (HTTP still serves)", err)
		} else {
			frontends = append(frontends, db)
			if cfg.HTTP.PromptAdapters == nil {
				cfg.HTTP.PromptAdapters = make(map[string]httpapi.PromptAdapter)
			}
			cfg.HTTP.PromptAdapters["discord"] = db
			if cfg.HTTP.SendAdapters == nil {
				cfg.HTTP.SendAdapters = make(map[string]httpapi.SendAdapter)
			}
			cfg.HTTP.SendAdapters["discord"] = db
			log.Printf("discord: front-end started")
		}
	} else {
		log.Printf("discord: disabled (WALLE_DISCORD_TOKEN unset)")
	}

	// Ensure an early HTTP bind/serve failure cannot leave a connected chat
	// Gateway or worker process behind. The normal shutdown path clears this
	// flag after it drains the same components.
	needsFallbackCleanup := true
	defer func() {
		if !needsFallbackCleanup {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cfg.Pool.DrainTimeout+5*time.Second)
		defer cancel()
		var cleanupWG sync.WaitGroup
		for _, frontend := range frontends {
			cleanupWG.Add(1)
			go func(fe chat.Frontend) {
				defer cleanupWG.Done()
				_ = fe.Stop(cleanupCtx)
			}(frontend)
		}
		cleanupWG.Add(1)
		go func() {
			defer cleanupWG.Done()
			_ = p.Shutdown(cleanupCtx)
		}()
		cleanupWG.Wait()
	}()

	// 3. HTTP server. We own the *http.Server (rather than calling
	//    httpapi.Server.ListenAndServe) so we can Shutdown it gracefully and
	//    so the "listening" log fires only after the socket is bound. The
	//    httpapi.Server is just the handler + config holder here.
	cfg.HTTP.Sessions = mgr
	cfg.HTTP.RPCConfig = cfg.RPC
	cfg.HTTP.Turns = turns
	srv := httpapi.New(cfg.HTTP, p)
	listener, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return err
	}
	log.Printf("listening %s", listener.Addr())

	httpServer := &http.Server{
		Addr:    cfg.HTTP.Addr,
		Handler: srv.Handler(),
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()

	// 4. Wait for a signal (ctx cancel) or a serve failure (e.g. bind lost).
	select {
	case err := <-serveErr:
		// Serve returned on its own. ErrServerClosed means someone (not us
		// yet) shut it down — treat as clean.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Signal received: fall through to graceful shutdown.
	}

	log.Printf("shutting down (drain budget %s)", cfg.Pool.DrainTimeout)
	grace := cfg.Pool.DrainTimeout + 5*time.Second
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	// Drain HTTP + pool concurrently: the listener closes immediately (no new
	// requests), in-flight SSE handlers unblock once the pool aborts their
	// streaming agents, then both shutdowns complete.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := httpServer.Shutdown(shutCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := p.Shutdown(shutCtx); err != nil {
			log.Printf("pool shutdown: %v", err)
		}
	}()
	// Stop chat front-ends concurrently too: their Stop cancels the poll loop
	// and drains in-flight turns (bounded), which in turn Releases slots so the
	// pool/pool shutdown can complete.
	for _, fe := range frontends {
		wg.Add(1)
		go func(fe chat.Frontend) {
			defer wg.Done()
			if err := fe.Stop(shutCtx); err != nil {
				log.Printf("chat shutdown: %v", err)
			}
		}(fe)
	}
	wg.Wait()
	needsFallbackCleanup = false

	// Drain Serve's return value (ErrServerClosed after Shutdown).
	<-serveErr

	log.Printf("drained, exiting")
	return nil
}
