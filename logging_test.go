package coolify

import (
	"bytes"
	"context"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoveryLogging(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	var output bytes.Buffer
	oldOutput := stdlog.Writer()
	stdlog.SetOutput(&output)
	t.Cleanup(func() { stdlog.SetOutput(oldOutput) })
	list := `[{"uuid":"one"}]`
	body := `[{"ip":"192.0.2.1","domains":["app.example.com"]},{"ip":"2001:db8::1","domains":["v6.example.com"]},{"ip":"edge.example.net","domains":["alias.example.com"]}]`
	fail := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/api/v1/servers" {
			fmt.Fprint(w, list)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "cache.json")
	config := fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n servers auto\n persist %s\n log_discovery\n}", api.URL, path)
	restart := func(config string) *Coolify {
		t.Helper()
		c, err := parse(controller(config))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.client.CloseIdleConnections)
		return c
	}
	refresh := func(c *Coolify, wantError bool) string {
		t.Helper()
		output.Reset()
		err := c.refresh(context.Background())
		if (err != nil) != wantError {
			t.Fatalf("refresh error: %v", err)
		}
		return output.String()
	}
	c := restart(config)
	got := refresh(c, false)
	for _, want := range []string{`discovered server "one"`, "discovered record app.example.com. A 192.0.2.1", "discovered record v6.example.com. AAAA 2001:db8::1", "discovered record alias.example.com. CNAME edge.example.net."} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("expected %q once in %s", want, got)
		}
	}
	if got := refresh(c, false); got != "" {
		t.Fatalf("unchanged refresh logged: %s", got)
	}
	restored := restart(config)
	if got := refresh(restored, false); got != "" {
		t.Fatalf("cached discoveries logged again: %s", got)
	}
	fail = true
	if got := refresh(restored, true); got != "" {
		t.Fatalf("outage logged discoveries: %s", got)
	}
	fail = false
	// New targets for an existing name are discoveries as well.
	body = `[{"ip":"192.0.2.10","domains":["app.example.com"]}]`
	got = refresh(restored, false)
	if strings.Count(got, "discovered record") != 1 || !strings.Contains(got, "app.example.com. A 192.0.2.10") {
		t.Fatalf("target change: %s", got)
	}
	list = `[{"uuid":"one"},{"uuid":"two"}]`
	got = refresh(restored, false)
	if !strings.Contains(got, `discovered server "two"`) || strings.Contains(got, "discovered record") {
		t.Fatalf("duplicate records on another server: %s", got)
	}
	// Removal is quiet; rediscovery is logged.
	list, body = "[]", "[]"
	if got := refresh(restored, false); got != "" {
		t.Fatalf("removal logged: %s", got)
	}
	list = `[{"uuid":"one"}]`
	body = `[{"ip":"192.0.2.1","domains":["app.example.com"]}]`
	got = refresh(restored, false)
	if !strings.Contains(got, `discovered server "one"`) || !strings.Contains(got, "app.example.com. A 192.0.2.1") {
		t.Fatalf("rediscovery: %s", got)
	}
	disabled := restart(strings.ReplaceAll(strings.ReplaceAll(config, " log_discovery\n", ""), " persist "+path+"\n", ""))
	if got := refresh(disabled, false); got != "" {
		t.Fatalf("logging enabled by default: %s", got)
	}
}

func TestDiscoveryLoggingConfiguration(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	for _, body := range []string{"log_discovery true", "log_discovery extra args", "log_discovery\n log_discovery"} {
		if _, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n server one\n " + body + "\n}")); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
