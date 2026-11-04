// Package media provides standard pronunciation synthesis, safe web image retrieval,
// pre-generation prompt compliance checks, and the post-generation visual audit hook.
package media

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// ErrUnsafeMedia is returned when a media generation or image search request violates safety rules.
var ErrUnsafeMedia = errors.New("抱歉，根据安全合规规范，无法生成或发送涉及淫秽色情、暴力血腥或违法违规内容的图片。")

var (
	// Explicit NSFW, pornographic, CSAM, extreme gore/violence, and illegal fabrication patterns.
	// Word boundaries are used on Latin tokens so ordinary technical or medical words do not false-trigger.
	reUnsafeMediaEN = regexp.MustCompile(`(?i)\b(` +
		`nsfw|porn|pornography|pornographic|hentai|erotica|xxx|` +
		`nude|nudity|naked\s+(?:girl|woman|man|boy|body|person|people|photo|pic|image)|` +
		`topless\s+(?:girl|woman|photo|pic|model)|` +
		`explicit\s+sex|sex\s+act|blowjob|handjob|gangbang|orgy|incest|rape|` +
		`csam|child\s+porn|underage\s+(?:nude|sex|girl|boy)|loli\s+hentai|shota\s+hentai|` +
		`gore|beheading|decapitation|dismemberment|mutilated\s+corpse|snuff\s+film|terrorist\s+execution|` +
		`meth\s+recipe|bomb\s+making|explosive\s+recipe|forged\s+passport|counterfeit\s+currency` +
		`)\b`)

	unsafeMediaZH = []string{
		// 淫秽色情与未成年人保护
		"淫秽", "色情", "黄色图片", "黄色漫画", "成人图片", "成人漫画", "羞羞图",
		"全裸", "裸照", "裸体图", "裸体照", "裸女", "裸男", "露点", "私处特写", "无码", "里番", "工口", "本子图",
		"性交", "口交", "群交", "肛交", "乱伦", "强奸", "迷奸", "轮奸",
		"儿童色情", "幼女色情", "幼童裸", "未成年裸",
		// 极端暴力血腥与恐怖主义
		"斩首", "肢解", "碎尸", "分尸", "虐杀", "活剥", "血腥尸体", "恐怖袭击教程", "自杀教程", "自残教程",
		// 严重违法犯罪
		"制毒教程", "冰毒制作", "海洛因制作", "炸药制作", "炸弹制作", "枪支制造图纸", "伪造身份证", "伪造公章", "伪造货币",
	}

	reTokenSeparators = regexp.MustCompile(`[_./\-:?&=+%]+`)

	reImageIntentEN = regexp.MustCompile(`(?i)\b(` +
		`draw|paint|sketch|illustrate|wallpaper|` +
		`(?:generate|create|make|render|find|search|send|show)\b[^\n.!?]{0,64}\b(?:image|images|picture|pictures|photo|photos|pic|pics|illustration|poster|avatar|wallpaper)` +
		`)\b`)

	reImageIntentZH = regexp.MustCompile(`(?:来|发|找|搜|给|求|生成|制作)(?:一)?(?:张|幅|个)[^\n。！？]{0,36}(?:图|图片|照片|壁纸|海报|插画)`)

	imageIntentZH = []string{
		"画一", "画张", "画个", "画幅", "帮我画", "给我画",
		"生成图片", "生成一张", "生成张", "生成一幅", "生图", "AI生图", "ai生图", "文生图", "改图", "P图", "p图",
		"配图", "壁纸", "插画", "海报图",
		"来张图", "来一张图", "发张图", "发一张图", "找张图", "找一张图", "搜张图", "搜一张图", "找图片", "搜图片",
	}
)

// CheckPromptSafety verifies that a media prompt, image search query, URL, or caption
// does not contain pornographic/NSFW, gory/violent, or illegal content.
func CheckPromptSafety(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	lower := strings.ToLower(trimmed)
	for _, kw := range unsafeMediaZH {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return ErrUnsafeMedia
		}
	}
	if reUnsafeMediaEN.MatchString(trimmed) {
		return ErrUnsafeMedia
	}
	normalized := reTokenSeparators.ReplaceAllString(trimmed, " ")
	if normalized != trimmed && reUnsafeMediaEN.MatchString(normalized) {
		return ErrUnsafeMedia
	}
	return nil
}

// HasImageRequestIntent reports whether a user message appears to ask for generating,
// editing, or searching/sending an image.
func HasImageRequestIntent(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	for _, kw := range imageIntentZH {
		if strings.Contains(trimmed, kw) {
			return true
		}
	}
	if reImageIntentZH.MatchString(trimmed) {
		return true
	}
	return reImageIntentEN.MatchString(trimmed)
}

// ImageAuditor is the post-generation / pre-delivery visual safety review hook.
// Every outgoing image passes through AuditImage before being uploaded to Telegram or Discord.
//
// TODO(next-phase): Plug in a zero-context multimodal vision compliance auditor here
// to inspect rendered pixel content before delivery. This iteration intentionally uses
// NoopImageAuditor as a placeholder per project roadmap.
type ImageAuditor interface {
	AuditImage(ctx context.Context, path, mime string) (allowed bool, reason string, err error)
}

// NoopImageAuditor is the placeholder implementation of ImageAuditor for the current iteration.
type NoopImageAuditor struct{}

// AuditImage implements ImageAuditor as a pass-through placeholder.
func (NoopImageAuditor) AuditImage(_ context.Context, _, _ string) (bool, string, error) {
	return true, "", nil
}
