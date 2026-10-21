// Package coolify implements DNS records discovered from a Coolify instance.
package coolify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
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
	Next           plugin.Handler
	baseURL, token string
	persistPath    string
	debug          bool
	logDiscovery   bool
	dashboardName  string
	publish        []target
	servers        []server
	autoServers    bool
	discovered     []server
	zones          plugin.Zones
	interval       time.Duration
	ttl            uint32
	client         *http.Client
	refreshMu      sync.Mutex
	mu             sync.RWMutex
	byServer       map[string]records
	snapshot       records
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
	c.mu.RLock()
	previous := c.snapshot
	c.mu.RUnlock()
	var failures []string
	if c.autoServers {
		servers, err := c.fetchServers(ctx)
		if err != nil {
			failures = append(failures, fmt.Sprintf("server discovery: %v", err))
		} else {
			if c.logDiscovery {
				known := map[string]bool{}
				for _, s := range c.discovered {
					known[s.uuid] = true
				}
				for _, s := range servers {
					if !known[s.uuid] {
						log.Infof("discovered server %q", s.uuid)
					}
				}
			}
			c.discovered = servers
		}
	}
	servers, active := c.activeServers()
	for uuid := range c.byServer {
		if !active[uuid] {
			delete(c.byServer, uuid)
		}
	}
	for _, s := range servers {
		fresh, err := c.fetch(ctx, s)
		if err != nil {
			failures = append(failures, fmt.Sprintf("server %s: %v", s.uuid, err))
			continue
		}
		c.byServer[s.uuid] = fresh
	}
	c.rebuildSnapshot(servers)
	if c.logDiscovery {
		c.logRecordAdditions(previous)
	}
	if err := c.saveState(); err != nil {
		failures = append(failures, fmt.Sprintf("persist cache: %v", err))
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// activeServers preserves explicit precedence over discovered servers.
func (c *Coolify) activeServers() ([]server, map[string]bool) {
	servers := append([]server(nil), c.servers...)
	active := map[string]bool{}
	for _, s := range servers {
		active[s.uuid] = true
	}
	for _, s := range c.discovered {
		if !active[s.uuid] {
			servers = append(servers, s)
			active[s.uuid] = true
		}
	}
	return servers, active
}

func (c *Coolify) rebuildSnapshot(servers []server) {
	merged := records{}
	for _, s := range servers {
		for name, ts := range c.byServer[s.uuid] {
			if _, exists := merged[name]; !exists {
				merged[name] = ts
			}
		}
	}
	if len(c.publish) > 0 {
		if _, exists := merged[c.dashboardName]; exists {
			log.Warningf("discovered resource %s conflicts with dashboard publish record; using publish target", c.dashboardName)
		}
		merged[c.dashboardName] = c.publish
	}
	c.mu.Lock()
	c.snapshot = merged
	c.mu.Unlock()
}

// logRecordAdditions compares published snapshots, including target changes.
// Restored cache entries form the baseline and are not logged as new discoveries.
func (c *Coolify) logRecordAdditions(previous records) {
	c.mu.RLock()
	current := c.snapshot
	c.mu.RUnlock()
	var additions []string
	for name, targets := range current {
		if c.zones.Matches(name) == "" {
			continue
		}
		known := map[target]bool{}
		for _, t := range previous[name] {
			known[t] = true
		}
		for _, t := range targets {
			if known[t] {
				continue
			}
			kind, value := "CNAME", t.alias
			if t.ip.IsValid() {
				kind, value = "AAAA", t.ip.String()
				if t.ip.Is4() {
					kind = "A"
				}
			}
			additions = append(additions, fmt.Sprintf("%s %s %s", name, kind, value))
		}
	}
	sort.Strings(additions)
	for _, record := range additions {
		log.Infof("discovered record %s", record)
	}
}

func validServerUUID(uuid string) bool {
	return uuid != "" && !strings.ContainsAny(uuid, "/?#%") && uuid != "." && uuid != ".."
}

// fetchServers returns a stable order so API list ordering cannot change DNS precedence.
func (c *Coolify) fetchServers(ctx context.Context) ([]server, error) {
	var rows []struct {
		UUID string `json:"uuid"`
	}
	if err := c.get(ctx, "/api/v1/servers", &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("invalid servers JSON")
	}
	servers := make([]server, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if !validServerUUID(row.UUID) || seen[row.UUID] {
			return nil, fmt.Errorf("invalid or duplicate server UUID in API response")
		}
		seen[row.UUID] = true
		servers = append(servers, server{uuid: row.UUID})
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].uuid < servers[j].uuid })
	return servers, nil
}

// get bounds API responses. Debug errors include transport and read failures,
// never request headers or response bodies.
func (c *Coolify) get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		if c.debug {
			return fmt.Errorf("API request failed: %w", err)
		}
		return fmt.Errorf("API request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	const maxBody = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if c.debug {
			return fmt.Errorf("reading API response failed: %w", err)
		}
		return fmt.Errorf("reading API response failed")
	}
	if len(data) > maxBody {
		return fmt.Errorf("API response exceeds 8 MiB")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("invalid API JSON")
	}
	return nil
}

func (c *Coolify) fetch(ctx context.Context, s server) (records, error) {
	var rows []struct {
		IP      string   `json:"ip"`
		Domains []string `json:"domains"`
	}
	if err := c.get(ctx, "/api/v1/servers/"+url.PathEscape(s.uuid)+"/domains", &rows); err != nil {
		return nil, err
	}
	if rows == nil {
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
