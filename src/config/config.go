// Package config consolidates ALL WALLE_* env parsing for the wall-e gateway
// into one place, so main.go stays thin and testable.
//
// A gateway-wide Config composes the four downstream configs:
//
//	rpc.Config      (pi process spawn flags)
//	session.Config  (transcript dir)
//	pool.Config     (worker pool sizing/drain)
//	httpapi.Config  (HTTP listen address, bearer token, queue timeout)
//
// plus a LogLevel string (parsed but routed to stdlib log for v1 — no logging
// dependency).
//
// Import direction: config imports httpapi/pool/session/rpc, never the reverse.
// This keeps the dependency graph acyclic. httpapi.ConfigFromEnv (Phase 4) is
// left in place as a thin per-package helper; config.Load builds the httpapi
// sub-config directly so there is no call into httpapi from config.
//
// All defaults match the plan (┬º4):
//
//	WALLE_TOKEN                  required
//	WALLE_PORT                   8080
//	WALLE_POOL_SIZE              4
//	WALLE_DRAIN_TIMEOUT          30s
//	WALLE_SESSION_DIR            /home/wall-e/sessions
//	WALLE_PI_BIN                 pi
//	WALLE_PROVIDER               (unset → pi settings default)
//	WALLE_MODEL                  (unset → pi settings default)
//	WALLE_CONFIRM_DEFAULT        true
//	WALLE_LOG_LEVEL              info
//	WALLE_HTTP_QUEUE_TIMEOUT     60s
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"wall-e/httpapi"
	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/session"
)

// ChatConfig is the gateway-wide chat front-end config (Phase 6+). It is
// defined here rather than in chat/ to keep config the single owner of env
// parsing; the values are copied into a chat.Config by main.
type ChatConfig struct {
	// Telegram holds the parsed WALLE_TELEGRAM_* values.
	Telegram TelegramConfig
}

// TelegramConfig holds the Telegram front-end settings.
type TelegramConfig struct {
	// Token is the bot token (empty → Telegram front-end disabled).
	Token string
	// AllowedChats is the optional chat-id allowlist (empty = allow all).
	AllowedChats []int64
}

// Default values. Exported so tests and main can reference them.
const (
	DefaultPort             = "8080"
	DefaultPoolSize         = 4
	DefaultDrainTimeout     = 30 * time.Second
	DefaultSessionDir       = "/home/wall-e/sessions"
	DefaultPiBin            = "pi"
	DefaultConfirmDefault   = true
	DefaultLogLevel         = "info"
	DefaultHTTPQueueTimeout = 60 * time.Second
)

// Config is the gateway-wide configuration assembled from WALLE_* env vars.
// It is a plain value type: every field is populated by Load, and every
// downstream subsystem reads only its own sub-config.
type Config struct {
	HTTP    httpapi.Config
	Pool    pool.Config
	Session session.Config
	RPC     rpc.Config

	// LogLevel is parsed from WALLE_LOG_LEVEL (default "info"). v1 routes it
	// to the stdlib log; the gateway itself doesn't filter on it yet, but the
	// value is validated so a typo is caught at startup rather than later.
	LogLevel string

	// SessionDir is echoed at the top level because it is also needed by the
	// container wiring (mkdir at startup) and by the Dockerfile, and pulling
	// it back out of session.Config is awkward.
	SessionDir string

	// Chat holds the optional chat front-end configs (Telegram, ...).
	Chat ChatConfig
}

// Load reads WALLE_* env vars and returns a fully-populated Config with all
// defaults applied, or an error describing the first invalid/missing value.
//
// Load does NOT mutate the environment. Callers (main) are responsible for
// any mkdir/chown of the session dir; Load only validates the string.
func Load() (Config, error) {
	var cfg Config
	var errs []string

	// --- WALLE_TOKEN (required) -----------------------------------------
	token := os.Getenv("WALLE_TOKEN")
	if token == "" {
		errs = append(errs, "WALLE_TOKEN is required")
	}

	// --- WALLE_PORT (default 8080) --------------------------------------
	port := os.Getenv("WALLE_PORT")
	if port == "" {
		port = DefaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		// A port must be a plain integer for our listen address; reject
		// "8080/tcp" etc. early with a clear message.
		errs = append(errs, fmt.Sprintf("invalid WALLE_PORT %q: must be an integer", port))
		port = DefaultPort
	}

	// --- WALLE_HTTP_QUEUE_TIMEOUT (default 60s) -------------------------
	httpQueueTimeout, err := parseDurationEnv("WALLE_HTTP_QUEUE_TIMEOUT", DefaultHTTPQueueTimeout)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg.HTTP = httpapi.Config{
		Token:        token,
		Addr:         ":" + port,
		QueueTimeout: httpQueueTimeout,
	}

	// --- WALLE_POOL_SIZE (default 4) ------------------------------------
	poolSize, err := parseIntEnv("WALLE_POOL_SIZE", DefaultPoolSize)
	if err != nil {
		errs = append(errs, err.Error())
	}
	if poolSize < 1 {
		errs = append(errs, fmt.Sprintf("invalid WALLE_POOL_SIZE %d: must be >= 1", poolSize))
		poolSize = DefaultPoolSize
	}

	// --- WALLE_DRAIN_TIMEOUT (default 30s) ------------------------------
	drainTimeout, err := parseDurationEnv("WALLE_DRAIN_TIMEOUT", DefaultDrainTimeout)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg.Pool = pool.Config{
		Size:        poolSize,
		DrainTimeout: drainTimeout,
		// RPCConfig, Sessions, NewClient are wired by main (Load doesn't know
		// how to build a *session.Manager; that requires MkdirAll which is a
		// side effect). main copies cfg.RPC / cfg.Session in after building
		// the manager.
	}

	// --- WALLE_SESSION_DIR (default /home/wall-e/sessions) --------------
	sessionDir := os.Getenv("WALLE_SESSION_DIR")
	if sessionDir == "" {
		sessionDir = DefaultSessionDir
	}
	cfg.Session = session.Config{SessionDir: sessionDir}
	cfg.SessionDir = sessionDir

	// --- WALLE_PI_BIN / WALLE_PROVIDER / WALLE_MODEL --------------------
	piBin := os.Getenv("WALLE_PI_BIN")
	if piBin == "" {
		piBin = DefaultPiBin
	}
	provider := os.Getenv("WALLE_PROVIDER")
	model := os.Getenv("WALLE_MODEL")

	// --- WALLE_CONFIRM_DEFAULT (default true) ---------------------------
	confirmDefault, err := parseBoolEnv("WALLE_CONFIRM_DEFAULT", DefaultConfirmDefault)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg.RPC = rpc.Config{
		PiBin:     piBin,
		Provider:  provider,
		Model:     model,
		SessionDir: sessionDir,
		UIPolicy:  rpc.ExtensionUIPolicy{ConfirmedDefault: confirmDefault},
	}

	// --- WALLE_LOG_LEVEL (default info) ---------------------------------
	logLevel := os.Getenv("WALLE_LOG_LEVEL")
	if logLevel == "" {
		logLevel = DefaultLogLevel
	}
	if !isValidLogLevel(logLevel) {
		errs = append(errs, fmt.Sprintf("invalid WALLE_LOG_LEVEL %q: must be one of debug/info/warn/error", logLevel))
	}
	cfg.LogLevel = logLevel

	// --- WALLE_TELEGRAM_TOKEN / WALLE_TELEGRAM_ALLOWED_CHATS -------------
	// Telegram is optional: if the token is unset, the front-end is skipped
	// (HTTP still serves). The allowlist is a comma-separated list of chat ids.
	cfg.Chat.Telegram.Token = os.Getenv("WALLE_TELEGRAM_TOKEN")
	allowedChats, err := parseInt64ListEnv("WALLE_TELEGRAM_ALLOWED_CHATS")
	if err != nil {
		errs = append(errs, err.Error())
	}
	cfg.Chat.Telegram.AllowedChats = allowedChats

	if len(errs) > 0 {
		return cfg, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// --- env parsing helpers --------------------------------------------------

// parseDurationEnv reads a duration-typed env var, applying def when unset.
func parseDurationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("invalid %s %q: %w", name, v, err)
	}
	if d <= 0 {
		return def, fmt.Errorf("invalid %s %q: must be positive", name, v)
	}
	return d, nil
}

// parseIntEnv reads an integer-typed env var, applying def when unset.
func parseIntEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("invalid %s %q: %w", name, v, err)
	}
	return n, nil
}

// parseBoolEnv reads a boolean-typed env var, applying def when unset. Accepts
// the common spellings (1/0, true/false, yes/no, on/off) case-insensitively so
// a typo doesn't silently flip the default.
func parseBoolEnv(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return def, fmt.Errorf("invalid %s %q: must be true/false", name, v)
}

func isValidLogLevel(s string) bool {
	switch strings.ToLower(s) {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

// parseInt64ListEnv reads a comma-separated list of int64 values (e.g.
// "123,-456,789"). An empty/unset value returns nil (no error) so the var is
// optional. Whitespace around each element is trimmed.
func parseInt64ListEnv(name string) ([]int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", name, v, err)
		}
		out = append(out, n)
	}
	return out, nil
}