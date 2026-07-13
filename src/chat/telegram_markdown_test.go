package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRenderTelegramMarkdown(t *testing.T) {
	input := "# Title\nHello **bold** and `x < y`.\n- [x] done\n```go\nfmt.Println(\"x\")\n```\nSee [site](https://example.com?a=1&b=2)"
	want := "<b>Title</b>\nHello <b>bold</b> and <code>x &lt; y</code>.\n☑ done\n<pre><code class=\"language-go\">fmt.Println(&#34;x&#34;)\n</code></pre>\nSee <a href=\"https://example.com?a=1&amp;b=2\">site</a>"
	if got := renderTelegramMarkdown(input); got != want {
		t.Fatalf("renderTelegramMarkdown() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderTelegramMarkdown_EscapesUnsupportedMarkdown(t *testing.T) {
	input := "Use foo_bar_baz and <unsafe> & an invalid [link](javascript:alert(1))."
	want := "Use foo_bar_baz and &lt;unsafe&gt; &amp; an invalid [link](javascript:alert(1))."
	if got := renderTelegramMarkdown(input); got != want {
		t.Fatalf("renderTelegramMarkdown() = %q, want %q", got, want)
	}
}

func TestRenderTelegramMarkdown_AvoidsCodeInsideStyleEntities(t *testing.T) {
	input := "**Quick/simple: cron + `wall-e msg`** and *use `m2h` too*"
	want := "<b>Quick/simple: cron + </b><code>wall-e msg</code> and <i>use </i><code>m2h</code><i> too</i>"
	if got := renderTelegramMarkdown(input); got != want {
		t.Fatalf("renderTelegramMarkdown() = %q, want %q", got, want)
	}
}

func TestHTTPTelegramAPI_SendMessageUsesHTMLParseMode(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/sendMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":42},"text":""}}`)
	}))
	defer srv.Close()

	api := newHTTPTelegramAPI("token", srv.URL)
	if _, err := api.SendMessage(context.Background(), 42, "**hi** <tag>", 7); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if got["parse_mode"] != telegramParseModeHTML {
		t.Fatalf("parse_mode = %v, want %s", got["parse_mode"], telegramParseModeHTML)
	}
	if got["text"] != "<b>hi</b> &lt;tag&gt;" {
		t.Fatalf("text = %v", got["text"])
	}
	if got["reply_to_message_id"] != float64(7) {
		t.Fatalf("reply_to_message_id = %v", got["reply_to_message_id"])
	}
}
