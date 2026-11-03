package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rshun/grok-bot/internal/grok"
)

const (
	maxPrefetchURLs      = 3
	maxPrefetchBodyBytes = 512 << 10 // 512 KB
	maxPrefetchRunes     = 3200
	perLinkTimeout       = 6 * time.Second
	totalPrefetchTimeout = 8 * time.Second
	prefetchUserAgent    = "Mozilla/5.0 (compatible; grok-bot/0.5; +https://github.com/rshun/grok-bot)"
)

var (
	reHTTPURL = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#\[\]@!$&()*+,;=%]+`)
	reDropBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg>`),
		regexp.MustCompile(`(?is)<iframe\b[^>]*>.*?</iframe>`),
		regexp.MustCompile(`(?is)<template\b[^>]*>.*?</template>`),
	}
	reHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTitleTag    = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	reMetaDesc1   = regexp.MustCompile(`(?is)<meta\b[^>]*\b(?:name|property)\s*=\s*["'](?:description|og:description|twitter:description)["'][^>]*\bcontent\s*=\s*["']([^"']+)["'][^>]*>`)
	reMetaDesc2   = regexp.MustCompile(`(?is)<meta\b[^>]*\bcontent\s*=\s*["']([^"']+)["'][^>]*\b(?:name|property)\s*=\s*["'](?:description|og:description|twitter:description)["'][^>]*>`)
	reBlockTag    = regexp.MustCompile(`(?i)</?(?:p|div|br|li|tr|h[1-6]|article|section|header|footer|blockquote|pre|hr)\b[^>]*>`)
	reAnyTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)

	errBlockedHost = errors.New("blocked non-public host")
)

var safePrefetchClient = newSafeHTTPClient()

func newSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   4 * time.Second,
		KeepAlive: 15 * time.Second,
	}
	transport := &http.Transport{
		Proxy: nil, // Do not route untrusted user URLs through internal proxies.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if isBlockedHostname(host) {
				return nil, errBlockedHost
			}
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(addrs) == 0 {
				return nil, errBlockedHost
			}
			for _, a := range addrs {
				if !isPublicIP(a.IP) {
					return nil, errBlockedHost
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   perLinkTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("unsupported redirect scheme")
			}
			if req.URL.User != nil || isBlockedHostname(req.URL.Hostname()) {
				return errBlockedHost
			}
			return nil
		},
	}
}

func isBlockedHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return !isPublicIP(ip)
	}
	return !strings.Contains(h, ".")
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0:
			return false
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // RFC 6598 CGNAT 100.64.0.0/10
			return false
		case ip4[0] == 169 && ip4[1] == 254:
			return false
		case ip4[0] == 192 && ip4[1] == 0 && (ip4[2] == 0 || ip4[2] == 2):
			return false
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19):
			return false
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100:
			return false
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113:
			return false
		case ip4[0] >= 224:
			return false
		}
	}
	return true
}

// extractURLs pulls up to maxPrefetchURLs unique http/https links from a user message,
// stripping trailing prose punctuation while preserving balanced parentheses in URLs.
func extractURLs(text string) []string {
	matches := reHTTPURL.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, raw := range matches {
		cleaned := trimTrailingURLPunct(raw)
		u, err := url.Parse(cleaned)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			continue
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
		if len(out) >= maxPrefetchURLs {
			break
		}
	}
	return out
}

func trimTrailingURLPunct(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		switch r {
		case '.', ',', ';', ':', '!', '?', ']', '}', '>', '\'', '"',
			'。', '，', '；', '：', '！', '？', '）', '】', '》', '」', '』', '”', '’':
			s = s[:len(s)-size]
		case ')':
			if strings.Count(s, ")") > strings.Count(s, "(") {
				s = s[:len(s)-size]
			} else {
				return s
			}
		default:
			return s
		}
	}
	return s
}

func (s *Service) withLinkPrefetch(ctx context.Context, userText, prompt string) string {
	if s.DisableLinkPrefetch {
		return prompt
	}
	urls := extractURLs(userText)
	if len(urls) == 0 {
		return prompt
	}
	fetchCtx, cancel := context.WithTimeout(ctx, totalPrefetchTimeout)
	defer cancel()

	fetcher := s.LinkFetcher
	if fetcher == nil {
		fetcher = func(c context.Context, rawURL string) (string, error) {
			return fetchLinkSummaryWithClient(c, safePrefetchClient, rawURL, "https://api.github.com", "https://raw.githubusercontent.com", "https://api.fxtwitter.com")
		}
	}

	results := make([]string, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			linkCtx, linkCancel := context.WithTimeout(fetchCtx, perLinkTimeout)
			defer linkCancel()
			summary, err := fetcher(linkCtx, target)
			if err != nil {
				return
			}
			summary = strings.TrimSpace(grok.RedactSensitive(summary))
			if summary != "" {
				results[idx] = fmt.Sprintf("--- 链接: %s ---\n%s", target, truncateLinkRunes(summary, maxPrefetchRunes))
			}
		}(i, u)
	}
	wg.Wait()

	var blocks []string
	for _, r := range results {
		if r != "" {
			blocks = append(blocks, r)
		}
	}
	if len(blocks) == 0 {
		return prompt
	}
	return "[外链预处理摘录（系统已自动抓取消息中链接的正文摘要供参考）]\n" +
		strings.Join(blocks, "\n\n") +
		"\n[外链摘录结束]\n\n" + prompt
}

func fetchLinkSummaryWithClient(ctx context.Context, client *http.Client, rawURL, githubAPIBase, githubRawBase, fxTwitterBase string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))

	switch host {
	case "github.com":
		if summary, ok := fetchGitHubURL(ctx, client, u, githubAPIBase, githubRawBase); ok {
			return summary, nil
		}
	case "x.com", "twitter.com", "mobile.twitter.com":
		if summary, ok := fetchXStatusURL(ctx, client, u, fxTwitterBase); ok {
			return summary, nil
		}
	}
	return fetchGenericWebpage(ctx, client, rawURL)
}

func fetchGitHubURL(ctx context.Context, client *http.Client, u *url.URL, apiBase, rawBase string) (string, bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	owner, repo := parts[0], parts[1]
	if len(parts) >= 4 && parts[2] == "pull" {
		num := parts[3]
		apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%s", strings.TrimRight(apiBase, "/"), owner, repo, num)
		body, _, err := doPrefetchGET(ctx, client, apiURL, "application/vnd.github+json")
		if err != nil {
			return "", false
		}
		var pr struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			State     string `json:"state"`
			Draft     bool   `json:"draft"`
			Merged    bool   `json:"merged"`
			Body      string `json:"body"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Changed   int    `json:"changed_files"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if json.Unmarshal(body, &pr) != nil || pr.Title == "" {
			return "", false
		}
		state := pr.State
		if pr.Merged {
			state = "merged"
		} else if pr.Draft {
			state = "draft"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "GitHub PR %s/%s#%d: %s\n", owner, repo, pr.Number, pr.Title)
		fmt.Fprintf(&b, "状态: %s | 作者: @%s | 分支: %s <- %s | 变更: +%d / -%d (%d files)\n",
			state, pr.User.Login, pr.Base.Ref, pr.Head.Ref, pr.Additions, pr.Deletions, pr.Changed)
		if desc := strings.TrimSpace(pr.Body); desc != "" {
			fmt.Fprintf(&b, "\n描述:\n%s\n", truncateLinkRunes(desc, 1800))
		}
		if filesBody, _, fErr := doPrefetchGET(ctx, client, apiURL+"/files?per_page=15", "application/vnd.github+json"); fErr == nil {
			var files []struct {
				Filename  string `json:"filename"`
				Status    string `json:"status"`
				Additions int    `json:"additions"`
				Deletions int    `json:"deletions"`
			}
			if json.Unmarshal(filesBody, &files) == nil && len(files) > 0 {
				b.WriteString("\n变更文件:\n")
				for _, f := range files {
					fmt.Fprintf(&b, "- %s (%s, +%d/-%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
				}
			}
		}
		return strings.TrimSpace(b.String()), true
	}

	if len(parts) >= 4 && parts[2] == "issues" {
		num := parts[3]
		apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%s", strings.TrimRight(apiBase, "/"), owner, repo, num)
		body, _, err := doPrefetchGET(ctx, client, apiURL, "application/vnd.github+json")
		if err != nil {
			return "", false
		}
		var issue struct {
			Number   int    `json:"number"`
			Title    string `json:"title"`
			State    string `json:"state"`
			Comments int    `json:"comments"`
			Body     string `json:"body"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if json.Unmarshal(body, &issue) != nil || issue.Title == "" {
			return "", false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "GitHub Issue %s/%s#%d: %s\n", owner, repo, issue.Number, issue.Title)
		fmt.Fprintf(&b, "状态: %s | 作者: @%s | 评论数: %d\n", issue.State, issue.User.Login, issue.Comments)
		if desc := strings.TrimSpace(issue.Body); desc != "" {
			fmt.Fprintf(&b, "\n内容:\n%s", truncateLinkRunes(desc, 2200))
		}
		return strings.TrimSpace(b.String()), true
	}

	if len(parts) >= 4 && parts[2] == "commit" {
		sha := parts[3]
		apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", strings.TrimRight(apiBase, "/"), owner, repo, sha)
		body, _, err := doPrefetchGET(ctx, client, apiURL, "application/vnd.github+json")
		if err != nil {
			return "", false
		}
		var cm struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
			Stats struct {
				Additions int `json:"additions"`
				Deletions int `json:"deletions"`
			} `json:"stats"`
			Files []struct {
				Filename string `json:"filename"`
				Status   string `json:"status"`
			} `json:"files"`
		}
		if json.Unmarshal(body, &cm) != nil || cm.Commit.Message == "" {
			return "", false
		}
		shortSHA := cm.SHA
		if len(shortSHA) > 10 {
			shortSHA = shortSHA[:10]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "GitHub Commit %s/%s@%s (%s, +%d/-%d)\n%s\n",
			owner, repo, shortSHA, cm.Commit.Author.Name, cm.Stats.Additions, cm.Stats.Deletions, strings.TrimSpace(cm.Commit.Message))
		if len(cm.Files) > 0 {
			b.WriteString("\n涉及文件:\n")
			for i, f := range cm.Files {
				if i >= 15 {
					break
				}
				fmt.Fprintf(&b, "- %s (%s)\n", f.Filename, f.Status)
			}
		}
		return strings.TrimSpace(b.String()), true
	}

	if len(parts) >= 5 && parts[2] == "blob" {
		rawPath := strings.Join(parts[3:], "/")
		rawURL := fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(rawBase, "/"), owner, repo, rawPath)
		body, _, err := doPrefetchGET(ctx, client, rawURL, "text/plain")
		if err == nil && utf8.Valid(body) && len(bytesTrimSpace(body)) > 0 {
			return fmt.Sprintf("GitHub 文件 %s/%s/%s:\n%s", owner, repo, rawPath, truncateLinkRunes(string(body), maxPrefetchRunes)), true
		}
	}
	return "", false
}

func fetchXStatusURL(ctx context.Context, client *http.Client, u *url.URL, fxBase string) (string, bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Match /{user}/status/{id} or /i/web/status/{id} or /i/status/{id}
	var statusID, screenName string
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "status" || parts[i] == "statuses" {
			statusID = parts[i+1]
			if i > 0 {
				screenName = parts[i-1]
			}
			break
		}
	}
	if statusID == "" {
		return "", false
	}
	if screenName == "" {
		screenName = "i"
	}
	apiURL := fmt.Sprintf("%s/%s/status/%s", strings.TrimRight(fxBase, "/"), screenName, statusID)
	body, _, err := doPrefetchGET(ctx, client, apiURL, "application/json")
	if err != nil {
		return "", false
	}
	var payload struct {
		Tweet *struct {
			Text      string `json:"text"`
			CreatedAt string `json:"created_at"`
			Likes     int    `json:"likes"`
			Retweets  int    `json:"retweets"`
			Replies   int    `json:"replies"`
			Author    struct {
				Name       string `json:"name"`
				ScreenName string `json:"screen_name"`
			} `json:"author"`
			Article *struct {
				Title       string `json:"title"`
				PreviewText string `json:"preview_text"`
			} `json:"article"`
			Quote *struct {
				Text   string `json:"text"`
				Author struct {
					Name       string `json:"name"`
					ScreenName string `json:"screen_name"`
				} `json:"author"`
			} `json:"quote"`
		} `json:"tweet"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Tweet == nil {
		return "", false
	}
	tw := payload.Tweet
	var b strings.Builder
	fmt.Fprintf(&b, "X/Twitter 推文 — %s (@%s)", tw.Author.Name, tw.Author.ScreenName)
	if tw.CreatedAt != "" {
		fmt.Fprintf(&b, " · %s", tw.CreatedAt)
	}
	b.WriteByte('\n')
	if tw.Article != nil && tw.Article.Title != "" {
		fmt.Fprintf(&b, "长文标题: %s\n", tw.Article.Title)
		if tw.Article.PreviewText != "" {
			fmt.Fprintf(&b, "%s\n", tw.Article.PreviewText)
		}
	}
	if strings.TrimSpace(tw.Text) != "" {
		b.WriteString(strings.TrimSpace(tw.Text) + "\n")
	}
	if tw.Quote != nil && strings.TrimSpace(tw.Quote.Text) != "" {
		fmt.Fprintf(&b, "\n[引用 @%s]: %s\n", tw.Quote.Author.ScreenName, strings.TrimSpace(tw.Quote.Text))
	}
	fmt.Fprintf(&b, "互动: 💬 %d  🔁 %d  ❤️ %d", tw.Replies, tw.Retweets, tw.Likes)
	return strings.TrimSpace(b.String()), true
}

func fetchGenericWebpage(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	body, contentType, err := doPrefetchGET(ctx, client, rawURL, "text/html,application/xhtml+xml,application/json,text/plain;q=0.9,*/*;q=0.5")
	if err != nil {
		return "", err
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	for _, skipPrefix := range []string{"image/", "video/", "audio/", "font/", "application/octet-stream", "application/pdf", "application/zip"} {
		if strings.HasPrefix(ct, skipPrefix) {
			return "", errors.New("binary content skipped")
		}
	}
	raw := string(body)
	if ct == "text/html" || ct == "application/xhtml+xml" || looksLikeHTML(raw) {
		return extractHTMLContent(raw), nil
	}
	if !utf8.Valid(body) {
		return "", errors.New("non-utf8 content skipped")
	}
	return normalizeLinkText(raw), nil
}

func doPrefetchGET(ctx context.Context, client *http.Client, targetURL, accept string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", prefetchUserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPrefetchBodyBytes))
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(strings.TrimSpace(truncateLinkRunes(s, 256)))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

func extractHTMLContent(raw string) string {
	var title, desc string
	if m := reTitleTag.FindStringSubmatch(raw); len(m) == 2 {
		title = cleanInlineText(html.UnescapeString(reAnyTag.ReplaceAllString(m[1], "")))
	}
	if m := reMetaDesc1.FindStringSubmatch(raw); len(m) == 2 {
		desc = cleanInlineText(html.UnescapeString(m[1]))
	} else if m := reMetaDesc2.FindStringSubmatch(raw); len(m) == 2 {
		desc = cleanInlineText(html.UnescapeString(m[1]))
	}

	cleaned := reHTMLComment.ReplaceAllString(raw, " ")
	for _, drop := range reDropBlocks {
		cleaned = drop.ReplaceAllString(cleaned, " ")
	}
	cleaned = reBlockTag.ReplaceAllString(cleaned, "\n")
	cleaned = reAnyTag.ReplaceAllString(cleaned, " ")
	cleaned = html.UnescapeString(cleaned)
	body := normalizeLinkText(cleaned)

	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "标题: %s\n", title)
	}
	if desc != "" && !strings.Contains(body, desc) {
		fmt.Fprintf(&b, "摘要: %s\n", desc)
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(body)
	}
	return strings.TrimSpace(b.String())
}

func cleanInlineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeLinkText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.Join(strings.Fields(line), " ")
		out = append(out, trimmed)
	}
	joined := strings.TrimSpace(strings.Join(out, "\n"))
	return reBlankLines.ReplaceAllString(joined, "\n\n")
}

func truncateLinkRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit]) + "…[已截断]"
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
