package coolify

import (
	"bytes"
	"context"
	"fmt"
	golog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func TestPublishAvailableWithoutAPI(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	for _, tc := range []struct {
		target string
		typ    uint16
		want   string
	}{
		{"192.0.2.20", dns.TypeA, "192.0.2.20"},
		{"2001:db8::20", dns.TypeAAAA, "2001:db8::20"},
		{"Edge.example.net", dns.TypeCNAME, "edge.example.net."},
	} {
		t.Run(tc.target, func(t *testing.T) {
			c, err := parse(controller("coolify https://CLOUD.example.com:8443 {\n token_env COOLIFY_TOKEN\n connect_host 192.0.2.10\n publish " + tc.target + "\n server one\n}"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.client.CloseIdleConnections()
			c.client.Transport = unavailableAPI{}
			query := func() {
				t.Helper()
				r := new(dns.Msg)
				r.SetQuestion("cloud.example.com.", tc.typ)
				w := dnstest.NewRecorder(&test.ResponseWriter{})
				code, err := c.ServeDNS(context.Background(), w, r)
				if err != nil || code != dns.RcodeSuccess || w.Msg == nil || len(w.Msg.Answer) != 1 || !strings.HasSuffix(w.Msg.Answer[0].String(), tc.want) {
					t.Fatalf("unexpected reply %v: %v", w.Msg, err)
				}
			}
			query() // The dashboard is available before any refresh.
			if err := c.refresh(context.Background()); err == nil {
				t.Fatal("expected API failure")
			}
			query()
		})
	}
}

func TestPublishPrecedenceAndPluginChain(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"ip":"198.51.100.1","domains":["cloud.example.com","app.example.com"]}]`)
	}))
	defer api.Close()
	c, err := parse(controller("coolify http://cloud.example.com {\n token_env COOLIFY_TOKEN\n publish dashboard.t.example.net\n server one edge.example.net\n}"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.CloseIdleConnections()
	// Use the mock transport while configuring the real dashboard hostname.
	c.client.Transport = apiTransport{url: api.URL, client: api.Client()}
	var warnings bytes.Buffer
	originalOutput := golog.Writer()
	golog.SetOutput(&warnings)
	defer golog.SetOutput(originalOutput)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "[WARNING]") || !strings.Contains(warnings.String(), "cloud.example.com.") {
		t.Fatalf("missing dashboard collision warning: %s", warnings.String())
	}
	if ts := c.snapshot[c.dashboardName]; len(ts) != 1 || ts[0].alias != "dashboard.t.example.net." {
		t.Fatalf("dashboard lost precedence: %v", ts)
	}
	c.client.Transport = unavailableAPI{}
	if err := c.refresh(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	for _, tc := range []struct{ name, target string }{
		{"cloud.example.com.", "dashboard.t.example.net."},
		{"app.example.com.", "edge.example.net."},
	} {
		calls := 0
		c.Next = plugin.HandlerFunc(func(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
			calls++
			if r.Question[0].Name != tc.target {
				t.Fatalf("unexpected target %s", r.Question[0].Name)
			}
			msg := new(dns.Msg)
			msg.SetReply(r)
			rr, _ := dns.NewRR(tc.target + " 30 IN A 198.51.100.10")
			msg.Answer = []dns.RR{rr}
			return dns.RcodeSuccess, w.WriteMsg(msg)
		})
		r := new(dns.Msg)
		r.SetQuestion(tc.name, dns.TypeA)
		w := dnstest.NewRecorder(&test.ResponseWriter{})
		code, err := c.ServeDNS(context.Background(), w, r)
		if err != nil || code != dns.RcodeSuccess || calls != 1 || w.Msg == nil || len(w.Msg.Answer) != 2 {
			t.Fatalf("unexpected reply %v: %v", w.Msg, err)
		}
		if alias, ok := w.Msg.Answer[0].(*dns.CNAME); !ok || alias.Target != tc.target {
			t.Fatalf("CNAME not preserved: %v", w.Msg.Answer)
		}
	}
}

// apiTransport connects a hostname-based origin to the mock API.
type apiTransport struct {
	url    string
	client *http.Client
}

func (a apiTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	replacement, err := http.NewRequestWithContext(r.Context(), r.Method, a.url+r.URL.RequestURI(), nil)
	if err != nil {
		return nil, err
	}
	req.URL = replacement.URL
	return a.client.Do(req)
}

func TestPublishConfiguration(t *testing.T) {
	t.Setenv("COOLIFY_TOKEN", "secret")
	for _, value := range []string{"", "https://edge.example.com", "edge.example.com:443", "bad/name", "*.example.com", "fe80::1%eth0", "CLOUD.example.com.", "edge.example.com extra", "edge.example.com\n publish second.example.com"} {
		if _, err := parse(controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n server one\n publish " + value + "\n}")); err == nil {
			t.Fatalf("accepted invalid publish %q", value)
		}
	}
	input := "coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n publish 192.0.2.1\n}"
	for _, zone := range []string{"example.com.", "cloud.example.com.", "other.example.com.", "com."} {
		p := controller(input)
		p.ServerBlockKeys = []string{zone}
		c, err := parse(p)
		if zone == "other.example.com." {
			if err == nil {
				t.Fatal("accepted dashboard outside zone")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		c.client.CloseIdleConnections()
	}
	for _, origin := range []string{"http://127.0.0.1", "http://[::1]", "http://bad_name.example.com"} {
		if _, err := parse(controller(strings.Replace(input, "https://cloud.example.com", origin, 1))); err == nil {
			t.Fatalf("accepted non-dashboard hostname %s", origin)
		}
	}
	// Without publish, the URL can be outside the zone and produces no dashboard record.
	p := controller("coolify https://cloud.example.com {\n token_env COOLIFY_TOKEN\n connect_host 192.0.2.1\n server one\n}")
	p.ServerBlockKeys = []string{"apps.example.com."}
	c, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.CloseIdleConnections()
	if len(c.snapshot) != 0 {
		t.Fatal("published dashboard without publish")
	}
}

// unavailableAPI makes outage coverage independent of external networking.
type unavailableAPI struct{}

func (unavailableAPI) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("API unavailable")
}
