package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"fujipic/internal/media"
)

func TestClassifiedUploadErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{media.ErrFeature, 415, "unsupported_image_variant"},
		{media.ErrSVGFeature, 415, "unsupported_svg_feature"},
		{media.ErrSVGSize, 413, "svg_too_large"},
		{media.ErrSVGComplexity, 422, "svg_too_complex"},
		{media.ErrBusy, 503, "converter_busy"},
	} {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Language", "en")
		(&Server{}).writeUploadError(w, tc.err)
		var body struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != tc.status || body.Error.Code != tc.code || body.Error.Message != translate("en", uploadErrorMessage(tc.err)) {
			t.Fatal(tc, w.Code, body)
		}
	}
}
