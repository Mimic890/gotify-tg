package telegram

import (
	"html"
	"strings"
	"unicode/utf8"
)

// MaxMessageLen is Telegram's limit for sendMessage text.
const MaxMessageLen = 4096

const (
	maxTitleRunes = 256
	maxAppRunes   = 128
)

// Format renders a Gotify message as Telegram HTML, split into chunks that
// each fit into one sendMessage call. All input is HTML-escaped.
//
// Lengths are measured in UTF-16 code units of the raw HTML, which is an
// upper bound of what Telegram counts after entity parsing.
func Format(title, app, body string) []string {
	title = truncate(strings.TrimSpace(clean(title)), maxTitleRunes)
	app = truncate(strings.TrimSpace(clean(app)), maxAppRunes)
	body = strings.TrimSpace(clean(body))

	var hb strings.Builder
	if title != "" {
		hb.WriteString("<b>" + html.EscapeString(title) + "</b>\n")
	}
	if app != "" {
		hb.WriteString("<i>" + html.EscapeString(app) + "</i>\n")
	}
	header := hb.String()

	if body == "" {
		header = strings.TrimRight(header, "\n")
		if header == "" {
			return []string{"(empty message)"}
		}
		return []string{header}
	}
	if header != "" {
		header += "\n"
	}

	chunks := Split(html.EscapeString(body), MaxMessageLen-utf16Len(header), MaxMessageLen)
	chunks[0] = header + chunks[0]
	return chunks
}

// Split cuts already escaped HTML text into chunks: the first chunk holds at
// most first UTF-16 units, every following one at most rest. HTML entities
// are never cut, and cuts prefer line breaks, then spaces.
func Split(s string, first, rest int) []string {
	var out []string
	limit := first
	for {
		end, next := cutPoint(s, limit)
		if end == len(s) {
			out = append(out, s)
			return out
		}
		out = append(out, s[:end])
		s = s[next:]
		limit = rest
	}
}

// cutPoint returns where to end the current chunk (end) and where the next
// one starts (next, which skips the separator at the cut).
func cutPoint(s string, limit int) (end, next int) {
	width := 0
	lastNL, lastSP := -1, -1
	i := 0
	for i < len(s) {
		n, w := unit(s[i:])
		if width+w > limit {
			break
		}
		switch s[i] {
		case '\n':
			lastNL = i
		case ' ':
			lastSP = i
		}
		width += w
		i += n
	}
	if i >= len(s) {
		return len(s), len(s)
	}
	// Only use a separator if it keeps the chunk at least half full.
	if lastNL > 0 && lastNL >= i/2 {
		return lastNL, lastNL + 1
	}
	if lastSP > 0 && lastSP >= i/2 {
		return lastSP, lastSP + 1
	}
	if i == 0 { // limit smaller than a single unit; never loop forever
		n, _ := unit(s)
		return n, n
	}
	return i, i
}

// unit returns the byte length and UTF-16 width of the next indivisible
// piece of s: an HTML entity like "&amp;" or a single rune.
func unit(s string) (n, width int) {
	if s[0] == '&' {
		if j := strings.IndexByte(s, ';'); j > 0 && j <= 10 {
			return j + 1, j + 1
		}
	}
	r, n := utf8.DecodeRuneInString(s)
	if r > 0xFFFF {
		return n, 2
	}
	return n, 1
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// clean makes s valid UTF-8 and drops NUL bytes, which Telegram rejects.
func clean(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}
