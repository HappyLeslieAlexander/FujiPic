// Package media talks to the credential-free, network-isolated decoder over a
// Unix socket. Requests contain bytes, never URLs or host filesystem paths.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"fujipic/internal/admission"
	"fujipic/internal/imagefile"
)

var ErrUnavailable = errors.New("media converter unavailable")
var ErrBusy = fmt.Errorf("%w: busy", ErrUnavailable)

// These wrap existing permanent errors so preview jobs keep their retry policy.
var (
	ErrFeature       = fmt.Errorf("%w: unsupported format feature", imagefile.ErrUnsupported)
	ErrSVGFeature    = fmt.Errorf("%w: unsupported SVG feature", ErrFeature)
	ErrSVGSize       = fmt.Errorf("%w: SVG exceeds 4 MiB", imagefile.ErrUnsupported)
	ErrSVGComplexity = fmt.Errorf("%w: SVG structure exceeds limit", imagefile.ErrUnsupported)
)

const MaxPreviewBytes int64 = 16 << 20

type Client struct {
	http *http.Client
	gate *admission.Gate
}

func New(socket string) *Client {
	return &Client{gate: admission.New(8), http: &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
		MaxConnsPerHost: 2, ResponseHeaderTimeout: 95 * time.Second,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) acquire(ctx context.Context) error {
	// Inspection also executes untrusted native parsing. Preserve single-decoder
	// resource limits, but queue both classes FIFO instead of rejecting uploads
	// whenever a preview runs. Waiting ends on cancellation or a bounded deadline.
	wait, cancel := context.WithTimeout(ctx, 85*time.Second)
	err := c.gate.Acquire(wait)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrBusy
	}
	return nil
}

func (c *Client) request(ctx context.Context, op, path, format string, pixels int64, dim int) (*http.Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://converter/"+op, f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Format", format)
	req.Header.Set("X-Max-Pixels", strconv.FormatInt(pixels, 10))
	req.Header.Set("X-Max-Dimension", strconv.Itoa(dim))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		if resp.StatusCode == 503 && resp.Header.Get("X-Media-Busy") == "1" {
			return nil, ErrBusy
		}
		reason := resp.Header.Get("X-Media-Error")
		switch {
		case resp.StatusCode == 415 && reason == "unsupported_feature":
			if format == "svg" {
				return nil, ErrSVGFeature
			}
			return nil, ErrFeature
		case format == "svg" && resp.StatusCode == 413 && reason == "svg_size":
			return nil, ErrSVGSize
		case format == "svg" && resp.StatusCode == 422 && reason == "svg_complexity":
			return nil, ErrSVGComplexity
		}
		switch resp.StatusCode {
		case 415:
			return nil, imagefile.ErrUnsupported
		case 422:
			return nil, imagefile.ErrDimensions
		default:
			return nil, ErrUnavailable
		}
	}
	return resp, nil
}

func (c *Client) Inspect(ctx context.Context, path, format string, pixels int64, dim int) (imagefile.Metadata, error) {
	var meta imagefile.Metadata
	if err := c.acquire(ctx); err != nil {
		return meta, err
	}
	defer c.gate.Release()
	r, err := c.request(ctx, "inspect", path, format, pixels, dim)
	if err != nil {
		return meta, err
	}
	defer r.Body.Close()
	err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&meta)
	return meta, err
}

func (c *Client) Convert(ctx context.Context, path, format, tmp string, pixels int64, dim int) (*imagefile.Info, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.gate.Release()
	r, err := c.request(ctx, "convert", path, format, pixels, dim)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	// Independently validate the output; a decoder cannot choose stored paths,
	// MIME types or metadata. Only a bounded PNG is published.
	info, err := imagefile.Process(r.Body, tmp, MaxPreviewBytes, 4096*4096, 4096)
	if err == nil && info.MIME != "image/png" {
		info.Cleanup()
		return nil, imagefile.ErrUnsupported
	}
	return info, err
}
