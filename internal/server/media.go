package server

import (
	"context"

	"fujipic/internal/imagefile"
	"fujipic/internal/model"
)

type mediaView struct {
	OriginalURL     string `json:"original_url"`
	PreviewURL      string `json:"preview_url,omitempty"`
	ProcessingState string `json:"processing_state"`
}

func (s *Server) imageMedia(ctx context.Context, img *model.Image) mediaView {
	v := mediaView{OriginalURL: s.uploads.URL(img), ProcessingState: "ready"}
	if !imagefile.NeedsPreview(img.MIME) {
		v.PreviewURL = v.OriginalURL
		return v
	}
	status, err := s.store.MediaStatus(ctx, img.ID)
	if err != nil {
		v.ProcessingState = "failed"
		return v
	}
	v.ProcessingState = status.State
	if status.State == "ready" && status.PreviewKey != "" {
		v.PreviewURL = s.cfg.PublicBaseURL + "/" + status.PreviewKey
	}
	return v
}

func (s *Server) imageView(ctx context.Context, img model.Image) imageView {
	m := s.imageMedia(ctx, &img)
	v := imageView{Image: img, mediaView: m, URL: m.PreviewURL, Name: displayName(img.OriginalName)}
	if v.URL != "" {
		v.Markdown = markdownFor(img.OriginalName, v.URL)
		if !imagefile.NeedsPreview(img.MIME) {
			v.AlternateURLs = s.uploads.AlternateURLs(&img)
		} else {
			key := v.URL[len(s.cfg.PublicBaseURL):]
			for _, base := range s.cfg.PublicBaseAliases {
				v.AlternateURLs = append(v.AlternateURLs, base+key)
			}
		}
	}
	return v
}

func (s *Server) mediaAliases(ctx context.Context, img *model.Image) []string {
	return s.imageView(ctx, *img).AlternateURLs
}
