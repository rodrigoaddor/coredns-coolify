package coolify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func TestParse(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	good := `coolify https://coolify.example.com {
 token_env COOLIFY_TOKEN
 server first
 server second 192.0.2.10
 server third edge.example.com
 refresh 30s
 timeout 2s
 ttl 120
 }`
	c, err := parse(controller(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.servers) != 3 || c.baseURL != "https://coolify.example.com" || c.token != "secret" || c.ttl != 120 {
		t.Fatalf("unexpected configuration: %+v", c)
	}
	for _, body := range []string{
		"server one", "token_env COOLIFY_TOKEN", "token_env MISSING\nserver one",
		"token_env COOLIFY_TOKEN\nserver one\nserver one", "token_env COOLIFY_TOKEN\nserver one bad/name",
		"token_env COOLIFY_TOKEN\nserver one\nrefresh 0s", "token_env COOLIFY_TOKEN\nserver one\ntimeout -1s",
		"token_env COOLIFY_TOKEN\nserver one\nttl -1", "token_env COOLIFY_TOKEN\nserver one\nunknown foo",
		"token_env COOLIFY_TOKEN\ntoken_env COOLIFY_TOKEN\nserver one", "token_env COOLIFY_TOKEN\nserver ../one",
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := parse(controller("coolify https://coolify.example.com {\n" + body + "\n}")); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestRefreshAndDNS(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	var mu sync.Mutex
	responses := map[string]string{
		"one":   `[{"ip":"192.0.2.1","domains":["app.example.com","https://Docs.example.com:443/path"]},{"ip":"2001:db8::1","domains":["v6.example.com"]}]`,
		"two":   `[{"ip":"127.0.0.1","domains":["override.example.com","app.example.com"]}]`,
		"three": `[{"ip":"127.0.0.1","domains":["alias.example.com"]}]`,
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer authentication")
		}
		if r.Method != "GET" {
			t.Error("expected GET")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/servers/"), "/domains")
		mu.Lock()
		defer mu.Unlock()
		if body, ok := responses[id]; ok {
			fmt.Fprint(w, body)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer api.Close()
	c, err := parse(controller(fmt.Sprintf(`coolify %s {
 token_env COOLIFY_TOKEN
 server one
 server two 198.51.100.2
 server three edge.example.net
 }`, api.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	query := func(name string, typ uint16) *dns.Msg {
		t.Helper()
		r := new(dns.Msg)
		r.SetQuestion(name, typ)
		w := dnstest.NewRecorder(&test.ResponseWriter{})
		code, err := c.ServeDNS(context.Background(), w, r)
		if err != nil || code != dns.RcodeSuccess || w.Msg == nil || !w.Msg.Authoritative {
			t.Fatalf("query %s: code=%d err=%v response=%v", name, code, err, w.Msg)
		}
		return w.Msg
	}
	for _, tc := range []struct {
		name string
		typ  uint16
		want string
	}{
		{"app.example.com.", dns.TypeA, "192.0.2.1"},
		{"DOCS.example.com.", dns.TypeA, "192.0.2.1"},
		{"v6.example.com.", dns.TypeAAAA, "2001:db8::1"},
		{"override.example.com.", dns.TypeA, "198.51.100.2"},
		{"alias.example.com.", dns.TypeA, "edge.example.net."},
		{"alias.example.com.", dns.TypeCNAME, "edge.example.net."},
	} {
		msg := query(tc.name, tc.typ)
		if len(msg.Answer) != 1 || !strings.HasSuffix(msg.Answer[0].String(), tc.want) {
			t.Fatalf("unexpected answer %v", msg.Answer)
		}
	}
	if msg := query("app.example.com.", dns.TypeAAAA); len(msg.Answer) != 0 {
		t.Fatal("expected NODATA")
	}
	// One failed server retains data while a successful empty response removes records.
	mu.Lock()
	delete(responses, "one")
	responses["two"] = "[]"
	mu.Unlock()
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	query("app.example.com.", dns.TypeA)
	c.mu.RLock()
	_, exists := c.snapshot["override.example.com."]
	c.mu.RUnlock()
	if exists {
		t.Fatal("deleted record retained")
	}
	// Unknown names are delegated.
	called := false
	c.Next = plugin.HandlerFunc(func(_ context.Context, _ dns.ResponseWriter, _ *dns.Msg) (int, error) {
		called = true
		return dns.RcodeSuccess, nil
	})
	r := new(dns.Msg)
	r.SetQuestion("unknown.example.com.", dns.TypeA)
	c.ServeDNS(context.Background(), &test.ResponseWriter{}, r)
	if !called {
		t.Fatal("unknown name did not fall through")
	}
	// Query and refresh can safely run concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				query("app.example.com.", dns.TypeA)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		_ = c.refresh(context.Background())
	}
	wg.Wait()
}

func TestInvalidAPIDataRetainsSnapshot(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	body := `[{"ip":"192.0.2.1","domains":["app.example.com"]}]`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer api.Close()
	c, err := parse(controller(fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n server one\n}", api.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"broken", "null", `[{"ip":"bad/name","domains":["app.example.com"]}]`, `[{"ip":"app.example.com","domains":["app.example.com"]}]`, `[{"ip":"192.0.2.1","domains":["app.example.com"]},{"ip":"edge.example.com","domains":["app.example.com"]}]`} {
		body = invalid
		if err := c.refresh(context.Background()); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
		if len(c.snapshot["app.example.com."]) != 1 {
			t.Fatal("lost previous snapshot")
		}
	}
}

func controller(input string) *caddy.Controller {
	c := caddy.NewTestController("dns", input)
	c.ServerBlockKeys = []string{"."}
	return c
}

func TestTargetsAndNames(t *testing.T) {
	for _, input := range []string{"192.0.2.1", "2001:db8::1", "::ffff:192.0.2.1", "Edge.Example.com."} {
		if _, err := parseTarget(input); err != nil {
			t.Fatalf("valid target %s: %v", input, err)
		}
	}
	for _, input := range []string{"", "https://edge.example.com", "edge.example.com:443", "fe80::1%eth0", "*.example.com", "bad..example.com"} {
		if _, err := parseTarget(input); err == nil {
			t.Fatalf("accepted invalid target %q", input)
		}
	}
}

func TestRedirectRefused(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; fmt.Fprint(w, "[]") }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer origin.Close()
	c, err := parse(controller(fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n server one\n}", origin.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("accepted redirect")
	}
	if reached {
		t.Fatal("followed redirect with credentials")
	}
}
