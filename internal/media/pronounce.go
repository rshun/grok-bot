package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultDictBaseURL = "https://api.dictionaryapi.dev/api/v2/entries/en"
	defaultTTSBaseURL  = "https://translate.googleapis.com/translate_tts"

	maxPronounceRunes = 500
	ttsChunkMaxRunes  = 180
	maxAudioBytes     = 10 << 20
	mediaUserAgent    = "Mozilla/5.0 (compatible; grok-bot/0.7; +https://github.com/rshun/grok-bot)"
)

var reSingleEnglishWord = regexp.MustCompile(`^[A-Za-z][A-Za-z'-]{0,44}$`)

// PronounceResult describes a synthesized or dictionary-fetched MP3 audio file.
type PronounceResult struct {
	Path     string
	Name     string
	Title    string
	Size     int64
	Lang     string
	Phonetic string
	Source   string // "dictionary" or "tts"
}

// Synthesizer produces standard, formal pronunciation MP3 files without requiring external API keys.
// Single English words prefer human lexicographer recordings from Free Dictionary API when matching
// the requested accent; phrases, sentences, and other languages use formal broadcast TTS.
type Synthesizer struct {
	Client      *http.Client
	DictBaseURL string
	TTSBaseURL  string
}

// Synthesize fetches or synthesizes pronunciation audio into outDir.
func (s Synthesizer) Synthesize(ctx context.Context, outDir, rawText, rawLang string) (PronounceResult, error) {
	text := cleanPronounceText(rawText)
	if text == "" {
		return PronounceResult{}, errors.New("请提供需要发音的单词或句子。")
	}
	if utf8.RuneCountInString(text) > maxPronounceRunes {
		return PronounceResult{}, fmt.Errorf("单次发音内容最多 %d 个字符，请精简为单词或短句。", maxPronounceRunes)
	}
	if outDir == "" {
		return PronounceResult{}, errors.New("语音输出目录未配置。")
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return PronounceResult{}, errors.New("创建语音缓存目录失败。")
	}

	canonicalLang, ttsLang, accentTag := NormalizeLang(text, rawLang)
	client := s.httpClient()

	var (
		audioBytes []byte
		phonetic   string
		source     string
	)
	if reSingleEnglishWord.MatchString(text) && (canonicalLang == "en-US" || canonicalLang == "en-GB") {
		dictData, ipa, ok := s.tryDictionaryAudio(ctx, client, strings.ToLower(text), accentTag)
		if ipa != "" {
			phonetic = ipa
		}
		if ok {
			audioBytes = dictData
			source = "dictionary"
		}
	}

	if len(audioBytes) == 0 {
		ttsData, err := s.fetchTTS(ctx, client, text, ttsLang)
		if err != nil {
			return PronounceResult{}, errors.New("暂时无法获取该内容的标准语音，请稍后重试。")
		}
		audioBytes = ttsData
		source = "tts"
	}

	slug := buildFilenameSlug(text)
	displayName := fmt.Sprintf("%s_%s.mp3", slug, canonicalLang)
	diskName := fmt.Sprintf("%s_%s_%s.mp3", slug, canonicalLang, randHex(4))
	diskPath := filepath.Join(outDir, diskName)
	if err := writeFileAtomic(diskPath, audioBytes); err != nil {
		return PronounceResult{}, errors.New("保存语音文件失败。")
	}

	title := text
	if runes := []rune(title); len(runes) > 48 {
		title = string(runes[:48]) + "…"
	}
	return PronounceResult{
		Path:     diskPath,
		Name:     displayName,
		Title:    title,
		Size:     int64(len(audioBytes)),
		Lang:     canonicalLang,
		Phonetic: phonetic,
		Source:   source,
	}, nil
}

func (s Synthesizer) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return NewSafeHTTPClient(12 * time.Second)
}

func (s Synthesizer) dictURL() string {
	if s.DictBaseURL != "" {
		return strings.TrimRight(s.DictBaseURL, "/")
	}
	return defaultDictBaseURL
}

func (s Synthesizer) ttsURL() string {
	if s.TTSBaseURL != "" {
		return strings.TrimRight(s.TTSBaseURL, "/")
	}
	return defaultTTSBaseURL
}

type dictEntry struct {
	Phonetic  string `json:"phonetic"`
	Phonetics []struct {
		Text  string `json:"text"`
		Audio string `json:"audio"`
	} `json:"phonetics"`
}

func (s Synthesizer) tryDictionaryAudio(ctx context.Context, client *http.Client, word, accent string) ([]byte, string, bool) {
	dictCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	endpoint := s.dictURL() + "/" + url.PathEscape(word)
	req, err := http.NewRequestWithContext(dictCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", mediaUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, "", false
	}
	var entries []dictEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return nil, "", false
	}

	var bestAudio, neutralAudio, bestIPA string
	for _, entry := range entries {
		if bestIPA == "" && strings.TrimSpace(entry.Phonetic) != "" {
			bestIPA = strings.TrimSpace(entry.Phonetic)
		}
		for _, p := range entry.Phonetics {
			ipa := strings.TrimSpace(p.Text)
			audioURL := strings.TrimSpace(p.Audio)
			if strings.HasPrefix(audioURL, "//") {
				audioURL = "https:" + audioURL
			}
			lowerAudio := strings.ToLower(audioURL)
			if ipa != "" && bestIPA == "" {
				bestIPA = ipa
			}
			if audioURL == "" || (!strings.HasPrefix(lowerAudio, "https://") && !strings.HasPrefix(lowerAudio, "http://")) {
				continue
			}
			tag := detectAudioAccentTag(lowerAudio)
			if tag == accent {
				if bestAudio == "" {
					bestAudio = audioURL
				}
				if ipa != "" {
					bestIPA = ipa
				}
			} else if tag == "" && neutralAudio == "" {
				neutralAudio = audioURL
			}
		}
	}

	chosen := bestAudio
	if chosen == "" {
		chosen = neutralAudio
	}
	if chosen == "" {
		return nil, bestIPA, false
	}
	data, err := downloadBytes(dictCtx, client, chosen, maxAudioBytes)
	if err != nil || !IsValidMP3(data) {
		return nil, bestIPA, false
	}
	return data, bestIPA, true
}

func detectAudioAccentTag(lowerURL string) string {
	switch {
	case strings.Contains(lowerURL, "-us.") || strings.Contains(lowerURL, "_us.") || strings.Contains(lowerURL, "/us/"):
		return "us"
	case strings.Contains(lowerURL, "-uk.") || strings.Contains(lowerURL, "_uk.") || strings.Contains(lowerURL, "-gb.") || strings.Contains(lowerURL, "/uk/"):
		return "uk"
	case strings.Contains(lowerURL, "-au.") || strings.Contains(lowerURL, "_au.") || strings.Contains(lowerURL, "-ca."):
		return "other"
	default:
		return ""
	}
}

func (s Synthesizer) fetchTTS(ctx context.Context, client *http.Client, text, ttsLang string) ([]byte, error) {
	chunks := splitTTSChunks(text, ttsChunkMaxRunes)
	if len(chunks) == 0 {
		return nil, errors.New("empty tts chunks")
	}
	var combined bytes.Buffer
	for i, chunk := range chunks {
		q := url.Values{}
		q.Set("ie", "UTF-8")
		q.Set("client", "tw-ob")
		q.Set("tl", ttsLang)
		q.Set("q", chunk)
		endpoint := s.ttsURL() + "?" + q.Encode()
		data, err := downloadBytes(ctx, client, endpoint, maxAudioBytes)
		if err != nil {
			return nil, err
		}
		if !IsValidMP3(data) {
			return nil, errors.New("invalid mp3 payload")
		}
		if i > 0 {
			data = stripID3v2(data)
		}
		if combined.Len()+len(data) > maxAudioBytes {
			return nil, errors.New("audio exceeds limit")
		}
		combined.Write(data)
	}
	return combined.Bytes(), nil
}

func downloadBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", mediaUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("response too large")
	}
	return data, nil
}

// IsValidMP3 checks whether data looks like a genuine MP3 stream (ID3v2 header or MPEG audio sync word)
// and is not an HTML/JSON error page.
func IsValidMP3(data []byte) bool {
	if len(data) < 64 {
		return false
	}
	trimmed := bytes.TrimLeft(data[:minInt(len(data), 32)], " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("<")) || bytes.HasPrefix(trimmed, []byte("{")) {
		return false
	}
	if len(data) >= 3 && data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		return true
	}
	// Allow optional zero padding before first MPEG frame sync.
	limit := minInt(len(data)-1, 512)
	for i := 0; i < limit; i++ {
		if data[i] == 0xFF && (data[i+1]&0xE0) == 0xE0 && (data[i+1]&0x18) != 0x08 && (data[i+1]&0x06) != 0x00 {
			return true
		}
	}
	return false
}

func stripID3v2(data []byte) []byte {
	if len(data) < 10 || data[0] != 'I' || data[1] != 'D' || data[2] != '3' {
		return data
	}
	// Synchsafe 28-bit integer size in bytes 6..9.
	size := (int(data[6]&0x7F) << 21) |
		(int(data[7]&0x7F) << 14) |
		(int(data[8]&0x7F) << 7) |
		int(data[9]&0x7F)
	total := 10 + size
	if total > 0 && total < len(data) {
		return data[total:]
	}
	return data
}

// NormalizeLang normalizes user/model language tags into a canonical display tag,
// the TTS language parameter, and an English accent preference ("us" or "uk").
func NormalizeLang(text, rawLang string) (canonical, ttsLang, accent string) {
	clean := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(rawLang, "_", "-")))
	switch clean {
	case "en-gb", "uk", "gb", "en-uk", "british":
		return "en-GB", "en-GB", "uk"
	case "en-us", "en", "us", "american":
		return "en-US", "en", "us"
	case "en-au", "au":
		return "en-AU", "en-AU", "other"
	case "zh", "zh-cn", "zh-hans", "cmn", "cn":
		return "zh-CN", "zh-CN", ""
	case "zh-tw", "zh-hk", "zh-hant", "tw":
		return "zh-TW", "zh-TW", ""
	case "ja", "ja-jp", "jp":
		return "ja-JP", "ja", ""
	case "ko", "ko-kr", "kr":
		return "ko-KR", "ko", ""
	case "fr", "fr-fr":
		return "fr-FR", "fr", ""
	case "de", "de-de":
		return "de-DE", "de", ""
	case "es", "es-es":
		return "es-ES", "es", ""
	case "ru", "ru-ru":
		return "ru-RU", "ru", ""
	case "it", "it-it":
		return "it-IT", "it", ""
	case "pt", "pt-br", "pt-pt":
		return "pt-BR", "pt", ""
	}
	if clean != "" && isSafeLangTag(clean) {
		return clean, clean, ""
	}
	// Auto-detect from script when lang is omitted.
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			return "ja-JP", "ja", ""
		case unicode.In(r, unicode.Hangul):
			return "ko-KR", "ko", ""
		}
	}
	for _, r := range text {
		if unicode.In(r, unicode.Han) {
			return "zh-CN", "zh-CN", ""
		}
	}
	return "en-US", "en", "us"
}

func isSafeLangTag(tag string) bool {
	if len(tag) < 2 || len(tag) > 12 {
		return false
	}
	for _, r := range tag {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func cleanPronounceText(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "`*_\"'“”‘’")
	return strings.Join(strings.Fields(s), " ")
}

func splitTTSChunks(text string, maxRunes int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxRunes {
			if tail := strings.TrimSpace(string(runes)); tail != "" {
				chunks = append(chunks, tail)
			}
			break
		}
		cut := findClauseBreak(runes[:maxRunes])
		part := strings.TrimSpace(string(runes[:cut]))
		if part != "" {
			chunks = append(chunks, part)
		}
		runes = runes[cut:]
	}
	return chunks
}

func findClauseBreak(window []rune) int {
	// Prefer sentence-ending punctuation, then clause punctuation, then whitespace.
	for _, set := range []string{".!?。！？\n", ";:,；：，、", " \t"} {
		for i := len(window) - 1; i >= len(window)/3; i-- {
			if strings.ContainsRune(set, window[i]) {
				return i + 1
			}
		}
	}
	return len(window)
}

func buildFilenameSlug(text string) string {
	var b strings.Builder
	count := 0
	for _, r := range text {
		if count >= 24 {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(unicode.ToLower(r))
			count++
		case r == '-' || r == '_':
			if b.Len() > 0 {
				b.WriteRune('-')
				count++
			}
		case unicode.IsSpace(r) && b.Len() > 0:
			b.WriteRune('_')
			count++
		}
	}
	slug := strings.Trim(b.String(), "-_")
	if slug == "" {
		return "pronounce"
	}
	return slug
}

func randHex(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".media-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
