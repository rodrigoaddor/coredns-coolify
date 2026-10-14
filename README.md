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
go get github.com/rodrigoaddor/coredns-coolify@v1.0.0
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
- `connect_ip IP`: connect directly to this IPv4 or IPv6 address while keeping the URL hostname for HTTPS and HTTP routing. The URL determines the port. When set, requests bypass environment HTTP proxies.
- `token_env NAME`: read the bearer token from an environment variable at startup. Alternatively use `token VALUE`; exactly one token directive is required.
- `server UUID [TARGET]`: repeat for each server. Without a target, each API row supplies its own address. A target overrides all rows for that server.
- `refresh DURATION`: polling interval, default `1m`; must be positive.
- `timeout DURATION`: HTTP timeout per server request, default `10s`; must be positive.
- `ttl SECONDS`: unsigned DNS TTL, default `60`; zero disables client caching.

Create an API token in Coolify's **Keys & Tokens → API tokens** and enable API access. Tokens are sent in `Authorization: Bearer ...`, as required by the [Coolify domains endpoint](https://coolify.io/docs/api/endpoints/servers/get-domains-by-server-uuid). The plugin calls `GET /api/v1/servers/UUID/domains` for each configured server. HTTP redirects are refused to avoid forwarding credentials.

## Connecting without DNS

If Coolify's hostname depends on this CoreDNS instance for resolution, set `connect_ip` to an address reachable from the CoreDNS process:

```corefile
coolify https://cloud.example.com {
    token_env COOLIFY_API_TOKEN
    connect_ip 10.0.0.20
    server server-uuid-one
}
```

The plugin connects to `10.0.0.20` and uses `cloud.example.com` for the HTTP Host header, TLS server name, and certificate verification. This avoids a DNS dependency when starting CoreDNS. An explicit port in the URL, such as `https://cloud.example.com:8443`, is preserved.

`connect_ip` controls the API connection. The optional target on each `server` directive controls the DNS records returned to clients.

## DNS and refresh behavior

IPv4 targets produce A records, IPv6 targets produce AAAA records, and hostname targets produce CNAME records. CNAME records are also returned for A/AAAA queries so recursive resolvers can follow the alias.

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
