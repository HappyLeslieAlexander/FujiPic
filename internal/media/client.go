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
	"sync"
	"time"

	"fujipic/internal/imagefile"
)

var ErrUnavailable = errors.New("media converter unavailable")
var ErrBusy = fmt.Errorf("%w: busy", ErrUnavailable)

const MaxPreviewBytes int64 = 16 << 20

type Client struct {
	http        *http.Client
	mu          sync.Mutex
	conversions int
	slot        chan struct{}
}

func New(socket string) *Client {
	return &Client{slot: make(chan struct{}, 1), http: &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
		MaxConnsPerHost: 2, ResponseHeaderTimeout: 95 * time.Second,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) request(ctx context.Context, op, path, format string, pixels int64, dim int) (*http.Response, error) {
	// Give durable previews priority over new synchronous inspections. Waiting
	// conversions close inspection admission before the current request finishes.
	c.mu.Lock()
	if op == "inspect" {
		if c.conversions > 0 {
			c.mu.Unlock()
			return nil, ErrBusy
		}
		select {
		case c.slot <- struct{}{}:
		default:
			c.mu.Unlock()
			return nil, ErrBusy
		}
		c.mu.Unlock()
	} else {
		c.conversions++
		c.mu.Unlock()
		defer func() { c.mu.Lock(); c.conversions--; c.mu.Unlock() }()
		select {
		case c.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer func() { <-c.slot }()
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
	r, err := c.request(ctx, "inspect", path, format, pixels, dim)
	if err != nil {
		return meta, err
	}
	defer r.Body.Close()
	err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&meta)
	return meta, err
}

func (c *Client) Convert(ctx context.Context, path, format, tmp string, pixels int64, dim int) (*imagefile.Info, error) {
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
