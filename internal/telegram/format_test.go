package telegram

import (
	"html"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatEscapes(t *testing.T) {
	got := Format(`<script>&"x"`, "My <App>", "a < b && c > d")
	want := "<b>&lt;script&gt;&amp;&#34;x&#34;</b>\n<i>My &lt;App&gt;</i>\n\na &lt; b &amp;&amp; c &gt; d"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestFormatEmptyParts(t *testing.T) {
	if got := Format("", "app", ""); got[0] != "<i>app</i>" {
		t.Errorf("got %q", got)
	}
	if got := Format("", "", "body"); got[0] != "body" {
		t.Errorf("got %q", got)
	}
	if got := Format(" ", "", "\n"); got[0] != "(empty message)" {
		t.Errorf("got %q", got)
	}
}

func TestFormatInvalidUTF8(t *testing.T) {
	got := Format("t", "a", "ok\xff\x00end")
	if !utf8.ValidString(got[0]) || strings.Contains(got[0], "\x00") {
		t.Errorf("got %q", got)
	}
}

func TestFormatSplitsLongMessages(t *testing.T) {
	body := strings.Repeat("line with <tags> & stuff 😀\n", 600)
	chunks := Format("Title", "App", body)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], "<b>Title</b>\n<i>App</i>\n\n") {
		t.Errorf("header missing: %q", chunks[0][:40])
	}
	var rebuilt strings.Builder
	for i, c := range chunks {
		if n := utf16Len(c); n > MaxMessageLen {
			t.Errorf("chunk %d has %d UTF-16 units", i, n)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
		if i > 0 && strings.Contains(c, "<b>") {
			t.Errorf("header repeated in chunk %d", i)
		}
		text := c
		if i == 0 {
			text = strings.TrimPrefix(c, "<b>Title</b>\n<i>App</i>\n\n")
		}
		if i > 0 {
			rebuilt.WriteString("\n") // the separator dropped at the cut
		}
		rebuilt.WriteString(text)
	}
	if got := html.UnescapeString(rebuilt.String()); got != strings.TrimSpace(body) {
		t.Error("content lost or altered while splitting")
	}
}

func TestSplitNeverCutsEntities(t *testing.T) {
	s := html.EscapeString(strings.Repeat("&", 50)) // "&amp;" x50, no separators
	for _, c := range Split(s, 12, 12) {
		if html.UnescapeString(c) != strings.Repeat("&", len(c)/5) || len(c)%5 != 0 {
			t.Fatalf("entity cut: %q", c)
		}
	}
}

func TestSplitHardCutWithoutSeparators(t *testing.T) {
	s := strings.Repeat("x", 10000)
	chunks := Split(s, MaxMessageLen, MaxMessageLen)
	if len(chunks) != 3 || len(chunks[0]) != MaxMessageLen || strings.Join(chunks, "") != s {
		t.Fatalf("got %d chunks", len(chunks))
	}
}

func TestSplitPrefersNewline(t *testing.T) {
	s := strings.Repeat("a", 70) + "\n" + strings.Repeat("b", 20) + " " + strings.Repeat("c", 30)
	chunks := Split(s, 100, 100)
	if chunks[0] != strings.Repeat("a", 70) {
		t.Fatalf("got %q", chunks)
	}
}

func TestTruncateTitle(t *testing.T) {
	got := Format(strings.Repeat("t", 1000), "", "")
	if n := utf8.RuneCountInString(got[0]); n > maxTitleRunes+len("<b></b>") {
		t.Errorf("title not truncated: %d runes", n)
	}
}
