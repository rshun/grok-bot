package telegram

import (
	"context"
	"testing"
)

func TestFormatHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fenced code block",
			in:   "目录内容：\n```\na.log\nb<1>.log\n```",
			want: "目录内容：\n<pre><code>a.log\nb&lt;1&gt;.log</code></pre>",
		},
		{
			name: "single line triple backticks",
			in:   "- ```app.log```\n```dir/```",
			want: "- <code>app.log</code>\n<pre><code>dir/</code></pre>",
		},
		{
			name: "inline code bold italic link",
			in:   "**加粗** *斜体* `code_<x>` [链接](https://example.com/a&b)",
			want: "<b>加粗</b> <i>斜体</i> <code>code_&lt;x&gt;</code> <a href=\"https://example.com/a&amp;b\">链接</a>",
		},
		{
			name: "heading and blockquote",
			in:   "### 标题\n> 引用 `x`",
			want: "<b>标题</b>\n<blockquote>引用 <code>x</code></blockquote>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHTML(tc.in); got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestSendUsesHTMLParseModeWithFallback(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(method string, n int) string {
		if n == 1 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
		}
		return ""
	})
	if err := b.Send(context.Background(), "9", "文件：`a.log`"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (HTML then fallback), got %d", len(calls))
	}
	if calls[0].form["parse_mode"] != "HTML" || calls[0].form["text"] != "文件：<code>a.log</code>" {
		t.Fatalf("first call: %v", calls[0].form)
	}
	if calls[1].form["parse_mode"] != "" || calls[1].form["text"] != "文件：`a.log`" {
		t.Fatalf("fallback call: %v", calls[1].form)
	}
}
