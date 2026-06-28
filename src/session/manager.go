// Package session owns the durable, stable mapping from a chat-platform
// channel id (Discord channel id, Telegram chat id, HTTP client id, …) to the
// *current* pi session transcript file path for that channel.
//
// The map is intentionally NOT persisted to a sidecar file in v1. Instead it is
// rebuilt lazily from the session directory itself: every transcript filename
// follows the scheme
//
//	<channelId>--<unixSeconds>--<uuid>.jsonl
//
// so on startup the manager walks WALLE_SESSION_DIR, groups files by their
// channel-id prefix, and treats the highest timestamp for each channel as the
// "current" session for that channel. This makes restarts robust to the
// gateway dying mid-turn: the newest file on disk is always the source of
// truth, regardless of whether the in-memory map was up to date.
//
// Channel ids from chat platforms are already unique but not necessarily
// filesystem-safe (e.g. they may contain '/' on HTTP clients). They are
// sanitized (see sanitizeChannelID) before being used as a filename prefix,
// so the on-disk prefix may differ from the logical channel id. The manager
// keeps the logical id as the map key and only sanitizes for filename
// construction / rebuild parsing.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChannelID is the logical, platform-stable identifier for a channel. It is
// used verbatim as the map key; only filename construction sanitizes it.
type ChannelID string

// Config configures a Manager.
type Config struct {
	// SessionDir is the directory that holds all pi transcript files. It is
	// the root used both for generating new paths and for rebuilding the map
	// on startup. Must be non-empty and absolute or resolvable against the
	// process working directory.
	SessionDir string
}

// Manager tracks the current session file path per channel id.
//
// All methods are safe for concurrent use. The map is the source of truth at
// runtime; the session directory is the source of truth across restarts (via
// RebuildFromDir).
type Manager struct {
	cfg Config

	mu      sync.RWMutex
	current map[ChannelID]string
}

// ErrPathOutsideSessionDir is returned by SetCurrent / ResyncFromState when
// the supplied path does not live under SessionDir. Per the plan's risk note
// (§8), switch_session targets are constrained to live under the session dir
// so the rebuild-on-startup invariant holds.
var ErrPathOutsideSessionDir = errors.New("session: path is outside session dir")

// filenameRe matches the on-disk transcript naming scheme. The channel prefix
// is greedy up to the last "--" so that sanitized channel ids containing
// underscores etc. still parse. The timestamp is decimal seconds; the uuid is
// any non-empty run of filename-safe chars. Note the separator between ts and
// uuid is also "--" (double dash), matching NewSessionPath.
var filenameRe = regexp.MustCompile(`^(.*)--(\d+)--([0-9a-zA-Z_-]+)\.jsonl$`)

// New creates a Manager and ensures SessionDir exists. It does NOT rebuild
// from the directory; call RebuildFromDir explicitly (usually once at startup
// after env parsing).
func New(cfg Config) (*Manager, error) {
	if cfg.SessionDir == "" {
		return nil, errors.New("session: SessionDir is required")
	}
	abs, err := filepath.Abs(cfg.SessionDir)
	if err != nil {
		return nil, fmt.Errorf("session: resolve session dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("session: create session dir: %w", err)
	}
	cfg.SessionDir = abs
	return &Manager{
		cfg:     cfg,
		current: make(map[ChannelID]string),
	}, nil
}

// SessionDir returns the absolute, cleaned session directory.
func (m *Manager) SessionDir() string { return m.cfg.SessionDir }

// Current returns the current session file path for ch. If the channel is
// already known, its stored path is returned with ok=true. If the channel has
// never been seen, a fresh path is generated (and stored) following the
// naming scheme; ok=false signals it was newly generated rather than recalled.
//
// Generating a path here does NOT create the file on disk — pi creates the
// file when it writes the first transcript line. The manager only owns the
// *name*.
func (m *Manager) Current(ch ChannelID) (path string, ok bool) {
	m.mu.RLock()
	p, found := m.current[ch]
	m.mu.RUnlock()
	if found {
		return p, true
	}
	// Upgrade to write lock to generate. Re-check under the write lock to
	// avoid a race with a concurrent Current() for the same channel.
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, found := m.current[ch]; found {
		return p, true
	}
	p = m.NewSessionPath(ch)
	m.current[ch] = p
	return p, false
}

// NewSessionPath returns a brand-new transcript path for ch following the
// naming scheme, without storing it as current. Useful when the caller
// explicitly wants a new session (and will SetCurrent/ResyncFromState after
// pi acknowledges it).
func (m *Manager) NewSessionPath(ch ChannelID) string {
	prefix := sanitizeChannelID(ch)
	ts := time.Now().Unix()
	uuid := newUUID()
	name := fmt.Sprintf("%s--%d--%s.jsonl", prefix, ts, uuid)
	return filepath.Join(m.cfg.SessionDir, name)
}

// SetCurrent records path as the current session file for ch. The path must
// live under SessionDir (after cleaning) or ErrPathOutsideSessionDir is
// returned and the map is left untouched.
func (m *Manager) SetCurrent(ch ChannelID, path string) error {
	if err := m.validatePath(path); err != nil {
		return err
	}
	m.mu.Lock()
	m.current[ch] = path
	m.mu.Unlock()
	return nil
}

// ResyncFromState updates the current path for ch based on a get_state result
// (the sessionFile field). It is the post-new_session/clone/switch_session
// hook used by the RPC client. The path is validated the same way as
// SetCurrent.
func (m *Manager) ResyncFromState(ch ChannelID, sessionFile string) error {
	if sessionFile == "" {
		return errors.New("session: empty sessionFile from get_state")
	}
	return m.SetCurrent(ch, sessionFile)
}

// ListKnownChannels returns all channel ids the manager currently has a path
// for. The order is sorted for stable output.
func (m *Manager) ListKnownChannels() []ChannelID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ChannelID, 0, len(m.current))
	for ch := range m.current {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RebuildFromDir walks SessionDir and reconstructs the channel→path map by
// parsing filenames. For each channel, the file with the highest unix-seconds
// timestamp wins (ties broken by uuid lexicographically, for determinism).
// Files that do not match the naming scheme are ignored.
//
// This replaces any in-memory state: it is the startup recovery path. Runtime
// mutations (SetCurrent/ResyncFromState) continue to update the map afterward.
func (m *Manager) RebuildFromDir() error {
	entries, err := os.ReadDir(m.cfg.SessionDir)
	if err != nil {
		return fmt.Errorf("session: read session dir: %w", err)
	}

	type cand struct {
		path string
		ts   int64
		uuid string
	}
	best := make(map[ChannelID]cand)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		match := filenameRe.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		prefix := match[1]
		ts, err := parseUnixSeconds(match[2])
		if err != nil {
			continue
		}
		uuid := match[3]
		ch := ChannelID(prefix) // prefix is already the sanitized form
		full := filepath.Join(m.cfg.SessionDir, name)

		cur, exists := best[ch]
		if !exists || ts > cur.ts || (ts == cur.ts && uuid > cur.uuid) {
			best[ch] = cand{path: full, ts: ts, uuid: uuid}
		}
	}

	next := make(map[ChannelID]string, len(best))
	for ch, c := range best {
		next[ch] = c.path
	}

	m.mu.Lock()
	m.current = next
	m.mu.Unlock()
	return nil
}

// validatePath ensures path resolves to a location inside SessionDir. It
// cleans and evaluates symlinks on the parent so "../" escapes and symlinks
// pointing outside are rejected.
func (m *Manager) validatePath(path string) error {
	clean := filepath.Clean(path)
	// Require an absolute path under the (absolute) SessionDir. If a relative
	// path is supplied, resolve it against SessionDir first (callers from the
	// RPC layer always send absolute paths from get_state, but be lenient).
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(m.cfg.SessionDir, clean)
	}
	// Resolve the parent directory (the file itself may not exist yet) to
	// catch symlink escapes.
	parent := filepath.Dir(clean)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		// Parent may not exist yet (new session file). Fall back to cleaning
		// the parent without symlink evaluation and check prefix-wise.
		resolvedParent = filepath.Clean(parent)
	}
	resolvedDir, err := filepath.EvalSymlinks(m.cfg.SessionDir)
	if err != nil {
		resolvedDir = filepath.Clean(m.cfg.SessionDir)
	}
	rel, err := filepath.Rel(resolvedDir, resolvedParent)
	if err != nil {
		return ErrPathOutsideSessionDir
	}
	if rel == "." || rel == "" {
		return nil
	}
	if strings.HasPrefix(rel, "..") {
		return ErrPathOutsideSessionDir
	}
	// Also reject Windows drive-prefixed or rooted escapes.
	if filepath.IsAbs(rel) {
		return ErrPathOutsideSessionDir
	}
	return nil
}

// sanitizeChannelID replaces characters that are unsafe as a filename
// component with '_'. This keeps the on-disk prefix usable across platforms
// (Linux rejects '/', Windows also rejects a handful of others) while
// preserving enough of the original id to be recognizable.
//
// The sanitized form is what gets written to disk and parsed back during
// RebuildFromDir; the logical ChannelID (map key) is left untouched.
func sanitizeChannelID(ch ChannelID) string {
	s := string(ch)
	repl := strings.NewReplacer(
		string(os.PathSeparator), "_",
		"/", "_",
		`\`, "_",
		":", "_",
		"*", "_",
		"?", "_",
		`"`, "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	s = repl.Replace(s)
	// Collapse runs of underscores for tidiness and to keep the "--"
	// separator unambiguous (a sanitized id cannot contain a literal "--"
	// that came from an unsafe char).
	s = collapseUnderscores(s)
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

var multiUnderscoreRe = regexp.MustCompile(`_{2,}`)

func collapseUnderscores(s string) string {
	return multiUnderscoreRe.ReplaceAllString(s, "_")
}

// parseUnixSeconds parses a non-negative decimal unix timestamp.
func parseUnixSeconds(s string) (int64, error) {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("session: bad timestamp %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	if len(s) == 0 {
		return 0, errors.New("session: empty timestamp")
	}
	return n, nil
}

// newUUID returns 16 hex chars from crypto/rand. It is not a strict RFC-4122
// UUID; it only needs to be unique within a (channel, second) bucket to
// disambiguate two sessions created in the same second.
func newUUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail in practice; fall back to time-based
		// entropy so generation never blocks.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
