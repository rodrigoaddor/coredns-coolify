package coolify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAutoDiscovery(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	list := `[{"uuid":"two"},{"uuid":"one"}]`
	failList, failOne := false, false
	calls := map[string]int{}
	oneIP := "192.0.2.1"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing token")
		}
		calls[r.URL.Path]++
		if r.URL.Path == "/api/v1/servers" {
			if failList {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, list)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/servers/"), "/domains")
		if id == "one" && failOne {
			w.WriteHeader(503)
			return
		}
		ip := "192.0.2.2"
		if id == "one" {
			ip = oneIP
		}
		fmt.Fprintf(w, `[{"ip":%q,"domains":[%q,"shared.example.com"]}]`, ip, id+".example.com")
	}))
	defer api.Close()
	c, err := parse(controller(fmt.Sprintf("coolify %s {\n token_env COOLIFY_TOKEN\n servers auto\n server two edge.example.net\n server explicit 2001:db8::1\n}", api.URL)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.CloseIdleConnections()
	refresh := func(wantError bool) {
		t.Helper()
		err := c.refresh(context.Background())
		if (err != nil) != wantError {
			t.Fatalf("refresh error = %v", err)
		}
	}
	refresh(false)
	if c.snapshot["one.example.com."][0].ip.String() != "192.0.2.1" || c.snapshot["two.example.com."][0].alias != "edge.example.net." || c.snapshot["explicit.example.com."][0].ip.String() != "2001:db8::1" {
		t.Fatalf("unexpected snapshot: %v", c.snapshot)
	}
	if calls["/api/v1/servers/two/domains"] != 1 {
		t.Fatal("explicit server fetched twice")
	}
	if c.snapshot["shared.example.com."][0].alias != "edge.example.net." {
		t.Fatal("explicit configuration did not take precedence")
	}
	// Listing failure still refreshes previously discovered servers.
	failList = true
	oneIP = "192.0.2.10"
	refresh(true)
	if c.snapshot["one.example.com."][0].ip.String() != oneIP {
		t.Fatal("cached server list was not refreshed")
	}
	failOne = true
	refresh(true)
	if c.snapshot["one.example.com."][0].ip.String() != oneIP {
		t.Fatal("failed discovered server lost records")
	}
	// Invalid lists retain the previous membership as well.
	failList = false
	for _, invalid := range []string{"null", "{}", `[{"uuid":""}]`, `[{"uuid":"../bad"}]`, `[{"uuid":"one"},{"uuid":"one"}]`} {
		list = invalid
		refresh(true)
		if len(c.snapshot["one.example.com."]) != 1 {
			t.Fatal("invalid list discarded records")
		}
	}
	// A successful list removes departed auto servers, retaining explicit ones.
	list = `[{"uuid":"three"}]`
	refresh(false)
	if _, ok := c.snapshot["one.example.com."]; ok {
		t.Fatal("removed server retained records")
	}
	if len(c.snapshot["three.example.com."]) != 1 || len(c.snapshot["two.example.com."]) != 1 {
		t.Fatal("membership change or explicit server lost")
	}
	list = "[]"
	refresh(false)
	if _, ok := c.snapshot["three.example.com."]; ok {
		t.Fatal("empty list retained auto server")
	}
}

func TestAutoDiscoveryInitialFailure(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	c, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n servers auto\n publish edge.example.net\n}"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.CloseIdleConnections()
	c.client.Transport = unavailableAPI{}
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("expected discovery failure")
	}
	if len(c.snapshot) != 1 || c.snapshot["cloud.example.com."][0].alias != "edge.example.net." {
		t.Fatal("discovery outage lost dashboard")
	}
}

func TestAutoDiscoveryConfiguration(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	for _, body := range []string{"servers", "servers all", "servers auto extra", "servers auto\n servers auto", "coolify_domain cloud.example.com", "token secret"} {
		if _, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n server one\n " + body + "\n}")); err == nil {
			t.Fatalf("accepted invalid configuration: %s", body)
		}
	}
}
