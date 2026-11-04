package core

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/media"
	"github.com/rshun/grok-bot/internal/sandbox"
)

func testValidMP3() []byte {
	out := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 4, 'T', 'E', 'S', 'T', 0xFF, 0xFB, 0x90, 0x64}
	out = append(out, []byte(strings.Repeat("AUDIO", 20))...)
	return out
}

func testValidPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 120, G: 180, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type rejectAuditor struct {
	reason string
}

func (r rejectAuditor) AuditImage(_ context.Context, _, _ string) (bool, string, error) {
	return false, r.reason, nil
}

func TestPronounceAndWebImageEndToEnd(t *testing.T) {
	pngBytes := testValidPNG(t, 480, 720)
	mp3Bytes := testValidMP3()

	var mediaSrv *httptest.Server
	mediaSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dict/serendipity":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"phonetic":"/ˌsɛrənˈdɪpɪti/","phonetics":[{"text":"/ˌsɛrənˈdɪpɪti/","audio":"` + mediaSrv.URL + `/audio/serendipity-us.mp3"}]}]`))
		case "/audio/serendipity-us.mp3":
			_, _ = w.Write(mp3Bytes)
		case "/tts":
			_, _ = w.Write(mp3Bytes)
		case "/aurora.png":
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mediaSrv.Close()

	svc, engine, mcpTS := actionsService(t)
	svc.MediaDir = filepath.Join(t.TempDir(), "media")
	svc.MediaHTTP = mediaSrv.Client()
	svc.Synthesizer = &media.Synthesizer{
		Client:      mediaSrv.Client(),
		DictBaseURL: mediaSrv.URL + "/dict",
		TTSBaseURL:  mediaSrv.URL + "/tts",
	}

	var pText, wText, unsafeText string
	var pErr, wErr, unsafeErr bool
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.Pronounce || !opts.SendWebImage || !opts.ImageGen {
			t.Errorf("expected Pronounce/SendWebImage/ImageGen enabled, got pronounce=%v webImg=%v imgGen=%v", opts.Pronounce, opts.SendWebImage, opts.ImageGen)
		}
		tok := tokenFrom(t, opts.Env)
		pText, pErr, _ = rpc(t, mcpTS, tok, "tools/call", map[string]any{
			"name":      "pronounce",
			"arguments": map[string]string{"text": "serendipity", "lang": "en-US"},
		})
		wText, wErr, _ = rpc(t, mcpTS, tok, "tools/call", map[string]any{
			"name":      "send_web_image",
			"arguments": map[string]string{"url": mediaSrv.URL + "/aurora.png", "caption": "极光壁纸"},
		})
		unsafeText, unsafeErr, _ = rpc(t, mcpTS, tok, "tools/call", map[string]any{
			"name":      "send_web_image",
			"arguments": map[string]string{"url": mediaSrv.URL + "/aurora.png", "caption": "色情裸照"},
		})
		return grok.HeadlessResult{Text: "serendipity 读作 /ˌsɛrənˈdɪpɪti/，附标准读音与图片。", SessionID: opts.SessionID}, nil
	}

	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 10 << 20}, "serendipity 怎么读？顺便发张极光图")
	ev.waitFor(t, 4)
	time.Sleep(30 * time.Millisecond)

	if pErr || !strings.Contains(pText, "IPA: /ˌsɛrənˈdɪpɪti/") {
		t.Fatalf("pronounce call failed: isErr=%v text=%q", pErr, pText)
	}
	if wErr || !strings.Contains(wText, "aurora.png") {
		t.Fatalf("send_web_image call failed: isErr=%v text=%q", wErr, wText)
	}
	if !unsafeErr || !strings.Contains(unsafeText, "安全合规") {
		t.Fatalf("expected unsafe caption to be blocked, got isErr=%v text=%q", unsafeErr, unsafeText)
	}

	got := ev.snapshot()
	if len(got) != 4 {
		t.Fatalf("unexpected events: %#v", got)
	}
	if !strings.HasPrefix(got[2], "file:serendipity_en-US.mp3|🔊 serendipity /ˌsɛrənˈdɪpɪti/ (en-US)|5|") {
		t.Fatalf("unexpected pronunciation file event: %q", got[2])
	}
	if !strings.HasPrefix(got[3], "file:aurora.png|极光壁纸|5|") {
		t.Fatalf("unexpected web image file event: %q", got[3])
	}
}

func TestAutoCollectGeneratedImagesAndCleanLocalLinks(t *testing.T) {
	svc, engine, _ := actionsService(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dataDir := filepath.Join(root, "data")
	mediaDir := filepath.Join(dataDir, "media")
	sessImg := filepath.Join(home, ".grok", "sessions", "ws", "sess-9", "images", "1.jpg")
	if err := os.MkdirAll(filepath.Dir(sessImg), 0o750); err != nil {
		t.Fatal(err)
	}
	imgBytes := testValidPNG(t, 400, 600)
	if err := os.WriteFile(sessImg, imgBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate an unsandboxed OS (e.g. Windows: Sandbox.Enabled == false, FileTools == false):
	// generated session images must still be delivered automatically without exposing send_file.
	svc.MediaDir = mediaDir
	svc.Sandbox = &sandbox.Manager{
		Path:    filepath.Join(home, ".grok", "sandbox.toml"),
		Enabled: false,
		Policy: sandbox.Policy{
			Home:     home,
			DataDir:  dataDir,
			MediaDir: mediaDir,
		},
	}

	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{
			Text:            "这是为你生成的雨夜手机壁纸 ![preview](images/1.jpg) 见 [images/1.jpg](images/1.jpg)",
			SessionID:       opts.SessionID,
			GeneratedImages: []string{sessImg},
		}, nil
	}

	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 10 << 20}, "帮我画一张雨夜手机壁纸")
	ev.waitFor(t, 3)

	got := ev.snapshot()
	if strings.Contains(got[1], "images/1.jpg") {
		t.Fatalf("local relative link was not stripped from mobile reply: %q", got[1])
	}
	if !strings.HasPrefix(got[2], "file:1.jpg||5|") {
		t.Fatalf("generated image was not auto-delivered: %#v", got)
	}
}

func TestUnsafeMediaPreTurnAndStreamAndAuditorPlaceholder(t *testing.T) {
	svc, engine, _ := actionsService(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dataDir := filepath.Join(root, "data")
	mediaDir := filepath.Join(dataDir, "media")
	svc.MediaDir = mediaDir
	svc.Sandbox = &sandbox.Manager{
		Path:    filepath.Join(home, ".grok", "sandbox.toml"),
		Enabled: false,
		Policy:  sandbox.Policy{Home: home, DataDir: dataDir, MediaDir: mediaDir},
	}

	// 1. Pre-turn user request safety interception (must not call Engine.Reply at all).
	ev1 := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev1}, limit: 10 << 20}, "帮我画一张全裸色情图片")
	ev1.waitFor(t, 2)
	if len(engine.calls) != 0 {
		t.Fatalf("unsafe image request should be blocked before calling Grok, got %d calls", len(engine.calls))
	}
	if !strings.Contains(ev1.snapshot()[1], "安全合规") {
		t.Fatalf("unexpected refusal message: %#v", ev1.snapshot())
	}

	// 2. Mid-stream unsafe image_gen tool prompt interception.
	sessImg := filepath.Join(home, ".grok", "sessions", "ws", "sess-bad", "images", "1.jpg")
	_ = os.MkdirAll(filepath.Dir(sessImg), 0o750)
	_ = os.WriteFile(sessImg, testValidPNG(t, 300, 450), 0o600)

	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.OnTool != nil {
			opts.OnTool(toolUse("image_gen", `{"prompt":"explicit porn nude photo","aspect_ratio":"2:3"}`))
		}
		return grok.HeadlessResult{
			Text:            "done",
			SessionID:       opts.SessionID,
			GeneratedImages: []string{sessImg},
		}, nil
	}
	ev2 := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev2}, limit: 10 << 20}, "随便来一张图")
	ev2.waitFor(t, 2)
	time.Sleep(30 * time.Millisecond)
	snap2 := ev2.snapshot()
	if len(snap2) != 2 || !strings.Contains(snap2[1], "安全合规") {
		t.Fatalf("expected unsafe stream image_gen to be blocked without sending file, got %#v", snap2)
	}

	// 3. Post-generation ImageAuditor hook placeholder verification.
	svc.ImageAuditor = rejectAuditor{reason: "视觉终审拦截：检测到违规画面"}
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{
			Text:            "已生成图片。",
			SessionID:       opts.SessionID,
			GeneratedImages: []string{sessImg},
		}, nil
	}
	ev3 := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev3}, limit: 10 << 20}, "画一张风景图")
	ev3.waitFor(t, 3)
	snap3 := ev3.snapshot()
	if snap3[2] != "send:视觉终审拦截：检测到违规画面" {
		t.Fatalf("expected ImageAuditor hook to intercept file delivery, got %#v", snap3)
	}
}
