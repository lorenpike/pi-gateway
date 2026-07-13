package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		`../photo.jpg`:            "photo.jpg",
		`C:\\tmp\\evil name?.png`: "evil_name.png",
		`.env`:                    "env",
		"bad\x00name;rm -rf.pdf":  "badname_rm_rf.pdf",
		"":                        "file",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Fatalf("SanitizeFilename(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSaveCollision(t *testing.T) {
	dir := t.TempDir()
	st := &Store{Dir: dir, Now: func() time.Time { return time.Date(2026, 7, 13, 18, 45, 12, 0, time.UTC) }}
	one, err := st.Save(context.Background(), "photo.jpg", strings.NewReader("one"))
	if err != nil {
		t.Fatalf("save one: %v", err)
	}
	two, err := st.Save(context.Background(), "photo.jpg", strings.NewReader("two"))
	if err != nil {
		t.Fatalf("save two: %v", err)
	}
	if filepath.Base(one.Path) != "20260713T184512Z--photo.jpg" {
		t.Fatalf("one = %s", one.Path)
	}
	if filepath.Base(two.Path) != "20260713T184512Z--photo--2.jpg" {
		t.Fatalf("two = %s", two.Path)
	}
	if b, _ := os.ReadFile(two.Path); string(b) != "two" {
		t.Fatalf("second file contents = %q", b)
	}
}

func TestFormatAttachmentPrompt(t *testing.T) {
	files := []SavedFile{{OriginalName: "photo.jpg", Path: "/home/wall-e/sessions/media/20260713T184512Z--photo.jpg"}}
	got := FormatAttachmentPrompt("Can you look at this?\n", files)
	want := "Can you look at this?\n\nAttached file: [photo.jpg](/home/wall-e/sessions/media/20260713T184512Z--photo.jpg)"
	if got != want {
		t.Fatalf("prompt = %q want %q", got, want)
	}
	got = FormatAttachmentPrompt("", files)
	if !strings.HasPrefix(got, "The user attached file(s):\n\nAttached file: [photo.jpg]") {
		t.Fatalf("attachment-only prompt = %q", got)
	}
}
