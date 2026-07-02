// Command wall-e is the gateway entrypoint. It loads configuration from WALLE_*
// env vars, wires the session manager, worker pool, and HTTP server, and runs
// until SIGINT/SIGTERM, then drains gracefully (HTTP connections + pi pool).
//
// main() is intentionally thin: the wiring lives in run() so it is unit-
// testable. A test builds a config from t.Setenv, drives run() with a
// cancellable context, polls /health to confirm the server is up, then cancels
// and asserts a clean return.
//
// Phase 5 of the gateway plan (archive/20260627--walle-gateway.md ┬º6).
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"wall-e/chat"
	"wall-e/config"
	"wall-e/httpapi"
	"wall-e/pool"
	"wall-e/session"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("wall-e: config: %v", err)
	}

	// Catch SIGINT/SIGTERM. On cancel, run() drains the HTTP server and the
	// worker pool and returns. (On Windows only SIGINT is deliverable, but
	// the gateway runs in a Linux container where Docker sends SIGTERM on
	// `docker stop`.)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("wall-e: %v", err)
	}
}

// run wires the gateway components and serves until ctx is cancelled (signal)
// or the HTTP server fails to bind/start. On cancel it drains in-flight
// requests and the worker pool concurrently, bounded by DrainTimeout + a
// small slack, then returns.
//
// Shutdown ordering note: the plan (┬º6 Phase 5) says "HTTP first, then pool".
// In practice the SSE handlers block on Slot.Events() waiting for agent_end,
// so http.Server.Shutdown cannot complete until the pool aborts the streaming
// agents (which produces the agent_end that unblocks the handlers). We
// therefore run httpServer.Shutdown and pool.Shutdown CONCURRENTLY under one
// grace timeout: the HTTP listener closes immediately (no new connections),
// the pool aborts in-flight streams so handlers return, and both complete
// within the bound. This achieves the plan's intent (graceful drain + clean
// exit within WALLE_DRAIN_TIMEOUT) and works with the current pool/handler
// design without modifying the green Phase 3/4 suites.
func run(ctx context.Context, cfg config.Config) error {
	log.SetPrefix("wall-e: ")
	log.Printf("starting: http=%s pool=%d session_dir=%s log=%s",
		cfg.HTTP.Addr, cfg.Pool.Size, cfg.SessionDir, cfg.LogLevel)

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

	// 3. HTTP server. We own the *http.Server (rather than calling
	//    httpapi.Server.ListenAndServe) so we can Shutdown it gracefully and
	//    so the "listening" log fires only after the socket is bound. The
	//    httpapi.Server is just the handler + config holder here.
	cfg.HTTP.Sessions = mgr
	cfg.HTTP.RPCConfig = cfg.RPC
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

	// 5. Optional chat front-ends. Telegram is started only if a bot token is
	//    configured; otherwise the gateway serves HTTP alone. A Start failure
	//    (e.g. bad token / network) is logged but non-fatal — HTTP still serves.
	var frontends []chat.Frontend
	if cfg.Chat.Telegram.Token != "" {
		tb, err := chat.NewTelegram(chat.Config{
			Token:        cfg.Chat.Telegram.Token,
			AllowedChats: cfg.Chat.Telegram.AllowedChats,
		}, p, nil)
		if err != nil {
			log.Printf("telegram: disabled: %v", err)
		} else if err := tb.Start(ctx); err != nil {
			log.Printf("telegram: start failed: %v (HTTP still serves)", err)
		} else {
			frontends = append(frontends, tb)
			log.Printf("telegram: front-end started")
		}
	} else {
		log.Printf("telegram: disabled (WALLE_TELEGRAM_TOKEN unset)")
	}

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

	// Drain Serve's return value (ErrServerClosed after Shutdown).
	<-serveErr

	log.Printf("drained, exiting")
	return nil
}
