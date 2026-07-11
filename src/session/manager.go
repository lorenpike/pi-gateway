// Package session owns the durable, stable mapping from a chat-platform
// channel (HTTP client channel, Telegram chat id, Discord channel id, …) to
// the current pi session transcript file path for that channel.
//
// Transcript filenames use the scheme
//
//	<channel-type>--<channel-id>--<YYYYMMDDTHHMMSSZ>--<uuid>.jsonl
//
// where channel-type and channel-id are sanitized filename components. The
// datestamp is UTC and lexicographically sortable. On startup the manager walks
// WALLE_SESSION_DIR, groups files by typed channel, and treats the newest file
// for each channel as current.
package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChannelID is the typed, platform-stable identifier for a channel. Construct
// values with NewChannelID so both components are normalized consistently for
// filenames and restart recovery.
type ChannelID string

// NewChannelID builds a typed channel id. Examples: NewChannelID("http",
// "smoke"), NewChannelID("telegram", "123456789").
func NewChannelID(channelType, channelID string) ChannelID {
	return ChannelID(sanitizeComponent(channelType) + "--" + sanitizeComponent(channelID))
}

func (ch ChannelID) parts() (channelType, channelID string) {
	parts := strings.SplitN(string(ch), "--", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	// Defensive fallback for tests/callers that still pass a raw string. New
	// code should use NewChannelID.
	return "unknown", sanitizeComponent(string(ch))
}

// Address returns the human-facing typed channel address, e.g.
// "telegram:123456789" or "http:morning-digest". Components are the sanitized
// components stored in the ChannelID; callers that need exact external ids
// should keep those ids separately.
func (ch ChannelID) Address() string {
	channelType, channelID := ch.parts()
	return channelType + ":" + channelID
}

// Config configures a Manager.
type Config struct {
	// SessionDir is the directory that holds all pi transcript files.
	SessionDir string
}

// Manager tracks the current session file path per typed channel id.
// All methods are safe for concurrent use.
type Manager struct {
	cfg Config

	mu      sync.RWMutex
	current map[ChannelID]string
}

// ErrPathOutsideSessionDir is returned by SetCurrent / ResyncFromState when
// the supplied path does not live under SessionDir.
var ErrPathOutsideSessionDir = errors.New("session: path is outside session dir")

const datestampLayout = "20060102T150405Z"

// SessionFile is metadata for one persisted pi session file. Path is omitted
// from JSON responses; Key is the opaque identifier used by HTTP export routes.
type SessionFile struct {
	Key          string    `json:"key"`
	ChannelType  string    `json:"channelType"`
	ChannelID    string    `json:"channelId"`
	Datestamp    string    `json:"datestamp"`
	CreatedAt    time.Time `json:"createdAt"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	SessionID    string    `json:"sessionId,omitempty"`
	Name         string    `json:"name,omitempty"`
	CWD          string    `json:"cwd,omitempty"`
	MessageCount int       `json:"messageCount"`
	Path         string    `json:"-"`
}

// TranscriptMessage is one chat-visible message read from a persisted pi
// transcript. Content is flattened to display text for lightweight clients.
type TranscriptMessage struct {
	ID        string `json:"id,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type parsedFilename struct {
	channelType string
	channelID   string
	datestamp   string
	createdAt   time.Time
	uuid        string
}

// New creates a Manager and ensures SessionDir exists. It does NOT rebuild
// from the directory; call RebuildFromDir explicitly at startup.
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
	return &Manager{cfg: cfg, current: make(map[ChannelID]string)}, nil
}

// SessionDir returns the absolute, cleaned session directory.
func (m *Manager) SessionDir() string { return m.cfg.SessionDir }

// Current returns the current session file path for ch. If the channel has
// never been seen, a fresh path is generated and stored, and ok=false.
func (m *Manager) Current(ch ChannelID) (path string, ok bool) {
	m.mu.RLock()
	p, found := m.current[ch]
	m.mu.RUnlock()
	if found {
		return p, true
	}
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
// typed naming scheme, without storing it as current.
func (m *Manager) NewSessionPath(ch ChannelID) string {
	channelType, channelID := ch.parts()
	name := fmt.Sprintf("%s--%s--%s--%s.jsonl", channelType, channelID, time.Now().UTC().Format(datestampLayout), newUUID())
	return filepath.Join(m.cfg.SessionDir, name)
}

// SetCurrent records path as the current session file for ch. The path must
// live under SessionDir.
func (m *Manager) SetCurrent(ch ChannelID, path string) error {
	if err := m.validatePath(path); err != nil {
		return err
	}
	m.mu.Lock()
	m.current[ch] = m.cleanPath(path)
	m.mu.Unlock()
	return nil
}

// CopySessionFile copies an existing session transcript to targetPath. Both
// paths must live under SessionDir. The target must not already exist.
func (m *Manager) CopySessionFile(sourcePath, targetPath string) error {
	if err := m.validateExistingFile(sourcePath); err != nil {
		return err
	}
	if err := m.validatePath(targetPath); err != nil {
		return err
	}
	src := m.cleanPath(sourcePath)
	dst := m.cleanPath(targetPath)
	if src == dst {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("session: open source session: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("session: create target session: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("session: copy session: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("session: close target session: %w", closeErr)
	}
	return nil
}

// RemoveSessionFile removes a session transcript under SessionDir. Missing
// files are ignored.
func (m *Manager) RemoveSessionFile(path string) error {
	if err := m.validatePath(path); err != nil {
		return err
	}
	if err := os.Remove(m.cleanPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: remove session: %w", err)
	}
	return nil
}

// ResyncFromState updates the current path for ch based on a get_state result.
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
// parsing typed session filenames. For each typed channel, the latest datestamp
// wins (ties broken by uuid lexicographically, for determinism). Files that do
// not match the typed naming scheme are ignored.
func (m *Manager) RebuildFromDir() error {
	entries, err := os.ReadDir(m.cfg.SessionDir)
	if err != nil {
		return fmt.Errorf("session: read session dir: %w", err)
	}

	type cand struct {
		path      string
		createdAt time.Time
		uuid      string
	}
	best := make(map[ChannelID]cand)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		pf, ok := parseSessionFilename(e.Name())
		if !ok {
			continue
		}
		ch := NewChannelID(pf.channelType, pf.channelID)
		full := filepath.Join(m.cfg.SessionDir, e.Name())
		cur, exists := best[ch]
		if !exists || pf.createdAt.After(cur.createdAt) || (pf.createdAt.Equal(cur.createdAt) && pf.uuid > cur.uuid) {
			best[ch] = cand{path: full, createdAt: pf.createdAt, uuid: pf.uuid}
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

// ListSessionFiles returns metadata for all typed session files under
// SessionDir, sorted by channelType then newest first.
func (m *Manager) ListSessionFiles() ([]SessionFile, error) {
	entries, err := os.ReadDir(m.cfg.SessionDir)
	if err != nil {
		return nil, fmt.Errorf("session: read session dir: %w", err)
	}
	out := make([]SessionFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		pf, ok := parseSessionFilename(e.Name())
		if !ok {
			continue
		}
		full := filepath.Join(m.cfg.SessionDir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		sf := SessionFile{
			Key:         sessionKey(e.Name()),
			ChannelType: pf.channelType,
			ChannelID:   pf.channelID,
			Datestamp:   pf.datestamp,
			CreatedAt:   pf.createdAt,
			ModifiedAt:  info.ModTime().UTC(),
			Path:        full,
		}
		m.readSessionMetadata(&sf)
		out = append(out, sf)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChannelType != out[j].ChannelType {
			return out[i].ChannelType < out[j].ChannelType
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// ResolveSessionKey resolves an opaque key from ListSessionFiles to metadata
// and a path under SessionDir.
func (m *Manager) ResolveSessionKey(key string) (SessionFile, bool, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return SessionFile{}, false, nil
	}
	sessions, err := m.ListSessionFiles()
	if err != nil {
		return SessionFile{}, false, err
	}
	for _, sf := range sessions {
		if sf.Key == key {
			return sf, true, nil
		}
	}
	return SessionFile{}, false, nil
}

// ReadTranscriptMessages reads chat-visible messages from an existing
// transcript file under SessionDir. It understands the common pi JSONL shape:
// {"type":"message","message":{"role":"user|assistant","content":...}}
// and flattens string/content-block payloads to text for web clients.
func (m *Manager) ReadTranscriptMessages(path string) ([]TranscriptMessage, error) {
	if err := m.validateExistingFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(m.cleanPath(path))
	if err != nil {
		return nil, fmt.Errorf("session: open transcript: %w", err)
	}
	defer f.Close()

	var out []TranscriptMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Type      string  `json:"type"`
			ID        string  `json:"id"`
			ParentID  *string `json:"parentId"`
			Timestamp string  `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.Type != "message" || (row.Message.Role != "user" && row.Message.Role != "assistant") {
			continue
		}
		msg := TranscriptMessage{
			ID:        row.ID,
			Timestamp: row.Timestamp,
			Role:      row.Message.Role,
			Content:   transcriptContentText(row.Message.Content),
		}
		if row.ParentID != nil {
			msg.ParentID = *row.ParentID
		}
		out = append(out, msg)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: scan transcript: %w", err)
	}
	return out, nil
}

func transcriptContentText(raw json.RawMessage) string {
	return transcriptContentTextDepth(raw, 0)
}

func transcriptContentTextDepth(raw json.RawMessage, depth int) string {
	if depth > 8 {
		return ""
	}
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if text := transcriptContentTextDepth(b, depth+1); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"text", "content"} {
		if v, ok := obj[k]; ok {
			if text := transcriptContentTextDepth(v, depth+1); text != "" {
				return text
			}
		}
	}
	var typ string
	_ = json.Unmarshal(obj["type"], &typ)
	switch typ {
	case "image", "input_image":
		return "[image]"
	case "tool_use", "tool_call":
		var name string
		_ = json.Unmarshal(obj["name"], &name)
		if name != "" {
			return "[tool: " + name + "]"
		}
		return "[tool]"
	case "tool_result":
		return "[tool result]"
	}
	return ""
}

func parseSessionFilename(name string) (parsedFilename, bool) {
	if !strings.HasSuffix(name, ".jsonl") {
		return parsedFilename{}, false
	}
	stem := strings.TrimSuffix(name, ".jsonl")
	parts := strings.Split(stem, "--")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return parsedFilename{}, false
	}
	createdAt, err := time.Parse(datestampLayout, parts[2])
	if err != nil {
		return parsedFilename{}, false
	}
	if !filenameSafeRe.MatchString(parts[0]) || !filenameSafeRe.MatchString(parts[1]) || !filenameSafeRe.MatchString(parts[3]) {
		return parsedFilename{}, false
	}
	return parsedFilename{channelType: parts[0], channelID: parts[1], datestamp: parts[2], createdAt: createdAt, uuid: parts[3]}, true
}

func sessionKey(filename string) string {
	sum := sha256.Sum256([]byte(filename))
	return hex.EncodeToString(sum[:16])
}

func (m *Manager) readSessionMetadata(sf *SessionFile) {
	f, err := os.Open(sf.Path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			continue
		}
		if first && head.Type == "session" {
			var h struct {
				ID  string `json:"id"`
				CWD string `json:"cwd"`
			}
			if err := json.Unmarshal([]byte(line), &h); err == nil {
				sf.SessionID = h.ID
				sf.CWD = h.CWD
			}
		}
		first = false
		switch head.Type {
		case "message":
			sf.MessageCount++
		case "session_info":
			var info struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(line), &info); err == nil {
				sf.Name = info.Name
			}
		}
	}
}

func (m *Manager) cleanPath(path string) string {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(m.cfg.SessionDir, clean)
	}
	return clean
}

func (m *Manager) validateExistingFile(path string) error {
	resolvedFile, err := filepath.EvalSymlinks(m.cleanPath(path))
	if err != nil {
		return fmt.Errorf("session: resolve session file: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(m.cfg.SessionDir)
	if err != nil {
		resolvedDir = filepath.Clean(m.cfg.SessionDir)
	}
	rel, err := filepath.Rel(resolvedDir, resolvedFile)
	if err != nil {
		return ErrPathOutsideSessionDir
	}
	if rel == "." || rel == "" {
		return nil
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return ErrPathOutsideSessionDir
	}
	return nil
}

// validatePath ensures path resolves to a location inside SessionDir.
func (m *Manager) validatePath(path string) error {
	clean := m.cleanPath(path)
	parent := filepath.Dir(clean)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
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
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return ErrPathOutsideSessionDir
	}
	return nil
}

var filenameSafeRe = regexp.MustCompile(`^[0-9A-Za-z_.-]+$`)
var multiUnderscoreRe = regexp.MustCompile(`_{2,}`)
var multiDashSepRe = regexp.MustCompile(`--+`)

// sanitizeChannelID is kept for older tests/callers; it sanitizes a channel id
// component. New code should use NewChannelID.
func sanitizeChannelID(ch ChannelID) string { return sanitizeComponent(string(ch)) }

func sanitizeComponent(s string) string {
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
		" ", "_",
	)
	s = repl.Replace(s)
	s = multiDashSepRe.ReplaceAllString(s, "_")
	s = multiUnderscoreRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

// newUUID returns 16 hex chars from crypto/rand.
func newUUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
