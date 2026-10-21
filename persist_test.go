package coolify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func TestPersistRestartAndRefresh(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	list := `[{"uuid":"two"},{"uuid":"one"}]`
	failList, failDomains := false, false
	address := "192.0.2.1"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/servers" {
			if failList {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, list)
			return
		}
		if failDomains {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/api/v1/servers/one/domains":
			fmt.Fprintf(w, `[{"ip":%q,"domains":["app.example.com"]},{"ip":"2001:db8::1","domains":["v6.example.com"]}]`, address)
		case "/api/v1/servers/two/domains":
			fmt.Fprint(w, `[{"ip":"192.0.2.2","domains":["alias.example.com","app.example.com"]}]`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	config := fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n servers auto\n server two edge.example.net\n persist %s\n}", api.URL, path)
	restart := func() *Coolify {
		t.Helper()
		c, err := parse(controller(config))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.client.CloseIdleConnections)
		return c
	}
	c := restart()
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("cache exposes token")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("cache permissions: %v", info.Mode())
	}
	restored := restart()
	if !reflect.DeepEqual(restored.snapshot, c.snapshot) || !reflect.DeepEqual(restored.discovered, c.discovered) {
		t.Fatal("restart did not restore records and sorted server list")
	}
	// Query before any API refresh, including explicit precedence and IPv6/CNAME.
	for _, tc := range []struct {
		name string
		typ  uint16
		want string
	}{
		{"app.example.com.", dns.TypeCNAME, "edge.example.net."},
		{"alias.example.com.", dns.TypeCNAME, "edge.example.net."},
		{"v6.example.com.", dns.TypeAAAA, "2001:db8::1"},
	} {
		req := new(dns.Msg)
		req.SetQuestion(tc.name, tc.typ)
		w := dnstest.NewRecorder(&test.ResponseWriter{})
		code, err := restored.ServeDNS(context.Background(), w, req)
		if err != nil || code != 0 || w.Msg == nil || len(w.Msg.Answer) != 1 || !strings.HasSuffix(w.Msg.Answer[0].String(), tc.want) {
			t.Fatalf("restored DNS %s: %v %v", tc.name, w.Msg, err)
		}
	}
	failList, failDomains = true, true
	if err := restored.refresh(context.Background()); err == nil {
		t.Fatal("expected outage")
	}
	if !reflect.DeepEqual(restored.snapshot, c.snapshot) {
		t.Fatal("outage lost restored records")
	}
	// A failed list request still uses cached membership to update domains.
	failDomains = false
	address = "192.0.2.10"
	if err := restored.refresh(context.Background()); err == nil {
		t.Fatal("expected list failure")
	}
	if restored.byServer["one"]["app.example.com."][0].ip.String() != address {
		t.Fatal("cached membership not refreshed")
	}
	if restart().byServer["one"]["app.example.com."][0].ip.String() != address {
		t.Fatal("partial refresh not persisted")
	}
	// Successful discovery removes departed servers, while explicit servers stay.
	failList = false
	list = "[]"
	if err := restored.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := restart()
	if len(after.discovered) != 0 || len(after.byServer) != 1 || len(after.snapshot["v6.example.com."]) != 0 || len(after.snapshot["alias.example.com."]) != 1 {
		t.Fatal("server removal not persisted")
	}
}

func TestPersistValidation(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	path := filepath.Join(t.TempDir(), "cache.json")
	config := fmt.Sprintf("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n servers auto\n server one\n publish dashboard.example.net\n persist %s\n}", path)
	parseConfig := func(config string) *Coolify {
		t.Helper()
		c, err := parse(controller(config))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.client.CloseIdleConnections)
		return c
	}
	c := parseConfig(config)
	c.byServer["one"] = records{"app.example.com.": {{alias: "edge.example.net."}}}
	if err := c.saveState(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Invalid caches are ignored as a unit, retaining the current dashboard record.
	for _, tc := range []struct {
		name   string
		mutate func(*persistedState)
	}{
		{"version", func(s *persistedState) { s.Version++ }},
		{"config", func(s *persistedState) { s.Config = "wrong" }},
		{"server", func(s *persistedState) { s.Discovered = []string{"../bad"} }},
		{"duplicate server", func(s *persistedState) { s.Discovered = []string{"one", "one"} }},
		{"inactive server", func(s *persistedState) { s.ByServer["other"] = map[string][]string{} }},
		{"domain", func(s *persistedState) { s.ByServer["one"]["bad/name"] = []string{"192.0.2.1"} }},
		{"target", func(s *persistedState) { s.ByServer["one"]["app.example.com."] = []string{"bad/name"} }},
		{"self alias", func(s *persistedState) { s.ByServer["one"]["app.example.com."] = []string{"app.example.com."} }},
		{"conflict", func(s *persistedState) {
			s.ByServer["one"]["app.example.com."] = []string{"edge.example.net.", "192.0.2.1"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state persistedState
			if err := json.Unmarshal(good, &state); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&state)
			data, _ := json.Marshal(state)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got := parseConfig(config)
			if len(got.snapshot) != 1 || got.snapshot["cloud.example.com."][0].alias != "dashboard.example.net." || len(got.byServer) != 0 || len(got.discovered) != 0 {
				t.Fatal("invalid cache was partially applied")
			}
		})
	}
	for _, data := range [][]byte{[]byte("broken"), make([]byte, maxStateSize+1)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if len(parseConfig(config).snapshot) != 1 {
			t.Fatal("invalid file applied")
		}
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{
		strings.Replace(config, "server one", "server one 192.0.2.9", 1),
		strings.Replace(config, "cloud.example.com", "other.example.com", 1),
		strings.Replace(config, "servers auto\n", "", 1),
	} {
		if len(parseConfig(changed).byServer) != 0 {
			t.Fatal("restored cache with incompatible configuration")
		}
	}
	t.Setenv("COOLIFY_TOKEN", "rotated")
	if len(parseConfig(config).byServer) != 0 {
		t.Fatal("restored cache for another token")
	}
}

func TestPersistEmptyAndWriteFailure(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	body := `[{"ip":"192.0.2.1","domains":["app.example.com"]}]`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "cache.json")
	config := fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n server one\n persist %s\n}", api.URL, path)
	c, err := parse(controller(config))
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.CloseIdleConnections()
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A failed replacement preserves the previous complete cache.
	previous, _ := os.ReadFile(path)
	original := c.persistPath
	c.persistPath = filepath.Join(path, "missing.json")
	body = "[]"
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("expected write failure")
	}
	data, _ := os.ReadFile(path)
	if string(previous) != string(data) || len(c.snapshot) != 0 {
		t.Fatal("disk failure damaged prior file or blocked fresh DNS state")
	}
	c.persistPath = original
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := parse(controller(config))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.client.CloseIdleConnections()
	if len(restored.snapshot) != 0 || len(restored.byServer["one"]) != 0 {
		t.Fatal("empty response deletion not persisted")
	}
	for _, directive := range []string{"persist", "persist one two", "persist one\n persist two"} {
		if _, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n server one\n" + directive + "\n}")); err == nil {
			t.Fatalf("accepted %q", directive)
		}
	}
}
