// Package media stores inbound channel attachments as local files and formats
// file-first prompts for pi. It is deliberately channel-neutral: HTTP,
// Telegram, and future adapters save bytes here, then submit normal text links.
package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// SavedFile describes one inbound attachment saved under a session media dir.
type SavedFile struct {
	OriginalName string
	StoredName   string
	Path         string
	MimeType     string
	Size         int64
}

// Store saves files under Dir, typically ${WALLE_SESSION_DIR}/media.
type Store struct {
	Dir string
	// Now is optional and exists for deterministic tests. If nil, time.Now is used.
	Now func() time.Time
}

const datestampLayout = "20060102T150405Z"
const maxSafeFilenameLen = 120

var unsafeNameRunRe = regexp.MustCompile(`[_-]{2,}`)

// NewStore returns a Store rooted at ${sessionDir}/media.
func NewStore(sessionDir string) *Store {
	return &Store{Dir: filepath.Join(sessionDir, "media")}
}

// SanitizeFilename converts an untrusted user/platform filename into a single
// conservative filename component. It strips path components, leading dots,
// control chars, and shell-hostile punctuation while preserving extensions when
// practical.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, `\\`, "/")
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	name = strings.TrimLeft(name, ".")
	if name == "" || name == "." || name == ".." {
		name = "file"
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || unicode.IsControl(r):
			// Drop controls entirely.
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := unsafeNameRunRe.ReplaceAllString(b.String(), "_")
	ext := filepath.Ext(out)
	stem := strings.TrimSuffix(out, ext)
	stem = strings.Trim(stem, "._-")
	if stem == "" {
		stem = "file"
	}
	out = stem + ext
	if len(out) <= maxSafeFilenameLen {
		return out
	}
	ext = filepath.Ext(out)
	stem = strings.TrimSuffix(out, ext)
	maxStem := maxSafeFilenameLen - len(ext)
	if maxStem < 1 {
		return out[:maxSafeFilenameLen]
	}
	if len(stem) > maxStem {
		stem = stem[:maxStem]
		stem = strings.Trim(stem, "._-")
		if stem == "" {
			stem = "file"
		}
	}
	return stem + ext
}

// Save copies r to a collision-safe file under s.Dir and returns metadata.
func (s *Store) Save(ctx context.Context, originalName string, r io.Reader) (SavedFile, error) {
	if s == nil || strings.TrimSpace(s.Dir) == "" {
		return SavedFile{}, errors.New("media: store dir is required")
	}
	if err := ctx.Err(); err != nil {
		return SavedFile{}, err
	}
	if r == nil {
		return SavedFile{}, errors.New("media: reader is nil")
	}
	abs, err := filepath.Abs(s.Dir)
	if err != nil {
		return SavedFile{}, fmt.Errorf("media: resolve dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return SavedFile{}, fmt.Errorf("media: create dir: %w", err)
	}

	safe := SanitizeFilename(originalName)
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	prefix := now().UTC().Format(datestampLayout) + "--"
	stored, path, out, err := createCollisionSafe(abs, prefix, safe)
	if err != nil {
		return SavedFile{}, err
	}
	defer out.Close()

	br := bufio.NewReader(r)
	peek, _ := br.Peek(512)
	mimeType := http.DetectContentType(peek)
	n, copyErr := io.Copy(out, br)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if copyErr != nil {
			return SavedFile{}, fmt.Errorf("media: save file: %w", copyErr)
		}
		return SavedFile{}, fmt.Errorf("media: close file: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(path)
		return SavedFile{}, err
	}
	return SavedFile{OriginalName: safe, StoredName: stored, Path: path, MimeType: mimeType, Size: n}, nil
}

func createCollisionSafe(dir, prefix, safe string) (stored, path string, f *os.File, err error) {
	ext := filepath.Ext(safe)
	stem := strings.TrimSuffix(safe, ext)
	if stem == "" {
		stem = "file"
	}
	for i := 1; i < 10000; i++ {
		name := safe
		if i > 1 {
			name = fmt.Sprintf("%s--%d%s", stem, i, ext)
		}
		stored = prefix + name
		path = filepath.Join(dir, stored)
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return stored, path, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", nil, fmt.Errorf("media: create file: %w", err)
		}
	}
	return "", "", nil, errors.New("media: too many filename collisions")
}

// FormatAttachmentPrompt appends standard Markdown file links to text.
func FormatAttachmentPrompt(text string, files []SavedFile) string {
	if len(files) == 0 {
		return text
	}
	var b strings.Builder
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		b.WriteString("The user attached file(s):")
	} else {
		b.WriteString(text)
	}
	b.WriteString("\n\n")
	for i, f := range files {
		if i > 0 {
			b.WriteByte('\n')
		}
		label := f.OriginalName
		if label == "" {
			label = f.StoredName
		}
		if label == "" {
			label = filepath.Base(f.Path)
		}
		b.WriteString("Attached file: [")
		b.WriteString(escapeMarkdownLinkText(label))
		b.WriteString("](")
		b.WriteString(escapeMarkdownURL(f.Path))
		b.WriteString(")")
	}
	return b.String()
}

func escapeMarkdownLinkText(s string) string {
	s = strings.ReplaceAll(s, `\\`, `\\\\`)
	s = strings.ReplaceAll(s, "]", `\]`)
	return s
}

func escapeMarkdownURL(s string) string {
	// Paths produced by Save are absolute filesystem paths and should not need
	// escaping beyond spaces/parens for Markdown's inline-link syntax.
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case ' ':
			b.WriteString("%20")
		case '(':
			b.WriteString("%28")
		case ')':
			b.WriteString("%29")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
