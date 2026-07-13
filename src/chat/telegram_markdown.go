package chat

import (
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const telegramParseModeHTML = "HTML"

// RenderTelegramMarkdown converts Markdown-ish text into the Telegram Bot API's
// supported HTML parse mode subset.
func RenderTelegramMarkdown(md string) string {
	return renderTelegramMarkdown(md)
}

// renderTelegramMarkdown converts the assistant's normal Markdown-ish output
// into the Telegram Bot API's HTML parse mode. Telegram does not understand
// CommonMark directly (and MarkdownV2 requires aggressive escaping), so we emit
// a conservative HTML subset and escape everything else. The converter is
// intentionally small/stdlib-only: unsupported Markdown remains readable as
// plain text rather than making the Telegram request fail.
func renderTelegramMarkdown(md string) string {
	if md == "" {
		return ""
	}

	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")

	var out strings.Builder
	inFence := false
	fenceLang := ""

	for _, part := range strings.SplitAfter(md, "\n") {
		hasNL := strings.HasSuffix(part, "\n")
		line := strings.TrimSuffix(part, "\n")
		trimmed := strings.TrimLeft(line, " \t")

		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				out.WriteString("</code></pre>")
				if hasNL {
					out.WriteByte('\n')
				}
				inFence = false
				fenceLang = ""
				continue
			}

			fenceLang = sanitizeTelegramCodeLanguage(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
			out.WriteString("<pre><code")
			if fenceLang != "" {
				out.WriteString(` class="language-`)
				out.WriteString(html.EscapeString(fenceLang))
				out.WriteByte('"')
			}
			out.WriteByte('>')
			inFence = true
			continue
		}

		if inFence {
			out.WriteString(html.EscapeString(line))
			if hasNL {
				out.WriteByte('\n')
			}
			continue
		}

		out.WriteString(renderTelegramMarkdownLine(line))
		if hasNL {
			out.WriteByte('\n')
		}
	}

	if inFence {
		out.WriteString("</code></pre>")
	}
	return removeEmptyTelegramStyleTags(out.String())
}

func sanitizeTelegramCodeLanguage(lang string) string {
	if lang == "" {
		return ""
	}
	fields := strings.Fields(lang)
	if len(fields) == 0 {
		return ""
	}
	lang = fields[0]
	var out strings.Builder
	for _, r := range lang {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '+' || r == '#' || r == '.' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func renderTelegramMarkdownLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	leading := line[:len(line)-len(trimmed)]

	if heading, ok := markdownHeading(trimmed); ok {
		return leading + "<b>" + renderTelegramInlineMarkdown(strings.TrimSpace(heading), 0) + "</b>"
	}
	if isMarkdownRule(trimmed) {
		return leading + "────────"
	}
	if strings.HasPrefix(trimmed, ">") {
		quote := strings.TrimSpace(strings.TrimLeft(trimmed, "> "))
		return leading + "<blockquote>" + renderTelegramInlineMarkdown(quote, 0) + "</blockquote>"
	}
	if rest, ok := unorderedListItem(trimmed); ok {
		marker := "• "
		rest = strings.TrimSpace(rest)
		lower := strings.ToLower(rest)
		switch {
		case strings.HasPrefix(lower, "[ ] "):
			marker = "☐ "
			rest = strings.TrimSpace(rest[4:])
		case strings.HasPrefix(lower, "[x] "):
			marker = "☑ "
			rest = strings.TrimSpace(rest[4:])
		}
		return leading + marker + renderTelegramInlineMarkdown(rest, 0)
	}

	return renderTelegramInlineMarkdown(line, 0)
}

func markdownHeading(s string) (string, bool) {
	level := 0
	for level < len(s) && level < 6 && s[level] == '#' {
		level++
	}
	if level == 0 || level >= len(s) || !isASCIISpace(s[level]) {
		return "", false
	}
	return s[level+1:], true
}

func isMarkdownRule(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	first := s[0]
	if first != '-' && first != '*' && first != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != first && !isASCIISpace(s[i]) {
			return false
		}
	}
	return true
}

func unorderedListItem(s string) (string, bool) {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && isASCIISpace(s[1]) {
		return s[2:], true
	}
	return "", false
}

func renderTelegramInlineMarkdown(s string, depth int) string {
	return renderTelegramInlineMarkdownWithActive(s, depth, nil)
}

func renderTelegramInlineMarkdownWithActive(s string, depth int, activeTags []string) string {
	if s == "" {
		return ""
	}
	if depth > 8 {
		return html.EscapeString(s)
	}

	var out strings.Builder
	for i := 0; i < len(s); {
		// Markdown backslash escapes: \*literal\* -> *literal*.
		if s[i] == '\\' && i+1 < len(s) {
			r, n := utf8.DecodeRuneInString(s[i+1:])
			out.WriteString(html.EscapeString(string(r)))
			i += 1 + n
			continue
		}

		if strings.HasPrefix(s[i:], "`") {
			if end, ticks := findInlineCodeEnd(s, i); end >= 0 {
				// Telegram doesn't allow code/pre entities inside bold, italic,
				// underline, strikethrough, or spoiler entities. Temporarily close
				// active style tags around inline code, then reopen them.
				writeClosingTags(&out, activeTags)
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(s[i+ticks : end]))
				out.WriteString("</code>")
				writeOpeningTags(&out, activeTags)
				i = end + ticks
				continue
			}
		}

		if strings.HasPrefix(s[i:], "![") {
			if label, href, end, ok := parseMarkdownLink(s, i+1); ok {
				out.WriteString(renderTelegramLink(label, href, depth))
				i = end
				continue
			}
		}
		if s[i] == '[' {
			if label, href, end, ok := parseMarkdownLink(s, i); ok {
				out.WriteString(renderTelegramLink(label, href, depth))
				i = end
				continue
			}
		}

		if strings.HasPrefix(s[i:], "**") && canOpenEmphasis(s, i, "**") {
			if end := findClosingEmphasis(s, i+2, "**"); end >= 0 {
				out.WriteString("<b>")
				out.WriteString(renderTelegramInlineMarkdownWithActive(s[i+2:end], depth+1, appendActiveTag(activeTags, "b")))
				out.WriteString("</b>")
				i = end + 2
				continue
			}
		}
		if strings.HasPrefix(s[i:], "__") && canOpenEmphasis(s, i, "__") {
			if end := findClosingEmphasis(s, i+2, "__"); end >= 0 {
				out.WriteString("<b>")
				out.WriteString(renderTelegramInlineMarkdownWithActive(s[i+2:end], depth+1, appendActiveTag(activeTags, "b")))
				out.WriteString("</b>")
				i = end + 2
				continue
			}
		}
		if strings.HasPrefix(s[i:], "~~") && canOpenEmphasis(s, i, "~~") {
			if end := findClosingEmphasis(s, i+2, "~~"); end >= 0 {
				out.WriteString("<s>")
				out.WriteString(renderTelegramInlineMarkdownWithActive(s[i+2:end], depth+1, appendActiveTag(activeTags, "s")))
				out.WriteString("</s>")
				i = end + 2
				continue
			}
		}
		if s[i] == '*' && !strings.HasPrefix(s[i:], "**") && canOpenEmphasis(s, i, "*") {
			if end := findClosingEmphasis(s, i+1, "*"); end >= 0 {
				out.WriteString("<i>")
				out.WriteString(renderTelegramInlineMarkdownWithActive(s[i+1:end], depth+1, appendActiveTag(activeTags, "i")))
				out.WriteString("</i>")
				i = end + 1
				continue
			}
		}
		if s[i] == '_' && !strings.HasPrefix(s[i:], "__") && canOpenEmphasis(s, i, "_") {
			if end := findClosingEmphasis(s, i+1, "_"); end >= 0 {
				out.WriteString("<i>")
				out.WriteString(renderTelegramInlineMarkdownWithActive(s[i+1:end], depth+1, appendActiveTag(activeTags, "i")))
				out.WriteString("</i>")
				i = end + 1
				continue
			}
		}

		r, n := utf8.DecodeRuneInString(s[i:])
		out.WriteString(html.EscapeString(string(r)))
		i += n
	}
	return out.String()
}

func appendActiveTag(tags []string, tag string) []string {
	next := make([]string, 0, len(tags)+1)
	next = append(next, tags...)
	next = append(next, tag)
	return next
}

func removeEmptyTelegramStyleTags(s string) string {
	for {
		next := strings.ReplaceAll(s, "<b></b>", "")
		next = strings.ReplaceAll(next, "<i></i>", "")
		next = strings.ReplaceAll(next, "<s></s>", "")
		if next == s {
			return s
		}
		s = next
	}
}

func writeClosingTags(out *strings.Builder, tags []string) {
	for i := len(tags) - 1; i >= 0; i-- {
		out.WriteString("</")
		out.WriteString(tags[i])
		out.WriteByte('>')
	}
}

func writeOpeningTags(out *strings.Builder, tags []string) {
	for _, tag := range tags {
		out.WriteByte('<')
		out.WriteString(tag)
		out.WriteByte('>')
	}
}

func findInlineCodeEnd(s string, start int) (end int, ticks int) {
	for start+ticks < len(s) && s[start+ticks] == '`' {
		ticks++
	}
	if ticks == 0 {
		return -1, 0
	}
	idx := strings.Index(s[start+ticks:], strings.Repeat("`", ticks))
	if idx < 0 {
		return -1, ticks
	}
	return start + ticks + idx, ticks
}

func parseMarkdownLink(s string, start int) (label, href string, end int, ok bool) {
	closeLabel := strings.IndexByte(s[start+1:], ']')
	if closeLabel < 0 {
		return "", "", 0, false
	}
	closeLabel += start + 1
	if closeLabel+1 >= len(s) || s[closeLabel+1] != '(' {
		return "", "", 0, false
	}
	closeHref := strings.IndexByte(s[closeLabel+2:], ')')
	if closeHref < 0 {
		return "", "", 0, false
	}
	closeHref += closeLabel + 2
	label = s[start+1 : closeLabel]
	href = strings.TrimSpace(s[closeLabel+2 : closeHref])
	if label == "" || !validTelegramLinkURL(href) {
		return "", "", 0, false
	}
	return label, href, closeHref + 1, true
}

func renderTelegramLink(label, href string, depth int) string {
	return `<a href="` + html.EscapeString(href) + `">` + renderTelegramInlineMarkdown(label, depth+1) + `</a>`
}

func validTelegramLinkURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\n\r<>") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "tg":
		return true
	default:
		return false
	}
}

func canOpenEmphasis(s string, pos int, delim string) bool {
	after := pos + len(delim)
	if after >= len(s) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(s[after:])
	if unicode.IsSpace(next) {
		return false
	}
	if strings.Contains(delim, "_") {
		if pos > 0 {
			prev, _ := utf8.DecodeLastRuneInString(s[:pos])
			if isAlphaNum(prev) {
				return false
			}
		}
	}
	return true
}

func findClosingEmphasis(s string, start int, delim string) int {
	for search := start; search < len(s); {
		idx := strings.Index(s[search:], delim)
		if idx < 0 {
			return -1
		}
		pos := search + idx
		if strings.TrimSpace(s[start:pos]) == "" {
			search = pos + len(delim)
			continue
		}
		prev, _ := utf8.DecodeLastRuneInString(s[:pos])
		if unicode.IsSpace(prev) {
			search = pos + len(delim)
			continue
		}
		if strings.Contains(delim, "_") && pos+len(delim) < len(s) {
			next, _ := utf8.DecodeRuneInString(s[pos+len(delim):])
			if isAlphaNum(next) {
				search = pos + len(delim)
				continue
			}
		}
		return pos
	}
	return -1
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t'
}

func isAlphaNum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
