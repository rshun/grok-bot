package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

// imageClient downloads attachments; the per-request context ends a slow transfer early.
var imageClient = &http.Client{Timeout: 2 * time.Minute}

// imageMIME returns the image type of an attachment, or "" when it is not an image Grok takes.
// A stated content type decides; the file extension is used only when there is none.
func imageMIME(a *discordgo.MessageAttachment) string {
	if mime := strings.TrimSpace(strings.SplitN(a.ContentType, ";", 2)[0]); mime != "" {
		mime = strings.ToLower(mime)
		switch mime {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
			return mime
		}
		return ""
	}
	return platform.ImageMIMEByName(a.Filename)
}

// attachmentImages splits the attachments into pictures and a flag for everything left out.
func attachmentImages(atts []*discordgo.MessageAttachment) (images []platform.Image, skipped bool) {
	for _, a := range atts {
		if a == nil {
			continue
		}
		mime := imageMIME(a)
		if mime == "" || len(images) >= platform.MaxImages {
			skipped = true
			continue
		}
		url := a.URL
		images = append(images, platform.Image{
			MIME: mime,
			Name: a.Filename,
			Size: int64(a.Size),
			Load: func(ctx context.Context) ([]byte, error) { return download(ctx, url) },
		})
	}
	return images, skipped
}

// download fetches one attachment. The CDN link carries a signature, so no returned error
// contains the URL.
func download(ctx context.Context, url string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("discord download failed")
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("discord download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord download failed: status %d", resp.StatusCode)
	}
	data, err := platform.ReadImage(resp.Body)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, platform.ErrImageTooLarge):
		return nil, err
	case err != nil:
		return nil, errors.New("discord download failed")
	}
	return data, nil
}
