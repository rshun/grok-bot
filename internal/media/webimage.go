package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// MaxWebImageBytes caps downloaded web images at 8 MiB so they fit under both
	// Telegram's 10 MiB sendPhoto limit and Discord's 10 MiB bot upload limit.
	MaxWebImageBytes = 8 << 20

	minImageDim = 80
	maxImageDim = 8192
)

var errBlockedHost = errors.New("blocked non-public host")

// WebImageResult describes a validated image downloaded from a public URL into MediaDir.
type WebImageResult struct {
	Path   string
	Name   string
	MIME   string
	Width  int
	Height int
	Size   int64
}

// DownloadWebImage safely downloads an image URL, verifies magic-byte format and mobile-usable
// pixel bounds, and stores it inside outDir for sending to Telegram or Discord.
func DownloadWebImage(ctx context.Context, client *http.Client, outDir, rawURL string) (WebImageResult, error) {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return WebImageResult{}, errors.New("请提供有效的 http/https 图片直链。")
	}
	if err := CheckPromptSafety(trimmed); err != nil {
		return WebImageResult{}, err
	}
	if outDir == "" {
		return WebImageResult{}, errors.New("媒体缓存目录未配置。")
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return WebImageResult{}, errors.New("创建媒体缓存目录失败。")
	}
	if client == nil {
		client = NewSafeHTTPClient(12 * time.Second)
	}

	data, err := downloadBytes(ctx, client, trimmed, MaxWebImageBytes)
	if err != nil {
		return WebImageResult{}, errors.New("下载该图片链接失败（链接可能不可达、受防盗链限制或超过 8MB）。")
	}

	mime, ext, ok := DetectImageMIME(data)
	if !ok {
		return WebImageResult{}, errors.New("该链接返回的不是受支持的图片格式（仅支持 JPG/PNG/WebP/GIF 图片直链，不能是网页地址）。")
	}
	w, h, err := decodeImageDimensions(data, mime)
	if err != nil {
		return WebImageResult{}, errors.New("图片内容损坏或无法解析尺寸，请换一张图片链接。")
	}
	if w < minImageDim || h < minImageDim {
		return WebImageResult{}, fmt.Errorf("图片尺寸过小（%dx%d），不适合移动端查看，请选择更清晰的图片。", w, h)
	}
	if w > maxImageDim || h > maxImageDim {
		return WebImageResult{}, fmt.Errorf("图片分辨率过大（%dx%d），请选择常规尺寸图片。", w, h)
	}

	baseSlug := cleanImageURLSlug(u.Path)
	displayName := baseSlug + ext
	diskName := fmt.Sprintf("%s_%s%s", baseSlug, randHex(4), ext)
	diskPath := filepath.Join(outDir, diskName)
	if err := writeFileAtomic(diskPath, data); err != nil {
		return WebImageResult{}, errors.New("保存图片缓存失败。")
	}

	return WebImageResult{
		Path:   diskPath,
		Name:   displayName,
		MIME:   mime,
		Width:  w,
		Height: h,
		Size:   int64(len(data)),
	}, nil
}

// DetectImageMIME inspects magic bytes and returns the canonical MIME type and extension.
func DetectImageMIME(data []byte) (mime, ext string, ok bool) {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg", ".jpg", true
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return "image/png", ".png", true
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif", ".gif", true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", ".webp", true
	default:
		return "", "", false
	}
}

func decodeImageDimensions(data []byte, mime string) (int, int, error) {
	if mime == "image/webp" {
		return decodeWebPDimensions(data)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// decodeWebPDimensions parses VP8, VP8L, and VP8X RIFF headers without external dependencies.
func decodeWebPDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, errors.New("bad webp header")
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8X":
		// 24-bit width-1 at 24..26, height-1 at 27..29 (little-endian).
		w := 1 + (int(data[24]) | int(data[25])<<8 | int(data[26])<<16)
		h := 1 + (int(data[27]) | int(data[28])<<8 | int(data[29])<<16)
		return w, h, nil
	case "VP8L":
		if data[20] != 0x2F {
			return 0, 0, errors.New("bad vp8l signature")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		w := int(bits&0x3FFF) + 1
		h := int((bits>>14)&0x3FFF) + 1
		return w, h, nil
	case "VP8 ":
		// Keyframe start code 0x9D 0x01 0x2A at 23..25.
		if data[23] != 0x9D || data[24] != 0x01 || data[25] != 0x2A {
			return 0, 0, errors.New("bad vp8 start code")
		}
		w := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3FFF)
		h := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3FFF)
		return w, h, nil
	default:
		return 0, 0, errors.New("unknown webp chunk")
	}
}

func cleanImageURLSlug(urlPath string) string {
	base := path.Base(urlPath)
	if ext := path.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	slug := buildFilenameSlug(base)
	if slug == "" || slug == "pronounce" {
		return "image"
	}
	return slug
}

// NewSafeHTTPClient builds an HTTP client that refuses connections or redirects to loopback,
// RFC1918 private, link-local, CGNAT, or internal hosts (SSRF protection).
func NewSafeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   4 * time.Second,
		KeepAlive: 15 * time.Second,
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if isBlockedHostname(host) {
				return nil, errBlockedHost
			}
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addrs) == 0 {
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
		ResponseHeaderTimeout: 6 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
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
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || h == "localhost" {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return !isPublicAddr(ip)
	}
	return false
}

func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return isPublicAddr(addr.Unmap())
}

func isPublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() ||
		addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return false
	}
	if cgnat := netip.MustParsePrefix("100.64.0.0/10"); cgnat.Contains(addr) {
		return false
	}
	if bench := netip.MustParsePrefix("198.18.0.0/15"); bench.Contains(addr) {
		return false
	}
	return true
}
