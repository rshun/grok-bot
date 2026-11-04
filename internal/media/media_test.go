package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func fakeMP3(tag string) []byte {
	// ID3v2 header (10 bytes, size=6) + tag + MPEG1 Layer3 frame sync (0xFF 0xFB) + padding >= 64 bytes.
	out := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 6}
	out = append(out, []byte("ABCDEF")[:6]...)
	out = append(out, 0xFF, 0xFB, 0x90, 0x64)
	out = append(out, []byte(strings.Repeat(tag, 20))...)
	return out
}

func makePNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 90, B: 180, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSynthesizeDictionaryAndAccentSelection(t *testing.T) {
	var ttsHits int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dict/schedule":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{
				"phonetic": "/ˈʃɛdjuːl/",
				"phonetics": [
					{"text": "/ˈskɛdʒuːl/", "audio": "` + srv.URL + `/audio/schedule-us.mp3"},
					{"text": "/ˈʃɛdjuːl/", "audio": "` + srv.URL + `/audio/schedule-uk.mp3"}
				]
			}]`))
		case r.URL.Path == "/audio/schedule-us.mp3":
			_, _ = w.Write(fakeMP3("US_AUDIO"))
		case r.URL.Path == "/audio/schedule-uk.mp3":
			_, _ = w.Write(fakeMP3("UK_AUDIO"))
		case r.URL.Path == "/tts":
			ttsHits++
			_, _ = w.Write(fakeMP3("TTS_" + r.URL.Query().Get("tl")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	syn := Synthesizer{
		Client:      srv.Client(),
		DictBaseURL: srv.URL + "/dict",
		TTSBaseURL:  srv.URL + "/tts",
	}
	outDir := t.TempDir()

	usRes, err := syn.Synthesize(context.Background(), outDir, "`schedule`", "en-US")
	if err != nil {
		t.Fatalf("Synthesize US failed: %v", err)
	}
	if usRes.Source != "dictionary" || usRes.Phonetic != "/ˈskɛdʒuːl/" || usRes.Name != "schedule_en-US.mp3" {
		t.Fatalf("unexpected US result: %+v", usRes)
	}
	ukRes, err := syn.Synthesize(context.Background(), outDir, "schedule", "en-GB")
	if err != nil {
		t.Fatalf("Synthesize UK failed: %v", err)
	}
	if ukRes.Source != "dictionary" || ukRes.Phonetic != "/ˈʃɛdjuːl/" || ukRes.Name != "schedule_en-GB.mp3" {
		t.Fatalf("unexpected UK result: %+v", ukRes)
	}
	if usRes.Path == ukRes.Path {
		t.Fatal("concurrent/same-turn audio files collided on disk")
	}
	usBytes, _ := os.ReadFile(usRes.Path)
	ukBytes, _ := os.ReadFile(ukRes.Path)
	if !bytes.Contains(usBytes, []byte("US_AUDIO")) || !bytes.Contains(ukBytes, []byte("UK_AUDIO")) {
		t.Fatal("wrong accent audio downloaded")
	}
	if ttsHits != 0 {
		t.Fatalf("dictionary hit should not invoke TTS, got %d TTS calls", ttsHits)
	}
}

func TestSynthesizeSentenceChunksAndRejectsHTML(t *testing.T) {
	var chunks []string
	returnHTML := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if returnHTML {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html><body>captcha required for this request........................................</body></html>"))
			return
		}
		if r.URL.Path == "/tts" {
			chunks = append(chunks, r.URL.Query().Get("q"))
			_, _ = w.Write(fakeMP3("CHUNK"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	syn := Synthesizer{
		Client:      srv.Client(),
		DictBaseURL: srv.URL + "/dict",
		TTSBaseURL:  srv.URL + "/tts",
	}
	longSentence := strings.Repeat("Practice makes standard pronunciation clear and natural. ", 5)
	res, err := syn.Synthesize(context.Background(), t.TempDir(), longSentence, "")
	if err != nil {
		t.Fatalf("sentence TTS failed: %v", err)
	}
	if res.Source != "tts" || res.Lang != "en-US" || len(chunks) < 2 {
		t.Fatalf("expected multi-chunk en-US TTS, got source=%s lang=%s chunks=%d", res.Source, res.Lang, len(chunks))
	}

	returnHTML = true
	if _, err := syn.Synthesize(context.Background(), t.TempDir(), "Bonjour tout le monde", "fr"); err == nil {
		t.Fatal("expected HTML payload masquerading as MP3 to be rejected")
	}
}

func TestDownloadWebImageValidatesFormatAndSize(t *testing.T) {
	goodPNG := makePNGBytes(t, 540, 720) // mobile portrait
	tinyPNG := makePNGBytes(t, 16, 16)   // tiny icon

	// Construct a valid VP8X WebP header with width = 256 (0xFF + 1) and height = 512 (0x01FF + 1)
	// to verify carry arithmetic across low 0xFF byte.
	webpVP8X := make([]byte, 36)
	copy(webpVP8X[0:4], "RIFF")
	copy(webpVP8X[8:12], "WEBP")
	copy(webpVP8X[12:16], "VP8X")
	webpVP8X[24], webpVP8X[25], webpVP8X[26] = 0xFF, 0x00, 0x00 // 255 + 1 = 256
	webpVP8X[27], webpVP8X[28], webpVP8X[29] = 0xFF, 0x01, 0x00 // 511 + 1 = 512

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wallpaper.png":
			_, _ = w.Write(goodPNG)
		case "/poster.webp":
			_, _ = w.Write(webpVP8X)
		case "/pixel.png":
			_, _ = w.Write(tinyPNG)
		case "/page.jpg":
			_, _ = w.Write([]byte("<!doctype html><html>not an image</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	img, err := DownloadWebImage(context.Background(), srv.Client(), outDir, srv.URL+"/wallpaper.png")
	if err != nil {
		t.Fatalf("DownloadWebImage good PNG failed: %v", err)
	}
	if img.MIME != "image/png" || img.Width != 540 || img.Height != 720 || img.Name != "wallpaper.png" {
		t.Fatalf("unexpected image metadata: %+v", img)
	}

	webpImg, err := DownloadWebImage(context.Background(), srv.Client(), outDir, srv.URL+"/poster.webp")
	if err != nil {
		t.Fatalf("DownloadWebImage VP8X WebP failed: %v", err)
	}
	if webpImg.Width != 256 || webpImg.Height != 512 || webpImg.Name != "poster.webp" {
		t.Fatalf("unexpected VP8X dimensions: %+v", webpImg)
	}

	if _, err := DownloadWebImage(context.Background(), srv.Client(), outDir, srv.URL+"/pixel.png"); err == nil {
		t.Fatal("expected tiny 16x16 tracking pixel to be rejected")
	}
	if _, err := DownloadWebImage(context.Background(), srv.Client(), outDir, srv.URL+"/page.jpg"); err == nil {
		t.Fatal("expected HTML response with .jpg URL to be rejected")
	}
	if _, err := DownloadWebImage(context.Background(), srv.Client(), outDir, srv.URL+"/nsfw_porn_image.png"); !errors.Is(err, ErrUnsafeMedia) {
		t.Fatalf("expected unsafe URL to return ErrUnsafeMedia, got %v", err)
	}
}

func TestPromptSafetyAndAuditorPlaceholder(t *testing.T) {
	blocked := []string{
		"画一张全裸美女图",
		"generate an nsfw porn image",
		"来一张血腥斩首图片",
		"画一个冰毒制作教程图",
	}
	for _, s := range blocked {
		if err := CheckPromptSafety(s); !errors.Is(err, ErrUnsafeMedia) {
			t.Errorf("expected %q to be blocked, got %v", s, err)
		}
		if !HasImageRequestIntent(s) {
			t.Errorf("expected %q to match image request intent", s)
		}
	}

	allowed := []string{
		"帮我画一张赛博朋克风格的东京雨夜手机壁纸",
		"generate a cute watercolor illustration of a golden retriever",
		"给我找一张阿尔卑斯山雪景的高清图片",
		"how to pronounce serendipity and entrepreneur",
		"Linux kill process by port",
	}
	for _, s := range allowed {
		if err := CheckPromptSafety(s); err != nil {
			t.Errorf("benign input %q falsely blocked: %v", s, err)
		}
	}

	var auditor ImageAuditor = NoopImageAuditor{}
	ok, reason, err := auditor.AuditImage(context.Background(), "/tmp/test.jpg", "image/jpeg")
	if err != nil || !ok || reason != "" {
		t.Fatalf("NoopImageAuditor unexpected result: ok=%v reason=%q err=%v", ok, reason, err)
	}
}
