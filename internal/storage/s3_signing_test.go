package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type signingTransport func(*http.Request) (*http.Response, error)

func (f signingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Verify the wire signature independently of the SDK signer. Existing fakeS3
// tests only require a nonempty Authorization header and cannot catch incorrect
// regions, credential mixing between tenants, or a mismatched secret.
func verifyS3Signature(r *http.Request, access, secret, region string) error {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")
	fields := make(map[string]string)
	for _, field := range strings.Split(auth, ", ") {
		key, value, _ := strings.Cut(field, "=")
		fields[key] = value
	}
	credential := strings.Split(fields["Credential"], "/")
	if len(credential) != 5 || credential[0] != access || credential[2] != region ||
		credential[3] != "s3" || credential[4] != "aws4_request" {
		return fmt.Errorf("incorrect credential scope (expected this client's key and region)")
	}
	var headers strings.Builder
	for _, name := range strings.Split(fields["SignedHeaders"], ";") {
		values := r.Header.Values(name)
		if name == "host" {
			host := r.Host
			if host == "" {
				host = r.URL.Host
			}
			values = []string{host}
		}
		if name == "content-length" {
			values = []string{fmt.Sprint(r.ContentLength)}
		}
		for i, value := range values {
			values[i] = strings.Join(strings.Fields(value), " ")
		}
		fmt.Fprintf(&headers, "%s:%s\n", name, strings.Join(values, ","))
	}
	canonical := strings.Join([]string{
		r.Method, r.URL.EscapedPath(), r.URL.Query().Encode(), headers.String(),
		fields["SignedHeaders"], r.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	scope := strings.Join(credential[1:], "/")
	toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	mac := func(key []byte, value string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(value))
		return h.Sum(nil)
	}
	key := mac([]byte("AWS4"+secret), credential[1])
	key = mac(key, region)
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	want := hex.EncodeToString(mac(key, toSign))
	if !hmac.Equal([]byte(want), []byte(fields["Signature"])) {
		return fmt.Errorf("signature does not match this client's secret")
	}
	return nil
}

func TestS3SignatureAfterHTTPTransport(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("HTTP2=%t", h2), func(t *testing.T) {
			var seen atomic.Int32
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != h2 {
					t.Errorf("unexpected HTTP version %s", r.Proto)
				}
				if err := verifyS3Signature(r, "jnb-access", "jnb-secret+/=", "af-johannesburg-1"); err != nil {
					t.Error(err)
					w.WriteHeader(403)
					return
				}
				seen.Add(1)
				if r.Body != nil {
					_, _ = io.Copy(io.Discard, r.Body)
				}
				w.WriteHeader(200)
			}))
			ts.EnableHTTP2 = h2
			ts.StartTLS()
			defer ts.Close()
			client, err := NewS3(context.Background(), S3Options{
				Endpoint: ts.URL, Region: "af-johannesburg-1", Bucket: "private-bucket",
				AccessKeyID: "jnb-access", SecretAccessKey: "jnb-secret+/=", ForcePathStyle: true,
				HTTPClient: ts.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Put(context.Background(), ".fujipic-probe/test", strings.NewReader("fujipic"), 7, "text/plain"); err != nil {
				t.Fatal(err)
			}
			if err := client.Delete(context.Background(), ".fujipic-probe/test"); err != nil {
				t.Fatal(err)
			}
			if seen.Load() != 2 {
				t.Fatal("TLS server did not verify both signatures")
			}
		})
	}
}

func TestS3RegionalCredentialsAndWireSignatures(t *testing.T) {
	// The explicit bucket configuration must win over ambient/default values.
	t.Setenv("AWS_REGION", "us-sanjose-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "wrong-environment-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wrong-environment-secret")
	t.Setenv("AWS_SESSION_TOKEN", "wrong-environment-session")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ctx := context.Background()
	var clients []*S3
	var count atomic.Int32
	for i, region := range []string{"us-sanjose-1", "af-johannesburg-1"} {
		access := fmt.Sprintf("tenant-%d-access", i)
		secret := fmt.Sprintf("tenant-%d-secret+/=", i)
		host := fmt.Sprintf("namespace%d.compat.objectstorage.%s.oraclecloud.com", i, region)
		bucket := fmt.Sprintf("private-bucket-%d", i)
		transport := signingTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != host || r.URL.Path != "/"+bucket+"/.fujipic-probe/test" {
				return nil, fmt.Errorf("request used another bucket's endpoint or path")
			}
			if r.Header.Get("X-Amz-Security-Token") != "" {
				return nil, fmt.Errorf("ambient credentials leaked into static bucket client")
			}
			if err := verifyS3Signature(r, access, secret, region); err != nil {
				return nil, err
			}
			// Ensure the checker rejects the wrong secret, not just a nonempty header.
			if verifyS3Signature(r, access, "wrong-secret", region) == nil {
				return nil, fmt.Errorf("signature verifier accepted the wrong secret")
			}
			count.Add(1)
			if r.Body != nil {
				_, _ = io.Copy(io.Discard, r.Body)
			}
			body := ""
			if r.Method == http.MethodGet {
				body = "fujipic"
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)), Request: r,
			}, nil
		})
		client, err := NewS3(ctx, S3Options{
			Endpoint: "https://" + host, Region: region, Bucket: bucket,
			AccessKeyID: access, SecretAccessKey: secret, ForcePathStyle: true,
			HTTPClient: &http.Client{Transport: transport},
		})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	// Reuse the first client after constructing the second to catch global state.
	for _, client := range []*S3{clients[0], clients[1], clients[0]} {
		const key = ".fujipic-probe/test"
		if err := client.Put(ctx, key, strings.NewReader("fujipic"), 7, "text/plain"); err != nil {
			t.Fatal(err)
		}
		obj, err := client.Get(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		obj.Body.Close()
		if _, err := client.Head(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := client.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 12 {
		t.Fatalf("verified %d signed requests, want 12", count.Load())
	}
}
