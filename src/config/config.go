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
// Import direction: config imports httpapi/pool/session/rpc, never the reverse.
// This keeps the dependency graph acyclic. httpapi.ConfigFromEnv (Phase 4) is
// left in place as a thin per-package helper; config.Load builds the httpapi
// sub-config directly so there is no call into httpapi from config.
//
// All defaults match the plan (┬º4):
//
//	WALLE_TOKEN                  required
//	WALLE_PORT                   6007
//	WALLE_POOL_SIZE              4
//	WALLE_DRAIN_TIMEOUT          30s
//	WALLE_SESSION_DIR            /home/wall-e/sessions
//	WALLE_PROVIDER               (unset → pi settings default)
//	WALLE_MODEL                  (unset → pi settings default)
//	WALLE_HTTP_QUEUE_TIMEOUT     60s
//	WALLE_SITE                   /opt/wall-e/www
//	WALLE_SESSION_EXPORT_TIMEOUT 30s
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
	// Discord holds the parsed WALLE_DISCORD_* values.
	Discord DiscordConfig
}

// TelegramConfig holds the Telegram front-end settings.
type TelegramConfig struct {
	// Token is the bot token (empty → Telegram front-end disabled).
	Token string
	// AllowedChats is the optional chat-id allowlist (empty = allow all).
	AllowedChats []int64
}

// DiscordConfig holds the Discord front-end settings. Snowflakes remain
// decimal strings because they may exceed signed integer ranges.
type DiscordConfig struct {
	Token           string
	AllowedChannels []string
}

// Default values. Exported so tests and main can reference them.
const (
	DefaultPort                 = "6007"
	DefaultPoolSize             = 4
	DefaultDrainTimeout         = 30 * time.Second
	DefaultSessionDir           = "/home/wall-e/sessions"
	DefaultSystemPrompt         = "/opt/wall-e/SYSTEM.md"
	DefaultHTTPQueueTimeout     = 60 * time.Second
	DefaultSiteDir              = "/opt/wall-e/www"
	DefaultSessionExportTimeout = 30 * time.Second
)

// Config is the gateway-wide configuration assembled from WALLE_* env vars.
// It is a plain value type: every field is populated by Load, and every
// downstream subsystem reads only its own sub-config.
type Config struct {
	HTTP    httpapi.Config
	Pool    pool.Config
	Session session.Config
	RPC     rpc.Config

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

	// --- WALLE_PORT (default 6007) --------------------------------------
	port := os.Getenv("WALLE_PORT")
	if port == "" {
		port = DefaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		// A port must be a plain integer for our listen address; reject
		// "6007/tcp" etc. early with a clear message.
		errs = append(errs, fmt.Sprintf("invalid WALLE_PORT %q: must be an integer", port))
		port = DefaultPort
	}

	// --- WALLE_HTTP_QUEUE_TIMEOUT (default 60s) -------------------------
	httpQueueTimeout, err := parseDurationEnv("WALLE_HTTP_QUEUE_TIMEOUT", DefaultHTTPQueueTimeout)
	if err != nil {
		errs = append(errs, err.Error())
	}

	// --- WALLE_SITE (default /opt/wall-e/www) ---------------------------
	siteDir := os.Getenv("WALLE_SITE")
	if siteDir == "" {
		siteDir = DefaultSiteDir
	}

	// --- WALLE_SESSION_EXPORT_TIMEOUT (default 30s) ---------------------
	sessionExportTimeout, err := parseDurationEnv("WALLE_SESSION_EXPORT_TIMEOUT", DefaultSessionExportTimeout)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg.HTTP = httpapi.Config{
		Token:         token,
		Addr:          ":" + port,
		QueueTimeout:  httpQueueTimeout,
		SiteDir:       siteDir,
		ExportTimeout: sessionExportTimeout,
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
		Size:         poolSize,
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

	// --- WALLE_PROVIDER / WALLE_MODEL -----------------------------------
	cfg.RPC = rpc.Config{
		Provider:     os.Getenv("WALLE_PROVIDER"),
		Model:        os.Getenv("WALLE_MODEL"),
		SystemPrompt: DefaultSystemPrompt,
		SessionDir:   sessionDir,
		UIPolicy:     rpc.DefaultExtensionUIPolicy(),
	}

	// --- WALLE_TELEGRAM_TOKEN / WALLE_TELEGRAM_ALLOWED_CHATS -------------
	// Telegram is optional: if the token is unset, the front-end is skipped
	// (HTTP still serves). The allowlist is a comma-separated list of chat ids.
	cfg.Chat.Telegram.Token = os.Getenv("WALLE_TELEGRAM_TOKEN")
	allowedChats, err := parseInt64ListEnv("WALLE_TELEGRAM_ALLOWED_CHATS")
	if err != nil {
		errs = append(errs, err.Error())
	}
	cfg.Chat.Telegram.AllowedChats = allowedChats

	// --- WALLE_DISCORD_TOKEN / WALLE_DISCORD_ALLOWED_CHANNELS ------------
	cfg.Chat.Discord.Token = os.Getenv("WALLE_DISCORD_TOKEN")
	allowedChannels, err := parseSnowflakeListEnv("WALLE_DISCORD_ALLOWED_CHANNELS")
	if err != nil {
		errs = append(errs, err.Error())
	}
	cfg.Chat.Discord.AllowedChannels = allowedChannels

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

// parseInt64ListEnv reads a comma-separated list of int64 values (e.g.
// "123,-456,789"). An empty/unset value returns nil (no error) so the var is
// optional. Whitespace around each element is trimmed.
func parseSnowflakeListEnv(name string) ([]string, error) {
	v := os.Getenv(name)
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("invalid %s %q: snowflakes must be unsigned decimal strings", name, v)
			}
		}
		if !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

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
