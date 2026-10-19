# CoreDNS Coolify plugin

A standalone CoreDNS plugin that discovers DNS records from one Coolify instance and any number of its servers. Each server can use its API-provided addresses or override every target with an IPv4 address, IPv6 address, or hostname.

## Installation

Compile the plugin into your CoreDNS build using the steps below.

Requires Go 1.26.7 or later. Tested with CoreDNS v1.14.7.

In a CoreDNS source checkout, add the following line to `plugin.cfg` before `hosts:hosts` (and therefore before `forward:forward`):

```text
coolify:github.com/rodrigoaddor/coredns-coolify
```

Run in the CoreDNS checkout:

```sh
go get github.com/rodrigoaddor/coredns-coolify@v1.2.0
go generate coredns.go
go mod tidy
go build -o coredns .
```

Run the resulting CoreDNS binary with your Corefile and Coolify token:

```sh
export COOLIFY_API_TOKEN='your-api-token'
./coredns -conf Corefile
```

See [Corefile.example](Corefile.example) for a complete example listening on TCP and UDP port 1053. Test a domain returned by Coolify with:

```sh
dig @127.0.0.1 -p 1053 app.example.com A
```

## Configuration

```corefile
.:1053 {
    errors
    coolify https://coolify.example.com {
        token_env COOLIFY_API_TOKEN
        server server-uuid-one
        server server-uuid-two 192.0.2.20
        server server-uuid-three edge.example.net
        refresh 1m
        timeout 10s
        ttl 60
    }
    forward . 1.1.1.1 9.9.9.9
}
```

- `coolify URL`: one instance origin. A bare hostname defaults to HTTPS. Explicit HTTP is supported. Paths, credentials, queries, and fragments are rejected.
- `coolify_domain DOMAIN`: publish A/AAAA records for the Coolify dashboard using `connect_host`, or the resolved IP addresses of the API URL when no connection override is set.
- `connect_host HOST`: connect directly to this IPv4 address, IPv6 address, or hostname (for example, `host.docker.internal`) while keeping the URL hostname for HTTPS and HTTP routing. The URL determines the port. When set, requests bypass environment HTTP proxies.
- `token_env NAME`: read the bearer token from an environment variable at startup. Alternatively use `token VALUE`; exactly one token directive is required.
- `server UUID [TARGET]`: repeat for each server. Without a target, each API row supplies its own address. A target overrides all rows for that server.
- `refresh DURATION`: polling interval, default `1m`; must be positive.
- `timeout DURATION`: HTTP timeout per server request, default `10s`; must be positive.
- `ttl SECONDS`: unsigned DNS TTL, default `60`; zero disables client caching.

Create an API token in Coolify's **Keys & Tokens → API tokens** and enable API access. Tokens are sent in `Authorization: Bearer ...`, as required by the [Coolify domains endpoint](https://coolify.io/docs/api/endpoints/servers/get-domains-by-server-uuid). The plugin calls `GET /api/v1/servers/UUID/domains` for each configured server. HTTP redirects are refused to avoid forwarding credentials.

## Connecting through the Docker host

If Coolify's hostname depends on this CoreDNS instance for resolution, set `connect_host` to an address or hostname reachable from the CoreDNS process. When running on the Coolify host:

```corefile
coolify https://cloud.example.com {
    token_env COOLIFY_API_TOKEN
    connect_host host.docker.internal
    coolify_domain cloud.example.com
    server server-uuid-one
}
```

The plugin resolves `host.docker.internal` through the system resolver (including `/etc/hosts`) and connects to it, while using `cloud.example.com` for the HTTP Host header, TLS server name, and certificate verification. This avoids resolving the dashboard hostname when starting CoreDNS. An explicit port in the URL, such as `https://cloud.example.com:8443`, is preserved.

On Linux, add this mapping to the CoreDNS service in your Coolify Docker Compose configuration, as described in the [Docker networking documentation](https://docs.docker.com/compose/how-tos/networking/):

```yaml
services:
  coredns:
    # Add to your existing service configuration.
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

The Coolify proxy must listen on the host port specified by the API URL (443 by default for HTTPS). You can also use a fixed address, such as `connect_host 10.0.0.20`.

`connect_host` controls the API connection. The optional target on each `server` directive controls the DNS records returned to clients.

`coolify_domain cloud.example.com` also publishes the dashboard itself as a DNS record. With the example above, it publishes the resolved IPv4 and IPv6 addresses of `host.docker.internal`, even when the API is unavailable. Clients must be able to reach those addresses. This explicit mapping takes precedence over resource records with the same name.

For dashboard records, the plugin uses a literal `connect_host` address directly or resolves its hostname on each refresh. Without `connect_host`, it uses the API URL hostname instead. All resolved IPv4 and IPv6 addresses are published; failed or empty lookups retain the previous addresses. System resolution must be able to resolve the selected hostname independently of this CoreDNS instance. These are the API access addresses, so a hostname behind a proxy publishes the proxy's addresses.

## DNS and refresh behavior

IPv4 targets produce A records, IPv6 targets produce AAAA records, and hostname targets produce CNAME records. For A/AAAA and ANY queries, the plugin follows hostname targets and includes their address records in the same answer:

```text
app.example.com.     60 IN CNAME server.example.com.
server.example.com.  60 IN A     10.0.1.10
```

Alias targets inside the configured zones are read from the same snapshot. Other targets are resolved through the next plugin (for example, `hosts` or `forward`) during the DNS query, preserving upstream TTLs and negative responses. Responses that use the next plugin are not marked authoritative. Without a next plugin, unresolved targets return only the CNAME chain. Explicit CNAME queries return only the alias. Local loops or chains exceeding 16 names return SERVFAIL.

Names are case insensitive. Both bare API domain names and HTTP(S) domain URLs (with ports or paths) are accepted. Names must use ASCII DNS hostname syntax; use punycode for internationalized names. Targets must be plain IPs or hostnames, without schemes, ports, or paths.

The plugin refreshes on startup and then periodically. Queries read an in-memory snapshot and never wait for HTTP. A failed request or invalid response retains that server's last successful records and logs a warning; other servers can still update. A successful empty array removes that server's records. Before the first successful fetch, that server contributes no records. Records remain available throughout an outage. Refresh requests run sequentially, so startup and a polling cycle can take up to the configured timeout per server.

Within one server, multiple addresses for the same name form an A/AAAA record set and duplicates are removed. Conflicting CNAME targets, CNAME/address combinations, and self aliases reject that server's refresh. If different servers contain the same name, the first configured server with records for it takes precedence.

Only names inside the CoreDNS server block's zones are answered. Unknown names always pass to the next plugin (usually `forward`). Known names with no requested record type return NOERROR with an empty answer. If no next plugin handles a missing name, CoreDNS returns SERVFAIL.

`cache` can be added for upstream queries, but it can also retain Coolify answers until their TTL expires after a refresh removes or changes a record.

## Development

```sh
go test -race ./...
go vet ./...
```

Tests use a mock Coolify HTTP API and cover authentication, parsing, A/AAAA/CNAME answers, overrides, fallthrough, duplicate-server precedence, record deletion, retained snapshots on failures, refused redirects, and concurrent queries during refresh.

## License

MIT. See [LICENSE](LICENSE).
