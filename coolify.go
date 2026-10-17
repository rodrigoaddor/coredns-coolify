// Package coolify implements DNS records discovered from a Coolify instance.
package coolify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/nonwriter"
	"github.com/miekg/dns"
)

type server struct{ uuid, override string }
type target struct {
	ip    netip.Addr
	alias string
}
type records map[string][]target

// Coolify is a CoreDNS handler. Its configuration is immutable after startup.
type Coolify struct {
	Next            plugin.Handler
	baseURL, token  string
	instanceDomain  string
	connectIP       netip.Addr
	instanceTargets []target
	lookupIP        func(context.Context, string, string) ([]netip.Addr, error)
	servers         []server
	zones           plugin.Zones
	interval        time.Duration
	ttl             uint32
	client          *http.Client
	refreshMu       sync.Mutex
	mu              sync.RWMutex
	byServer        map[string]records
	snapshot        records
}

func (c *Coolify) Name() string { return "coolify" }

func (c *Coolify) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if len(r.Question) != 1 {
		return dns.RcodeFormatError, nil
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	if q.Qclass != dns.ClassINET || c.zones.Matches(name) == "" {
		return plugin.NextOrFailure(c.Name(), c.Next, ctx, w, r)
	}
	c.mu.RLock()
	snapshot := c.snapshot
	_, found := snapshot[name]
	c.mu.RUnlock()
	if !found {
		return plugin.NextOrFailure(c.Name(), c.Next, ctx, w, r)
	}
	reply := new(dns.Msg)
	reply.SetReply(r)
	reply.Authoritative = true
	seen := map[string]bool{}
	answerName := q.Name
	for {
		if seen[name] || len(seen) >= 16 {
			return dns.RcodeServerFailure, fmt.Errorf("CNAME chain for %s loops or exceeds 16 names", q.Name)
		}
		seen[name] = true
		targets, local := snapshot[name]
		if !local || c.zones.Matches(name) == "" {
			if c.Next == nil {
				break
			}
			lookup := r.Copy()
			lookup.Question[0].Name = answerName
			captured := nonwriter.New(w)
			code, err := plugin.NextOrFailure(c.Name(), c.Next, ctx, captured, lookup)
			if err != nil || !plugin.ClientWrite(code) {
				return code, err
			}
			if captured.Msg == nil {
				return dns.RcodeServerFailure, fmt.Errorf("no response resolving CNAME target %s", answerName)
			}
			reply.Rcode = captured.Msg.Rcode
			reply.Authoritative = false
			reply.RecursionAvailable = captured.Msg.RecursionAvailable
			reply.Truncated = captured.Msg.Truncated
			reply.Answer = append(reply.Answer, captured.Msg.Answer...)
			reply.Ns = captured.Msg.Ns
			reply.Extra = captured.Msg.Extra
			break
		}
		alias := ""
		for _, t := range targets {
			h := dns.RR_Header{Name: answerName, Class: dns.ClassINET, Ttl: c.ttl}
			switch {
			case t.alias != "":
				h.Rrtype = dns.TypeCNAME
				reply.Answer = append(reply.Answer, &dns.CNAME{Hdr: h, Target: t.alias})
				alias = t.alias
			case t.ip.Is4() && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY):
				h.Rrtype = dns.TypeA
				reply.Answer = append(reply.Answer, &dns.A{Hdr: h, A: t.ip.AsSlice()})
			case t.ip.Is6() && (q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY):
				h.Rrtype = dns.TypeAAAA
				reply.Answer = append(reply.Answer, &dns.AAAA{Hdr: h, AAAA: t.ip.AsSlice()})
			}
		}
		if alias == "" || (q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA && q.Qtype != dns.TypeANY) {
			break
		}
		answerName = alias
		name = strings.ToLower(alias)
	}
	return reply.Rcode, w.WriteMsg(reply)
}

// refresh retains each failed server's last successful snapshot. Configuration
// order gives the first server precedence when multiple servers own a name.
func (c *Coolify) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	var failures []string
	if c.instanceDomain != "" {
		ts, err := c.fetchInstanceTargets(ctx)
		if err != nil {
			failures = append(failures, fmt.Sprintf("Coolify domain %s: %v", c.instanceDomain, err))
		} else {
			c.instanceTargets = ts
		}
	}
	for _, s := range c.servers {
		fresh, err := c.fetch(ctx, s)
		if err != nil {
			failures = append(failures, fmt.Sprintf("server %s: %v", s.uuid, err))
			continue
		}
		c.byServer[s.uuid] = fresh
	}
	merged := records{}
	for _, s := range c.servers {
		for name, ts := range c.byServer[s.uuid] {
			if _, exists := merged[name]; !exists {
				merged[name] = ts
			}
		}
	}
	// The explicitly configured instance domain takes precedence over resources.
	if len(c.instanceTargets) > 0 {
		merged[c.instanceDomain] = c.instanceTargets
	}
	c.mu.Lock()
	c.snapshot = merged
	c.mu.Unlock()
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// fetchInstanceTargets uses the same destination configured for API access.
// Coolify's server API may return an internal SSH hostname rather than its
// public address, so it cannot reliably supply the dashboard's DNS records.
func (c *Coolify) fetchInstanceTargets(ctx context.Context) ([]target, error) {
	if c.connectIP.IsValid() {
		return []target{{ip: c.connectIP}}, nil
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Coolify URL")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return []target{{ip: ip.Unmap()}}, nil
	}
	lookup := c.lookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	ctx, cancel := context.WithTimeout(ctx, c.client.Timeout)
	defer cancel()
	ips, err := lookup(ctx, "ip", u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("API hostname lookup failed")
	}
	ts := []target{}
	seen := map[netip.Addr]bool{}
	for _, ip := range ips {
		if !ip.IsValid() || ip.Zone() != "" {
			continue
		}
		ip = ip.Unmap()
		if !seen[ip] {
			ts = append(ts, target{ip: ip})
			seen[ip] = true
		}
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("API hostname has no IP addresses")
	}
	return ts, nil
}

func (c *Coolify) fetch(ctx context.Context, s server) (records, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/servers/"+url.PathEscape(s.uuid)+"/domains", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed")
	} // Do not log credentials or response bodies.
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	const maxBody = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading API response failed")
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("API response exceeds 8 MiB")
	}
	var rows []struct {
		IP      string   `json:"ip"`
		Domains []string `json:"domains"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		return nil, fmt.Errorf("invalid domains JSON")
	}
	out := records{}
	for _, row := range rows {
		if len(row.Domains) == 0 {
			continue
		}
		value := row.IP
		if s.override != "" {
			value = s.override
		}
		t, err := parseTarget(value)
		if err != nil {
			return nil, err
		}
		for _, domain := range row.Domains {
			name, err := domainName(domain, true)
			if err != nil {
				return nil, err
			}
			if name == t.alias {
				return nil, fmt.Errorf("CNAME for %s points to itself", name)
			}
			existing := out[name]
			duplicate := false
			for _, old := range existing {
				if old == t {
					duplicate = true
					break
				}
				if old.alias != "" || t.alias != "" {
					return nil, fmt.Errorf("conflicting CNAME targets for %s", name)
				}
			}
			if !duplicate {
				out[name] = append(existing, t)
			}
		}
	}
	return out, nil
}

func parseTarget(value string) (target, error) {
	if ip, err := netip.ParseAddr(value); err == nil && ip.Zone() == "" {
		return target{ip: ip.Unmap()}, nil
	}
	name, err := domainName(value, false)
	if err != nil {
		return target{}, fmt.Errorf("invalid IP or hostname target %q", value)
	}
	return target{alias: name}, nil
}

func domainName(value string, allowURL bool) (string, error) {
	if allowURL && strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return "", fmt.Errorf("invalid domain URL %q", value)
		}
		value = u.Hostname()
	}
	name := strings.ToLower(dns.Fqdn(value))
	if name == "." || len(name) > 254 {
		return "", fmt.Errorf("invalid domain %q", value)
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid domain %q", value)
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", fmt.Errorf("invalid domain %q", value)
			}
		}
	}
	return name, nil
}
