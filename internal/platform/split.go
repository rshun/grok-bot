package platform

import "strings"

const fenceClose = "\n```"
const fenceOpen = "```\n"

// Split breaks text into chunks of at most limit runes. Empty text becomes a
// single space so a platform that rejects empty messages still receives one.
// When a cut lands inside a fenced code block (```) and limit is large enough,
// the chunk is closed with ``` and the next chunk reopens the fence.
func Split(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{" "}
	}
	if limit < 1 {
		limit = 4000
	}
	canFence := limit >= 16
	var parts []string
	for len(runes) > limit {
		maxCut := limit
		if canFence && unclosedFence(string(runes[:limit])) {
			maxCut = limit - len([]rune(fenceClose))
		}
		cut := maxCut
		for i := maxCut; i > maxCut/2; i-- {
			if runes[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut < 1 {
			cut = maxCut
		}
		chunk := string(runes[:cut])
		reopen := canFence && unclosedFence(chunk)
		if reopen {
			chunk += fenceClose
		}
		parts = append(parts, chunk)
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
		if reopen && len(runes) > 0 {
			runes = append([]rune(fenceOpen), runes...)
		}
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	if len(parts) == 0 {
		return []string{text}
	}
	return parts
}

func unclosedFence(s string) bool {
	open := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			rest := strings.TrimPrefix(trimmed, "```")
			if !open && strings.HasSuffix(rest, "```") && len(rest) >= 3 {
				continue
			}
			open = !open
		}
	}
	return open
}
