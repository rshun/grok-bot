package telegram

import (
	"html"
	"strings"
)

// formatHTML converts common Markdown into Telegram-compatible HTML.
// All literal text is HTML-escaped so arbitrary file names and logs never break parsing.
func formatHTML(text string) string {
	var out strings.Builder
	lines := strings.Split(text, "\n")
	inFence := false
	var fenceBuf strings.Builder

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				if i > 0 {
					out.WriteByte('\n')
				}
				// Single-line ```code``` on one line.
				rest := strings.TrimPrefix(trimmed, "```")
				if strings.HasSuffix(rest, "```") && len(rest) >= 3 {
					inner := strings.TrimSuffix(rest, "```")
					out.WriteString("<pre><code>")
					out.WriteString(html.EscapeString(inner))
					out.WriteString("</code></pre>")
					continue
				}
				inFence = true
				fenceBuf.Reset()
				continue
			}
			inFence = false
			out.WriteString("<pre><code>")
			out.WriteString(html.EscapeString(fenceBuf.String()))
			out.WriteString("</code></pre>")
			continue
		}
		if inFence {
			if fenceBuf.Len() > 0 {
				fenceBuf.WriteByte('\n')
			}
			fenceBuf.WriteString(line)
			continue
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(formatLine(line))
	}
	if inFence {
		out.WriteString("<pre><code>")
		out.WriteString(html.EscapeString(fenceBuf.String()))
		out.WriteString("</code></pre>")
	}
	return out.String()
}

func formatLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	// Headings: # .. ######
	if strings.HasPrefix(trimmed, "#") {
		hashes := 0
		for hashes < len(trimmed) && trimmed[hashes] == '#' {
			hashes++
		}
		if hashes >= 1 && hashes <= 6 && hashes < len(trimmed) && trimmed[hashes] == ' ' {
			return "<b>" + formatInline(strings.TrimSpace(trimmed[hashes+1:])) + "</b>"
		}
	}
	// Blockquote: > text
	if strings.HasPrefix(trimmed, "> ") {
		return "<blockquote>" + formatInline(strings.TrimPrefix(trimmed, "> ")) + "</blockquote>"
	}
	return formatInline(line)
}

func formatInline(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		// Inline triple-backtick ```...```
		if strings.HasPrefix(s[i:], "```") {
			if end := strings.Index(s[i+3:], "```"); end >= 0 {
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(s[i+3 : i+3+end]))
				out.WriteString("</code>")
				i += 3 + end + 3
				continue
			}
		}
		// Inline code `...`
		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(s[i+1 : i+1+end]))
				out.WriteString("</code>")
				i += 1 + end + 1
				continue
			}
		}
		// Markdown link [label](https://...)
		if s[i] == '[' {
			if labelEnd := strings.Index(s[i+1:], "]("); labelEnd >= 0 {
				label := s[i+1 : i+1+labelEnd]
				urlStart := i + 1 + labelEnd + 2
				if urlEnd := strings.IndexByte(s[urlStart:], ')'); urlEnd >= 0 {
					rawURL := s[urlStart : urlStart+urlEnd]
					if (strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://")) && !strings.ContainsAny(rawURL, " \t\n\"<>") {
						out.WriteString(`<a href="`)
						out.WriteString(html.EscapeString(rawURL))
						out.WriteString(`">`)
						out.WriteString(formatInline(label))
						out.WriteString("</a>")
						i = urlStart + urlEnd + 1
						continue
					}
				}
			}
		}
		// Bold **...**
		if strings.HasPrefix(s[i:], "**") {
			if end := strings.Index(s[i+2:], "**"); end > 0 {
				out.WriteString("<b>")
				out.WriteString(formatInline(s[i+2 : i+2+end]))
				out.WriteString("</b>")
				i += 2 + end + 2
				continue
			}
		}
		// Strikethrough ~~...~~
		if strings.HasPrefix(s[i:], "~~") {
			if end := strings.Index(s[i+2:], "~~"); end > 0 {
				out.WriteString("<s>")
				out.WriteString(formatInline(s[i+2 : i+2+end]))
				out.WriteString("</s>")
				i += 2 + end + 2
				continue
			}
		}
		// Italic *...* (only when not followed by space and closed on the same line)
		if s[i] == '*' && i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '*' {
			if end := strings.IndexByte(s[i+1:], '*'); end > 0 && s[i+end] != ' ' {
				out.WriteString("<i>")
				out.WriteString(formatInline(s[i+1 : i+1+end]))
				out.WriteString("</i>")
				i += 1 + end + 1
				continue
			}
		}
		switch s[i] {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		default:
			out.WriteByte(s[i])
		}
		i++
	}
	return out.String()
}
