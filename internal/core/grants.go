package core

import (
	"runtime"
	"strings"
	"unicode"
)

// Write access is granted only by phrases in the user's own message; Grok never grants it.
var (
	// Longer phrases come first so 允许写入 wins over 允许写.
	grantPhrases  = []string{"允许写入", "授权写入", "可以写入", "允许写"}
	revokePhrases = []string{"取消写入", "禁止写入", "撤销写入", "收回写入"}
	revokeVerbs   = []string{"取消", "撤销", "收回"}
	revokeObjects = []string{"写入权限", "写权限"}
	pathSuffixes  = []string{"目录", "文件夹", "里", "下"}
)

const clauseBreaks = "，。；！？,;\n"

// listSeparators may sit between paths that follow one phrase.
const listSeparators = " \t、和及:："

// pathEnds stop a path token.
const pathEnds = "、和及的"

// parseGrants returns the paths the message grants or revokes write access to.
// A path counts only when it follows the phrase in the same clause.
func parseGrants(text string) (grant, revoke []string) {
	for _, clause := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(clauseBreaks, r) }) {
		grant = append(grant, pathsAfter(clause, grantPhrases)...)
		revoke = append(revoke, pathsAfter(clause, revokePhrases)...)
		if containsAny(clause, revokeVerbs) && containsAny(clause, revokeObjects) {
			revoke = append(revoke, allPaths(clause)...)
		}
	}
	return dedupe(grant), dedupe(revoke)
}

// mentionsPath reports whether a message names a local path or talks about write access.
func mentionsPath(text string) bool {
	if containsAny(text, grantPhrases) || containsAny(text, revokePhrases) {
		return true
	}
	return len(allPaths(text)) > 0
}

// pathsAfter collects the paths that directly follow any of the phrases.
func pathsAfter(clause string, phrases []string) []string {
	var out []string
	rest := clause
	for {
		i, phrase := firstPhrase(rest, phrases)
		if i < 0 {
			return out
		}
		rest = rest[i+len(phrase):]
		for {
			trimmed := strings.TrimLeft(rest, listSeparators)
			if !pathStart(trimmed) {
				break
			}
			path, n := pathToken(trimmed)
			if path != "" {
				out = append(out, path)
			}
			rest = trimmed[n:]
		}
	}
}

func firstPhrase(text string, phrases []string) (int, string) {
	best, found := -1, ""
	for _, phrase := range phrases {
		if i := strings.Index(text, phrase); i >= 0 && (best < 0 || i < best) {
			best, found = i, phrase
		}
	}
	return best, found
}

// allPaths returns every path token in text. A slash right after an ASCII
// letter or digit (as in 1/2) does not start a path.
func allPaths(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		if !pathStart(text[i:]) || (i > 0 && isASCIIAlnum(text[i-1])) {
			i++
			continue
		}
		path, n := pathToken(text[i:])
		if path != "" {
			out = append(out, path)
		}
		i += n
	}
	return out
}

// pathToken reads a path starting at a slash and returns it with the bytes consumed.
func pathToken(text string) (string, int) {
	end := len(text)
	for i, r := range text {
		if i > 0 && (unicode.IsSpace(r) || strings.ContainsRune(pathEnds, r) || strings.ContainsRune(clauseBreaks, r)) {
			end = i
			break
		}
	}
	path := text[:end]
	for trimmed := true; trimmed; {
		trimmed = false
		for _, suffix := range pathSuffixes {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimSuffix(path, suffix)
				trimmed = true
			}
		}
	}
	if len(path) < 2 {
		return "", end
	}
	return path, end
}

// pathStart reports whether text begins with an absolute path: /x, or C:\x and C:/x on Windows.
func pathStart(text string) bool {
	if strings.HasPrefix(text, "/") {
		return true
	}
	return runtime.GOOS == "windows" && len(text) >= 3 && text[0] < 0x80 && unicode.IsLetter(rune(text[0])) &&
		text[1] == ':' && (text[2] == '\\' || text[2] == '/')
}

func isASCIIAlnum(b byte) bool {
	return b < 0x80 && (unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b)))
}

func containsAny(text string, items []string) bool {
	for _, item := range items {
		if strings.Contains(text, item) {
			return true
		}
	}
	return false
}

func dedupe(items []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}
