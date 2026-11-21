package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

type logLines chan []byte

func (l logLines) Write(p []byte) (int, error) {
	l <- append([]byte(nil), p...)
	return len(p), nil
}

func TestAuthenticatedRateLimitAccessLogRetainsIdentity(t *testing.T) {
	lines := make(logLines, 256)
	env := newTestEnvWithLogger(t, nil, slog.NewJSONHandler(lines, nil))
	env.register(t, env.client, "logged@test.com", "password123", "")
	user, err := env.store.UserByEmail(context.Background(), "logged@test.com")
	if err != nil {
		t.Fatal(err)
	}
	for range 31 {
		resp, err := env.client.Get(env.ts.URL + "/api/v1/tokens")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	env.ts.Close()
	close(lines)
	found := false
	for line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry["path"] == "/api/v1/tokens" && entry["status"] == float64(http.StatusTooManyRequests) {
			found = true
			if entry["user_id"] != user.ID || entry["request_id"] == "" {
				t.Fatal(entry)
			}
		}
	}
	if !found {
		t.Fatal("authenticated 429 not logged")
	}
}
