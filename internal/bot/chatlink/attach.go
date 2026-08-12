package chatlink

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/llm"
)

// What one message may hand a model.
//
// These are facts about a model request, not about storage, which is why they
// live here and not in the store: base64 inflates a payload by 4/3 and the
// provider caps the whole request, so a turn may carry a few images and none of
// them may be large. Everything else is described rather than sent.
const (
	maxImages    = 4
	maxImageSize = 3 << 20
)

// viewableImageMimes are the types multimodal models accept. It is deliberately
// narrower than "an image": an SVG is a document that can carry script.
var viewableImageMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// Attachments resolves a message's files. Viewable images come back for a
// multimodal turn, and every attachment - including the ones that were skipped
// - is described in the note, so the model knows what arrived and never has to
// guess whether it can see a file.
func (t *Transport) Attachments(ctx context.Context, ids []string) ([]llm.Image, string) {
	var images []llm.Image
	var lines []string
	for _, id := range ids {
		att, body, err := t.store.Attachment(ctx, t.self, id)
		if err != nil {
			t.logger().Warn("could not read an attachment", "attachment", id, "err", err)
			lines = append(lines, "- (attachment unavailable)")
			continue
		}
		label := fmt.Sprintf("%s (%s, %s)",
			chat.SanitizeField(att.Filename), chat.SanitizeField(att.Mime), chat.HumanSize(att.Size))
		switch {
		case !viewableImageMimes[strings.ToLower(att.Mime)]:
			lines = append(lines, "- "+label+" - not an image, so its contents are unavailable")
		case att.Size > maxImageSize:
			lines = append(lines, "- "+label+" - image too large, not attached")
		case len(images) >= maxImages:
			lines = append(lines, "- "+label+" - per-message image limit reached, not attached")
		default:
			// The stored mime was sniffed from the bytes at upload, but sniff
			// again: what reaches the provider must be what these bytes are, or
			// the whole turn fails with a provider-side invalid_request.
			sniffed := http.DetectContentType(body)
			if !viewableImageMimes[sniffed] {
				lines = append(lines, "- "+label+" - contents are not an image, not attached")
				continue
			}
			images = append(images, llm.Image{
				MediaType: sniffed,
				Data:      base64.StdEncoding.EncodeToString(body),
			})
			lines = append(lines, "- "+label+" - attached as an image")
		}
	}
	if len(lines) == 0 {
		return images, ""
	}
	return images, "Attachments on this message:\n" + strings.Join(lines, "\n")
}
