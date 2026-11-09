// Package cdn implements cache purge adapters for the configured CDN.
package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Purger interface {
	// Purge invalidates one image URL. Returning an error schedules a retry.
	Purge(ctx context.Context, url string) error
	// Enabled reports whether purging is configured. When false, deletions
	// stay in the cdn_purges queue for manual handling.
	Enabled() bool
	Name() string
}

// BatchPurger is implemented by adapters whose provider can invalidate
// multiple files in one API call.
type BatchPurger interface {
	PurgeBatch(ctx context.Context, urls []string) error
}

type None struct{}

func (None) Purge(context.Context, string) error { return fmt.Errorf("cdn purge is not configured") }
func (None) Enabled() bool                       { return false }
func (None) Name() string                        { return "none" }

type Cloudflare struct {
	ZoneID   string
	Token    string
	Endpoint string
	Client   *http.Client
}

func (c *Cloudflare) Enabled() bool { return c.ZoneID != "" && c.Token != "" }
func (c *Cloudflare) Name() string  { return "cloudflare" }

func (c *Cloudflare) Purge(ctx context.Context, url string) error {
	return c.PurgeBatch(ctx, []string{url})
}

func (c *Cloudflare) PurgeBatch(ctx context.Context, urls []string) error {
	body, _ := json.Marshal(map[string]any{"files": urls})
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", c.ZoneID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloudflare purge: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	var parsed struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil && !parsed.Success {
		return fmt.Errorf("cloudflare purge rejected: %s", truncate(respBody))
	}
	return nil
}

type Generic struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

func (g *Generic) Enabled() bool { return g.Endpoint != "" && g.Token != "" }
func (g *Generic) Name() string  { return "generic" }

func (g *Generic) Purge(ctx context.Context, url string) error {
	return g.PurgeBatch(ctx, []string{url})
}

func (g *Generic) PurgeBatch(ctx context.Context, urls []string) error {
	body, _ := json.Marshal(map[string]any{"urls": urls})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cdn purge: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}
