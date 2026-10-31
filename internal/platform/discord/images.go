package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if strings.TrimSpace(a.ContentType) != "" {
		return platform.ImageMIME(a.ContentType)
	}
	return platform.ImageMIMEByName(a.Filename)
}

// splitAttachments splits attachments into vision images, general document files, and whether anything exceeded caps.
func splitAttachments(atts []*discordgo.MessageAttachment) (images []platform.Image, files []platform.Attachment, skipped bool) {
	for _, a := range atts {
		if a == nil {
			continue
		}
		url := a.URL
		if mime := imageMIME(a); mime != "" {
			if len(images) >= platform.MaxImages {
				skipped = true
				continue
			}
			images = append(images, platform.Image{
				MIME: mime,
				Name: a.Filename,
				Size: int64(a.Size),
				Load: func(ctx context.Context) ([]byte, error) { return download(ctx, url, platform.ReadImage) },
			})
			continue
		}
		if len(files) >= platform.MaxFiles {
			skipped = true
			continue
		}
		name := strings.TrimSpace(a.Filename)
		if name == "" {
			name = "attachment"
		}
		files = append(files, platform.Attachment{
			MIME: strings.TrimSpace(a.ContentType),
			Name: name,
			Size: int64(a.Size),
			Load: func(ctx context.Context) ([]byte, error) { return download(ctx, url, platform.ReadAttachment) },
		})
	}
	return images, files, skipped
}

// download fetches one attachment. The CDN link carries a signature, so no returned error
// contains the URL.
func download(ctx context.Context, url string, readBody func(io.Reader) ([]byte, error)) ([]byte, error) {
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
	data, err := readBody(resp.Body)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, platform.ErrImageTooLarge), errors.Is(err, platform.ErrFileTooLarge):
		return nil, err
	case err != nil:
		return nil, errors.New("discord download failed")
	}
	return data, nil
}
