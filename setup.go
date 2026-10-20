package coolify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
)

var log = clog.NewWithPlugin("coolify")

func init() { plugin.Register("coolify", setup) }

func setup(controller *caddy.Controller) error {
	c, err := parse(controller)
	if err != nil {
		return plugin.Error("coolify", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var start sync.Once
	var done chan struct{}
	controller.OnStartup(func() error {
		start.Do(func() {
			done = make(chan struct{})
			go func() {
				defer close(done)
				// Serve the static dashboard record while initial discovery runs.
				if err := c.refresh(ctx); err != nil && ctx.Err() == nil {
					log.Warning(err)
				}
				ticker := time.NewTicker(c.interval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := c.refresh(ctx); err != nil && ctx.Err() == nil {
							log.Warning(err)
						}
					}
				}
			}()
		})
		return nil
	})
	controller.OnShutdown(func() error {
		cancel()
		if done != nil {
			<-done
		}
		c.client.CloseIdleConnections()
		return nil
	})
	dnsserver.GetConfig(controller).AddPlugin(func(next plugin.Handler) plugin.Handler { c.Next = next; return c })
	return nil
}

func parse(p *caddy.Controller) (*Coolify, error) {
	c := &Coolify{interval: time.Minute, ttl: 60, byServer: map[string]records{}, snapshot: records{}}
	debug := os.Getenv("COOLIFY_DEBUG")
	c.debug = debug == "1" || strings.EqualFold(debug, "true")
	timeout := 10 * time.Second
	var connectHost string
	seen := map[string]bool{}
	count := 0
	for p.Next() {
		count++
		if count > 1 {
			return nil, plugin.ErrOnce
		}
		args := p.RemainingArgs()
		if len(args) != 1 {
			return nil, p.ArgErr()
		}
		u, err := url.Parse(args[0])
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, p.Err("Coolify URL must be an HTTP(S) origin without credentials, path, query or fragment")
		}
		c.baseURL = strings.TrimRight(u.String(), "/")
		c.dashboardName = u.Hostname()
		for p.NextBlock() {
			key := p.Val()
			args := p.RemainingArgs()
			if key != "server" && seen[key] {
				return nil, p.Errf("duplicate %s directive", key)
			}
			seen[key] = true
			switch key {
			case "server":
				if len(args) < 1 || len(args) > 2 {
					return nil, p.ArgErr()
				}
				if args[0] == "" || strings.ContainsAny(args[0], "/?#%") || args[0] == "." || args[0] == ".." {
					return nil, p.Err("invalid server UUID")
				}
				for _, s := range c.servers {
					if s.uuid == args[0] {
						return nil, p.Err("duplicate server UUID")
					}
				}
				s := server{uuid: args[0]}
				if len(args) == 2 {
					if _, err := parseTarget(args[1]); err != nil {
						return nil, p.Err(err.Error())
					}
					s.override = args[1]
				}
				c.servers = append(c.servers, s)
			case "servers":
				if len(args) != 1 || args[0] != "auto" {
					return nil, p.Err("servers requires auto")
				}
				c.autoServers = true
			case "token_env":
				if len(args) != 1 {
					return nil, p.ArgErr()
				}
				c.token = os.Getenv(args[0])
			case "publish":
				if len(args) != 1 {
					return nil, p.ArgErr()
				}
				t, err := parseTarget(args[0])
				if err != nil {
					return nil, p.Err(err.Error())
				}
				c.publish = []target{t}
			case "connect_host":
				if len(args) != 1 {
					return nil, p.ArgErr()
				}
				t, err := parseTarget(args[0])
				if err != nil {
					return nil, p.Err("connect_host must be an IP address or hostname without a scheme, port, path or zone")
				}
				connectHost = strings.TrimSuffix(t.alias, ".")
				if t.ip.IsValid() {
					connectHost = t.ip.String()
				}
			case "refresh", "timeout":
				if len(args) != 1 {
					return nil, p.ArgErr()
				}
				d, err := time.ParseDuration(args[0])
				if err != nil || d <= 0 {
					return nil, p.Errf("%s must be a positive duration", key)
				}
				if key == "refresh" {
					c.interval = d
				} else {
					timeout = d
				}
			case "ttl":
				if len(args) != 1 {
					return nil, p.ArgErr()
				}
				n, err := strconv.ParseUint(args[0], 10, 32)
				if err != nil {
					return nil, p.Err("ttl must be an unsigned 32-bit number of seconds")
				}
				c.ttl = uint32(n)
			default:
				return nil, p.Errf("unknown directive %s", key)
			}
		}
	}
	if count == 0 || (len(c.servers) == 0 && !c.autoServers && len(c.publish) == 0) {
		return nil, fmt.Errorf("a Coolify URL and server, servers auto, or publish are required")
	}
	if strings.TrimSpace(c.token) == "" || strings.ContainsAny(c.token, "\r\n") {
		return nil, fmt.Errorf("a nonempty API token is required via token_env")
	}
	c.zones = plugin.Zones(plugin.OriginsFromArgsOrServerBlock(nil, p.ServerBlockKeys))
	if len(c.publish) > 0 {
		name, err := domainName(c.dashboardName, false)
		if err != nil || net.ParseIP(c.dashboardName) != nil {
			return nil, fmt.Errorf("publishing requires a DNS hostname in the Coolify URL")
		}
		c.dashboardName = name
		if c.zones.Matches(name) == "" {
			return nil, fmt.Errorf("dashboard hostname %s is outside the configured zones", name)
		}
		if c.publish[0].alias == name {
			return nil, fmt.Errorf("dashboard CNAME for %s points to itself", name)
		}
		c.snapshot[name] = c.publish
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if connectHost != "" {
		// Only change the socket destination. The URL still supplies the HTTP
		// Host header, TLS server name, and certificate verification hostname.
		transport.Proxy = nil
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(connectHost, port))
		}
	}
	c.client = &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}
