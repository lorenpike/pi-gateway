package session

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pathRe asserts the on-disk naming scheme basename:
// <channel-type>--<channel-id>--<YYYYMMDDTHHMMSSZ>--<uuid>.jsonl. We match on
// the basename only so the test is OS-agnostic about path separators.
var pathRe = regexp.MustCompile(`^[^/\\]+--[^/\\]+--\d{8}T\d{6}Z--[0-9a-zA-Z_-]+\.jsonl$`)

// newManager makes a Manager rooted at a per-test temp dir.
func newManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := New(Config{SessionDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// writeSessionFile writes a minimal file into m's SessionDir with the given
// basename (no content needed; RebuildFromDir only looks at names).
func writeSessionFile(t *testing.T, m *Manager, name string) {
	t.Helper()
	p := filepath.Join(m.SessionDir(), name)
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestManager_NewChannel_GeneratesPath: first time a channel is seen, Current
// returns a path with the timestamp/uuid shape (regex-asserted) and ok=false
// (it was generated, not recalled).
func TestManager_NewChannel_GeneratesPath(t *testing.T) {
	m := newManager(t)
	p, ok := m.Current(NewChannelID("discord", "123"))
	if ok {
		t.Fatalf("first Current() should be ok=false, got ok=true path=%q", p)
	}
	if !pathRe.MatchString(filepath.Base(p)) {
		t.Errorf("path %q basename does not match typed session filename", filepath.Base(p))
	}
	if !strings.HasSuffix(p, ".jsonl") {
		t.Errorf("path %q should end with .jsonl", p)
	}
	if !strings.HasPrefix(filepath.Base(p), "discord--123--") {
		t.Errorf("path %q should contain typed channel prefix", filepath.Base(p))
	}
}

// TestManager_Roundtrip: SetCurrent then Current returns the same path and
// ok=true.
func TestManager_Roundtrip(t *testing.T) {
	m := newManager(t)
	ch := NewChannelID("http", "client-1")
	const want = "/home/wall-e/sessions/http--client-1--20260702T153012Z--deadbeef.jsonl"
	// SetCurrent validates the path lives under SessionDir; for this test we
	// craft a path that does. The simplest way: a path directly under the
	// manager's dir.
	wantPath := filepath.Join(m.SessionDir(), "http--client-1--20260702T153012Z--deadbeef.jsonl")
	if err := m.SetCurrent(ch, wantPath); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	got, ok := m.Current(ch)
	if !ok {
		t.Fatalf("Current after SetCurrent should be ok=true")
	}
	if got != wantPath {
		t.Errorf("got %q, want %q", got, wantPath)
	}
	_ = want // keep the original intent visible
}

// TestManager_ResyncFromState: given a get_state result with sessionFile, the
// map is updated (used after new_session/clone/switch_session).
func TestManager_ResyncFromState(t *testing.T) {
	m := newManager(t)
	ch := NewChannelID("telegram", "42")

	// First, an initial generation.
	_, _ = m.Current(ch)

	// Now resync from a get_state result (absolute path under the session
	// dir, matching what pi would return).
	resyncPath := filepath.Join(m.SessionDir(), "telegram--42--20260702T153012Z--cafef00d.jsonl")
	if err := m.ResyncFromState(ch, resyncPath); err != nil {
		t.Fatalf("ResyncFromState: %v", err)
	}
	got, ok := m.Current(ch)
	if !ok {
		t.Fatalf("Current after resync should be ok=true")
	}
	if got != resyncPath {
		t.Errorf("got %q, want resynced %q", got, resyncPath)
	}
}

// TestManager_ListKnownChannels: returns all known channel ids, sorted.
func TestManager_ListKnownChannels(t *testing.T) {
	m := newManager(t)
	// Seed three channels out of order.
	for _, ch := range []ChannelID{NewChannelID("http", "c-c"), NewChannelID("http", "c-a"), NewChannelID("http", "c-b")} {
		_, id := ch.parts()
		if err := m.SetCurrent(ch, filepath.Join(m.SessionDir(), "http--"+id+"--20260702T153012Z--u.jsonl")); err != nil {
			t.Fatalf("SetCurrent(%s): %v", ch, err)
		}
	}
	got := m.ListKnownChannels()
	want := []ChannelID{NewChannelID("http", "c-a"), NewChannelID("http", "c-b"), NewChannelID("http", "c-c")}
	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d (%v)", len(got), len(want), got)
	}
	for i, ch := range got {
		if ch != want[i] {
			t.Errorf("got[%d]=%q, want %q (full=%v)", i, ch, want[i], got)
		}
	}
}

// TestManager_RebuildFromDir: given typed session files, Current returns the
// newest file for each typed channel.
func TestManager_RebuildFromDir(t *testing.T) {
	m := newManager(t)
	writeSessionFile(t, m, "http--chanA--20260702T153012Z--aaaa.jsonl")
	writeSessionFile(t, m, "http--chanA--20260702T153013Z--bbbb.jsonl")
	writeSessionFile(t, m, "telegram--chanB--20260702T153014Z--cccc.jsonl")
	// A junk file that does not match the scheme — must be ignored.
	writeSessionFile(t, m, "README.txt")
	writeSessionFile(t, m, "nope--notatimestamp--x.jsonl")

	if err := m.RebuildFromDir(); err != nil {
		t.Fatalf("RebuildFromDir: %v", err)
	}

	gotA, okA := m.Current(NewChannelID("http", "chanA"))
	if !okA {
		t.Fatalf("chanA not found after rebuild")
	}
	if !strings.HasSuffix(gotA, "http--chanA--20260702T153013Z--bbbb.jsonl") {
		t.Errorf("chanA current = %q, want newest file", gotA)
	}

	gotB, okB := m.Current(NewChannelID("telegram", "chanB"))
	if !okB {
		t.Fatalf("chanB not found after rebuild")
	}
	if !strings.HasSuffix(gotB, "telegram--chanB--20260702T153014Z--cccc.jsonl") {
		t.Errorf("chanB current = %q, want typed file", gotB)
	}

	// Channels never seen should still generate a fresh path.
	gotC, okC := m.Current(NewChannelID("http", "chanC-new"))
	if okC {
		t.Errorf("chanC-new should be generated (ok=false), got ok=true path=%q", gotC)
	}
}

// TestManager_RebuildFromDir_TiebreakByUUID: two files with identical
// timestamps pick the lexicographically larger uuid for determinism.
func TestManager_RebuildFromDir_TiebreakByUUID(t *testing.T) {
	m := newManager(t)
	writeSessionFile(t, m, "http--chan--20260702T153012Z--aaaa.jsonl")
	writeSessionFile(t, m, "http--chan--20260702T153012Z--zzzz.jsonl")
	if err := m.RebuildFromDir(); err != nil {
		t.Fatalf("RebuildFromDir: %v", err)
	}
	got, ok := m.Current(NewChannelID("http", "chan"))
	if !ok {
		t.Fatalf("chan not found")
	}
	if !strings.HasSuffix(got, "http--chan--20260702T153012Z--zzzz.jsonl") {
		t.Errorf("tiebreak should pick zzzz, got %q", got)
	}
}

// TestManager_RebuildReplacesState: RebuildFromDir replaces any in-memory map
// with the on-disk truth (a stale in-memory entry is dropped if no file
// exists, and a file the manager never saw is picked up).
func TestManager_RebuildReplacesState(t *testing.T) {
	m := newManager(t)
	// Stale in-memory only (no file on disk).
	if err := m.SetCurrent(NewChannelID("http", "ghost"), filepath.Join(m.SessionDir(), "http--ghost--20260702T153012Z--x.jsonl")); err != nil {
		t.Fatalf("SetCurrent(ghost): %v", err)
	}
	// Real file on disk the manager doesn't know about.
	writeSessionFile(t, m, "http--real--20260702T153012Z--y.jsonl")

	if err := m.RebuildFromDir(); err != nil {
		t.Fatalf("RebuildFromDir: %v", err)
	}
	if _, ok := m.Current(NewChannelID("http", "ghost")); ok {
		t.Errorf("ghost should be gone after rebuild (no file on disk)")
	}
	if _, ok := m.Current(NewChannelID("http", "real")); !ok {
		t.Errorf("real should be present after rebuild")
	}
}

// TestManager_SetCurrent_RejectsOutsideSessionDir: paths resolving outside
// SessionDir are rejected (the §8 risk mitigation).
func TestManager_SetCurrent_RejectsOutsideSessionDir(t *testing.T) {
	m := newManager(t)
	outside := filepath.Join(t.TempDir(), "escaped.jsonl")
	err := m.SetCurrent("bad", outside)
	if err != ErrPathOutsideSessionDir {
		t.Errorf("got err=%v, want ErrPathOutsideSessionDir", err)
	}
	if _, ok := m.Current("bad"); ok {
		t.Errorf("rejected SetCurrent should not have stored a path")
	}
}

// TestManager_SetCurrent_RejectsDotDot: a "../" escape in the path is rejected
// even if it starts under SessionDir.
func TestManager_SetCurrent_RejectsDotDot(t *testing.T) {
	m := newManager(t)
	escape := filepath.Join(m.SessionDir(), "..", "escaped.jsonl")
	err := m.SetCurrent("bad2", escape)
	if err != ErrPathOutsideSessionDir {
		t.Errorf("got err=%v, want ErrPathOutsideSessionDir", err)
	}
}

// TestManager_ResyncFromState_Empty: empty sessionFile is rejected (mirrors the
// contract used by the RPC client's GetState).
func TestManager_ResyncFromState_Empty(t *testing.T) {
	m := newManager(t)
	if err := m.ResyncFromState("ch", ""); err == nil {
		t.Errorf("expected error for empty sessionFile")
	}
}

// TestManager_NewSessionPath_Uniqueness: generating two paths for the same
// channel in a tight loop yields distinct uuids (so the second never collides
// with the first even within the same second).
func TestManager_NewSessionPath_Uniqueness(t *testing.T) {
	m := newManager(t)
	ch := NewChannelID("http", "ch")
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		p := m.NewSessionPath(ch)
		if seen[p] {
			t.Fatalf("duplicate path generated: %s", p)
		}
		seen[p] = true
	}
}

// TestManager_SanitizeChannelID: unsafe characters are replaced and the
// sanitized prefix never contains "--" (so the naming scheme's separator
// stays unambiguous during RebuildFromDir parsing).
func TestManager_SanitizeChannelID(t *testing.T) {
	cases := map[string]string{
		"simple":         "simple",
		"with/slash":     "with_slash",
		`back\slash`:     "back_slash",
		"col:on":         "col_on",
		"star*quest?":    "star_quest",
		`quot"d":lt<gt>`: "quot_d_lt_gt",
		"":               "_",
		".":              "_",
		"..":             "_",
	}
	for in, want := range cases {
		got := sanitizeChannelID(ChannelID(in))
		if got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
		// Invariant: no "--" survives sanitization (it would corrupt the
		// filename parse on rebuild). Note: the "--" separating parts is
		// added by the path builder, not by the sanitizer.
		if strings.Contains(got, "--") {
			t.Errorf("sanitized form %q contains '--' (corrupts rebuild parse)", got)
		}
	}
}

// TestManager_New_CreatesDir: New makes the session directory if missing.
func TestManager_New_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sessions")
	m, err := New(Config{SessionDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(m.SessionDir())
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("session dir is not a directory")
	}
}

// TestManager_New_EmptySessionDir: New rejects an empty SessionDir.
func TestManager_New_EmptySessionDir(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Errorf("expected error for empty SessionDir")
	}
}
