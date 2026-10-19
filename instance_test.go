package coolify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func TestCoolifyDomainPinnedIP(t *testing.T) {
	for _, ip := range []string{"192.0.2.20", "2001:db8::20"} {
		t.Run(ip, func(t *testing.T) {
			c, err := parse(controller("coolify https://cloud.example.com {\n token secret\n connect_host " + ip + "\n coolify_domain CLOUD.example.com\n server one 198.51.100.2\n}"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.client.CloseIdleConnections()
			// Even an unreachable API must not prevent the fixed instance mapping.
			c.client.Timeout = time.Millisecond
			c.client.Transport = unavailableAPI{}
			c.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
				t.Error("pinned IP should not require DNS")
				return nil, fmt.Errorf("unexpected lookup")
			}
			if err := c.refresh(context.Background()); err == nil {
				t.Fatal("expected unavailable server API")
			}
			ts := c.snapshot["cloud.example.com."]
			if len(ts) != 1 || ts[0].ip.String() != ip {
				t.Fatalf("unexpected instance targets: %v", ts)
			}
			typ := uint16(dns.TypeA)
			if ts[0].ip.Is6() {
				typ = dns.TypeAAAA
			}
			r := new(dns.Msg)
			r.SetQuestion("cloud.example.com.", typ)
			w := dnstest.NewRecorder(&test.ResponseWriter{})
			code, err := c.ServeDNS(context.Background(), w, r)
			if err != nil || code != dns.RcodeSuccess || len(w.Msg.Answer) != 1 || !strings.HasSuffix(w.Msg.Answer[0].String(), ip) {
				t.Fatalf("unexpected DNS reply: %v %v", w.Msg, err)
			}
		})
	}
}

func TestCoolifyDomainRefresh(t *testing.T) {
	for _, connectHost := range []string{"", "host.docker.internal"} {
		t.Run("connect_host="+connectHost, func(t *testing.T) {
			body := `[{"ip":"198.51.100.1","domains":["cloud.example.com","app.example.com"]}]`
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer api.Close()
			c, err := parse(controller(fmt.Sprintf("coolify %s {\n token secret\n coolify_domain cloud.example.com\n server one\n}", api.URL)))
			if err != nil {
				t.Fatal(err)
			}
			defer c.client.CloseIdleConnections()
			// IP-literal origins need no DNS lookup.
			if err := c.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c.snapshot["cloud.example.com."][0].ip.String() != "127.0.0.1" {
				t.Fatal("resource overrode instance mapping")
			}
			if c.snapshot["app.example.com."][0].ip.String() != "198.51.100.1" {
				t.Fatal("resource mapping changed")
			}
			// Exercise DNS refresh without changing the resource HTTP endpoint.
			origin := c.baseURL
			c.baseURL = "http://api.example.invalid"
			c.connectHost = connectHost
			expectedHost := connectHost
			if expectedHost == "" {
				expectedHost = "api.example.invalid"
			}
			ips := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("::ffff:192.0.2.1")}
			var lookupErr error
			c.lookupIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
				if network != "ip" || host != expectedHost {
					t.Errorf("unexpected lookup: %s %s", network, host)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("lookup must have a timeout")
				}
				return ips, lookupErr
			}
			// Redirect requests to the mock API while retaining a hostname-based origin.
			transport := c.client.Transport.(*http.Transport)
			originalDial := transport.DialContext
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return originalDial(ctx, network, strings.TrimPrefix(origin, "http://"))
			}
			transport.Proxy = nil
			if err := c.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.snapshot["cloud.example.com."]) != 2 {
				t.Fatal("expected deduplicated A/AAAA targets")
			}
			lookupErr = fmt.Errorf("DNS unavailable")
			body = `[{"ip":"198.51.100.2","domains":["app.example.com"]}]`
			if err := c.refresh(context.Background()); err == nil {
				t.Fatal("expected DNS failure")
			}
			if len(c.snapshot["cloud.example.com."]) != 2 || c.snapshot["app.example.com."][0].ip.String() != "198.51.100.2" {
				t.Fatal("failed lookup did not retain instance targets while resources updated")
			}
			lookupErr = nil
			ips = []netip.Addr{netip.MustParseAddr("192.0.2.2")}
			if err := c.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ts := c.snapshot["cloud.example.com."]; len(ts) != 1 || ts[0].ip.String() != "192.0.2.2" {
				t.Fatal("instance address did not update")
			}
			ips = nil
			if err := c.refresh(context.Background()); err == nil {
				t.Fatal("expected empty lookup failure")
			}
			if c.snapshot["cloud.example.com."][0].ip.String() != "192.0.2.2" {
				t.Fatal("empty lookup discarded last address")
			}
		})
	}
}

func TestCoolifyDomainConfiguration(t *testing.T) {
	for _, input := range []string{"", "https://cloud.example.com", "*.example.com", "cloud.example.com extra", "cloud.example.com\n coolify_domain other.example.com"} {
		if _, err := parse(controller("coolify https://api.example.com {\n token secret\n server one\n coolify_domain " + input + "\n}")); err == nil {
			t.Fatalf("accepted invalid domain config %q", input)
		}
	}
}

// unavailableAPI makes outage coverage independent of external networking.
type unavailableAPI struct{}

func (unavailableAPI) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("API unavailable")
}
