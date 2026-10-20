package coolify

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestDebugRequestErrors(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret-token")
	for _, value := range []string{"", "false", "0", "true", "1"} {
		t.Run("debug="+value, func(t *testing.T) {
			t.Setenv("COOLIFY_DEBUG", value)
			c, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n servers auto\n}"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.client.CloseIdleConnections()
			debug := value == "true" || value == "1"
			for _, stage := range []string{"request", "response body"} {
				t.Run(stage, func(t *testing.T) {
					cause := &net.DNSError{Err: "no such host", Name: "cloud.example.com", IsNotFound: true}
					c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.Header.Get("Authorization") != "Bearer secret-token" {
							t.Error("missing authentication")
						}
						if stage == "request" {
							return nil, cause
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: failingBody{cause}, Request: r}, nil
					})
					err := c.refresh(context.Background())
					if err == nil {
						t.Fatal("expected refresh failure")
					}
					message := err.Error()
					if strings.Contains(message, cause.Error()) != debug {
						t.Fatalf("debug=%t: %s", debug, message)
					}
					if !strings.Contains(message, "server discovery:") || strings.Contains(message, "secret-token") {
						t.Fatalf("unexpected error: %s", message)
					}
					if !debug {
						want := "server discovery: API request failed"
						if stage == "response body" {
							want = "server discovery: reading API response failed"
						}
						if message != want {
							t.Fatalf("got %q, want %q", message, want)
						}
					}
				})
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }
