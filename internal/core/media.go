package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/media"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
)

const mediaDeliveredFallback = "已为你生成并发送。"

var (
	reMarkdownLocalImg  = regexp.MustCompile(`!\[[^\]]*\]\((?:\./)?images/[^)\s]+\)`)
	reMarkdownLocalLink = regexp.MustCompile(`\[([^\]]+)\]\((?:\./)?images/[^)\s]+\)`)
)

func (s *Service) synthesizer() media.Synthesizer {
	if s.Synthesizer != nil {
		return *s.Synthesizer
	}
	return media.Synthesizer{Client: s.MediaHTTP}
}

func (s *Service) imageAuditor() media.ImageAuditor {
	if s.ImageAuditor != nil {
		return s.ImageAuditor
	}
	return media.NoopImageAuditor{}
}

func (s *Service) effectivePolicy() (sandbox.Policy, bool) {
	if s.Sandbox != nil {
		p := s.Sandbox.Policy
		if p.MediaDir == "" && s.MediaDir != "" {
			p.MediaDir = s.MediaDir
		}
		return p, true
	}
	if s.MediaDir != "" {
		return sandbox.Policy{MediaDir: s.MediaDir}, true
	}
	return sandbox.Policy{}, false
}

func (s *Service) openSendableFile(path string) (*os.File, int64, error) {
	p, ok := s.effectivePolicy()
	if !ok {
		return nil, 0, os.ErrPermission
	}
	return p.OpenSendable(path)
}

func (s *Service) canGenerateImages(out platform.Replier) bool {
	if _, ok := out.(platform.FileSender); !ok {
		return false
	}
	return s.Sandbox != nil || s.MediaDir != ""
}

func (s *Service) pronounceCallback(limit int64) func(ctx context.Context, text, lang string) (actions.File, string, error) {
	return func(ctx context.Context, text, lang string) (actions.File, string, error) {
		res, err := s.synthesizer().Synthesize(ctx, s.MediaDir, text, lang)
		if err != nil {
			return actions.File{}, "", &actions.ToolError{Msg: err.Error()}
		}
		if res.Size > limit {
			return actions.File{}, "", &actions.ToolError{Msg: errTooBig}
		}
		caption := fmt.Sprintf("🔊 %s (%s)", res.Title, res.Lang)
		detail := fmt.Sprintf("Queued standard pronunciation audio %s (%s, source=%s).", res.Name, res.Lang, res.Source)
		if res.Phonetic != "" {
			caption = fmt.Sprintf("🔊 %s %s (%s)", res.Title, res.Phonetic, res.Lang)
			detail = fmt.Sprintf("Queued standard pronunciation audio %s (%s, IPA: %s, source=%s).", res.Name, res.Lang, res.Phonetic, res.Source)
		}
		return actions.File{
			Path:    res.Path,
			Name:    res.Name,
			Caption: caption,
			Size:    res.Size,
		}, detail, nil
	}
}

func (s *Service) fetchWebImageCallback(limit int64) func(ctx context.Context, rawURL, caption string) (actions.File, error) {
	return func(ctx context.Context, rawURL, caption string) (actions.File, error) {
		if err := media.CheckPromptSafety(rawURL); err != nil {
			return actions.File{}, &actions.ToolError{Msg: err.Error()}
		}
		if err := media.CheckPromptSafety(caption); err != nil {
			return actions.File{}, &actions.ToolError{Msg: err.Error()}
		}
		img, err := media.DownloadWebImage(ctx, s.MediaHTTP, s.MediaDir, rawURL)
		if err != nil {
			return actions.File{}, &actions.ToolError{Msg: err.Error()}
		}
		if img.Size > limit {
			return actions.File{}, &actions.ToolError{Msg: errTooBig}
		}
		return actions.File{
			Path:    img.Path,
			Name:    img.Name,
			Caption: caption,
			Size:    img.Size,
		}, nil
	}
}

// wrapToolSafetyMonitor inspects image_gen / image_edit prompts streamed during a turn and marks
// unsafeFlag when a prohibited prompt is detected, while forwarding events to inner onTool.
// When inner is nil and imageGen is false, nil is returned to avoid unnecessary callbacks.
func wrapToolSafetyMonitor(inner func(grok.ToolUse), imageGen bool, unsafeFlag *atomic.Bool) func(grok.ToolUse) {
	if inner == nil && !imageGen {
		return nil
	}
	return func(tu grok.ToolUse) {
		if (tu.Name == "image_gen" || tu.Name == "image_edit") && len(tu.Input) > 0 {
			var in struct {
				Prompt string `json:"prompt"`
			}
			if json.Unmarshal(tu.Input, &in) == nil && media.CheckPromptSafety(in.Prompt) != nil {
				unsafeFlag.Store(true)
			}
		}
		if inner != nil {
			inner(tu)
		}
	}
}

func (s *Service) appendGeneratedImages(existing []actions.File, genPaths []string, out platform.Replier) []actions.File {
	if len(genPaths) == 0 {
		return existing
	}
	sender, ok := out.(platform.FileSender)
	if !ok {
		return existing
	}
	policy, ok := s.effectivePolicy()
	if !ok {
		return existing
	}
	limit := sender.FileLimit()
	seen := make(map[string]bool, len(existing))
	for _, f := range existing {
		seen[filepath.Clean(f.Path)] = true
		if real, err := filepath.EvalSymlinks(f.Path); err == nil {
			seen[real] = true
		}
	}
	result := append([]actions.File(nil), existing...)
	for _, rawPath := range genPaths {
		if len(result) >= 10 {
			break
		}
		var (
			real string
			size int64
			err  error
		)
		if s.Sandbox != nil {
			real, size, err = policy.CheckGeneratedMedia(rawPath)
			if err != nil {
				// Also accept paths inside writable directories when sandboxed FileTools are active.
				real, size, err = policy.CheckSendable(rawPath)
			}
		} else {
			real, size, err = policy.CheckGeneratedMedia(rawPath)
		}
		if err != nil || size <= 0 || size > limit {
			continue
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		result = append(result, actions.File{
			Path: real,
			Name: filepath.Base(real),
			Size: size,
		})
	}
	return result
}

func cleanLocalImageRefs(text string) string {
	cleaned := reMarkdownLocalImg.ReplaceAllString(text, "")
	cleaned = reMarkdownLocalLink.ReplaceAllStringFunc(cleaned, func(match string) string {
		sub := reMarkdownLocalLink.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}
		label := strings.TrimSpace(sub[1])
		lower := strings.ToLower(label)
		if strings.HasPrefix(lower, "images/") || strings.HasPrefix(lower, "./images/") {
			return ""
		}
		return label
	})
	return strings.TrimSpace(cleaned)
}

func (s *Service) auditImageBeforeSend(ctx context.Context, path, name string) (allowed bool, reason string) {
	mime := imageMIMEByExt(name)
	if mime == "" {
		return true, ""
	}
	ok, msg, err := s.imageAuditor().AuditImage(ctx, path, mime)
	if err != nil {
		return false, "图片未通过视觉安全检查，已拦截发送。"
	}
	if !ok {
		if strings.TrimSpace(msg) != "" {
			return false, strings.TrimSpace(msg)
		}
		return false, media.ErrUnsafeMedia.Error()
	}
	return true, ""
}

func imageMIMEByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return ""
	}
}
