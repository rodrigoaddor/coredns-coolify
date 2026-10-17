package coolify

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func TestCNAMEAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		qtype uint16
		want  []string
	}{
		{"IPv4", dns.TypeA, []string{"APP.example.com.\t60\tIN\tCNAME\tserver.example.com.", "server.example.com.\t60\tIN\tA\t10.0.1.10"}},
		{"IPv6", dns.TypeAAAA, []string{"APP.example.com.\t60\tIN\tCNAME\tserver.example.com.", "server.example.com.\t60\tIN\tAAAA\t2001:db8::10"}},
		{"ANY", dns.TypeANY, []string{"APP.example.com.\t60\tIN\tCNAME\tserver.example.com.", "server.example.com.\t60\tIN\tA\t10.0.1.10", "server.example.com.\t60\tIN\tAAAA\t2001:db8::10"}},
		{"CNAME", dns.TypeCNAME, []string{"APP.example.com.\t60\tIN\tCNAME\tserver.example.com."}},
		{"TXT", dns.TypeTXT, []string{"APP.example.com.\t60\tIN\tCNAME\tserver.example.com."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coolify{ttl: 60, zones: plugin.Zones{"example.com."}, snapshot: records{
				"app.example.com.":    {{alias: "server.example.com."}},
				"server.example.com.": {{ip: netip.MustParseAddr("10.0.1.10")}, {ip: netip.MustParseAddr("2001:db8::10")}},
			}}
			c.Next = plugin.HandlerFunc(func(context.Context, dns.ResponseWriter, *dns.Msg) (int, error) {
				t.Fatal("local targets must not use the next plugin")
				return dns.RcodeServerFailure, nil
			})
			query := new(dns.Msg)
			query.SetQuestion("APP.example.com.", tc.qtype)
			w := dnstest.NewRecorder(&test.ResponseWriter{})
			code, err := c.ServeDNS(context.Background(), w, query)
			if code != dns.RcodeSuccess || err != nil || w.Msg == nil || !w.Msg.Authoritative {
				t.Fatalf("code=%d err=%v response=%v", code, err, w.Msg)
			}
			var got []string
			for _, rr := range w.Msg.Answer {
				got = append(got, rr.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("answers=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCNAMEUpstream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
		fail  bool
		empty bool
	}{
		{name: "IPv4", qtype: dns.TypeA},
		{name: "IPv6", qtype: dns.TypeAAAA},
		{name: "NODATA", qtype: dns.TypeA, empty: true},
		{name: "NXDOMAIN", qtype: dns.TypeA, rcode: dns.RcodeNameError, empty: true},
		{name: "SERVFAIL", qtype: dns.TypeA, rcode: dns.RcodeServerFailure},
		{name: "error", qtype: dns.TypeA, rcode: dns.RcodeServerFailure, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coolify{ttl: 60, zones: plugin.Zones{"example.com."}, snapshot: records{
				"app.example.com.":    {{alias: "middle.example.com."}},
				"middle.example.com.": {{alias: "server.example.net."}},
				// Targets outside our zones must be resolved by the next plugin.
				"server.example.net.": {{ip: netip.MustParseAddr("192.0.2.1")}},
			}}
			calls := 0
			var upstream *dns.Msg
			c.Next = plugin.HandlerFunc(func(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
				calls++
				if r.Question[0].Name != "server.example.net." || r.Question[0].Qtype != tc.qtype || !r.RecursionDesired {
					t.Fatalf("unexpected upstream query: %v", r)
				}
				if tc.fail {
					return tc.rcode, errors.New("lookup failed")
				}
				upstream = new(dns.Msg)
				upstream.SetReply(r)
				upstream.Rcode = tc.rcode
				upstream.RecursionAvailable = true
				upstream.AuthenticatedData = true
				if !tc.empty {
					value := "server.example.net. 30 IN A 10.0.1.10"
					if tc.qtype == dns.TypeAAAA {
						value = "server.example.net. 30 IN AAAA 2001:db8::10"
					}
					rr, _ := dns.NewRR(value)
					upstream.Answer = []dns.RR{rr}
				} else {
					rr, _ := dns.NewRR("example.net. 30 IN SOA ns.example.net. hostmaster.example.net. 1 60 60 60 30")
					upstream.Ns = []dns.RR{rr}
				}
				return tc.rcode, w.WriteMsg(upstream)
			})
			query := new(dns.Msg)
			query.SetQuestion("app.example.com.", tc.qtype)
			original := query.String()
			w := dnstest.NewRecorder(&test.ResponseWriter{})
			code, err := c.ServeDNS(context.Background(), w, query)
			if calls != 1 || code != tc.rcode || (err != nil) != tc.fail {
				t.Fatalf("calls=%d code=%d err=%v", calls, code, err)
			}
			if query.String() != original {
				t.Fatal("client query was mutated")
			}
			if tc.rcode == dns.RcodeServerFailure {
				if w.Msg != nil {
					t.Fatal("failed resolution wrote a partial response")
				}
				return
			}
			if w.Msg == nil || w.Msg.Authoritative || w.Msg.AuthenticatedData || !w.Msg.RecursionAvailable || w.Msg.Question[0].Name != "app.example.com." || w.Msg.Rcode != tc.rcode {
				t.Fatalf("unexpected response: %v", w.Msg)
			}
			if len(w.Msg.Answer) != 2+len(upstream.Answer) || (len(upstream.Answer) > 0 && !reflect.DeepEqual(w.Msg.Answer[2:], upstream.Answer)) || !reflect.DeepEqual(w.Msg.Ns, upstream.Ns) {
				t.Fatalf("upstream response was not merged: %v", w.Msg)
			}
		})
	}
}

func TestCNAMEChainFailure(t *testing.T) {
	for _, loop := range []bool{false, true} {
		t.Run(fmt.Sprintf("loop=%t", loop), func(t *testing.T) {
			c := &Coolify{ttl: 60, zones: plugin.Zones{"."}, snapshot: records{}}
			for i := 0; i < 17; i++ {
				c.snapshot[fmt.Sprintf("alias%d.example.com.", i)] = []target{{alias: fmt.Sprintf("alias%d.example.com.", i+1)}}
			}
			if loop {
				c.snapshot["alias1.example.com."] = []target{{alias: "alias0.example.com."}}
			}
			query := new(dns.Msg)
			query.SetQuestion("alias0.example.com.", dns.TypeA)
			w := dnstest.NewRecorder(&test.ResponseWriter{})
			if code, err := c.ServeDNS(context.Background(), w, query); code != dns.RcodeServerFailure || err == nil || w.Msg != nil {
				t.Fatalf("code=%d err=%v response=%v", code, err, w.Msg)
			}
		})
	}
}
