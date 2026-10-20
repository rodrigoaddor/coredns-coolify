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
go get github.com/rodrigoaddor/coredns-coolify@latest
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
        connect_host 192.0.2.10
        publish edge.example.net
        servers auto
        server server-uuid-one
        server server-uuid-two 192.0.2.20
        server server-uuid-three edge.example.net
        refresh 1m
        timeout 10s
        ttl 60
    }
    # Replace these documentation addresses with your DNS resolvers.
    forward . 192.0.2.53 198.51.100.53
}
```

- `coolify URL`: the Coolify API origin and dashboard DNS hostname. An explicit HTTP or HTTPS scheme is required. Paths, credentials, queries, and fragments are rejected.
- `token_env NAME`: read the bearer token from an environment variable at startup. Required.
- `connect_host HOST`: optionally connect to this IPv4 address, IPv6 address, or hostname (for example, `host.docker.internal`) while preserving the URL's HTTP Host header, TLS SNI, and certificate verification hostname. The URL determines the port. When set, requests bypass environment HTTP proxies. When omitted, connections use the URL hostname.
- `publish TARGET`: optionally publish the dashboard hostname from the URL. IPv4 produces A, IPv6 produces AAAA, and a hostname produces CNAME. The record is available immediately, before any API request. Omission disables dashboard publishing. This target is independent of `connect_host`.
- `server UUID [TARGET]`: discover domains for this server. Without a target, each API row supplies its own address. A target overrides all rows for that server. Repeat for multiple servers.
- `servers auto`: discover all servers using `GET /api/v1/servers` each refresh. Explicit `server` directives override individual server targets and are queried even if absent from the discovered list.
- `refresh DURATION`: polling interval, default `1m`; must be positive.
- `timeout DURATION`: HTTP timeout per API request, default `10s`; must be positive.
- `ttl SECONDS`: unsigned DNS TTL, default `60`; zero disables client caching.

Create an API token in Coolify's **Keys & Tokens → API tokens** and enable API access. Tokens are sent in `Authorization: Bearer ...`, as required by the [Coolify domains endpoint](https://coolify.io/docs/api/endpoints/servers/get-domains-by-server-uuid). The plugin calls `GET /api/v1/servers/UUID/domains` for each configured or discovered server. Auto discovery uses the [Coolify server list endpoint](https://coolify.io/docs/api/endpoints/servers/list-servers). HTTP redirects are refused to avoid forwarding credentials.

## Connecting through the Docker host

If Coolify's hostname depends on this CoreDNS instance for resolution, set `connect_host` to an address or hostname reachable from the CoreDNS process. When running on the Coolify host:

```corefile
coolify https://cloud.example.com {
    token_env COOLIFY_API_TOKEN
    connect_host host.docker.internal
    publish edge.example.net
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

The Coolify proxy must listen on the host port specified by the API URL (443 by default for HTTPS). You can also use a fixed address, such as `connect_host 192.0.2.20`.

`connect_host` controls only outbound API connections. `publish` controls the dashboard DNS record, and the optional target on each `server` directive controls application DNS records. A connection address is never used to infer a dashboard DNS target.

For example:

```corefile
coolify https://cloud.example.com {
    token_env COOLIFY_API_TOKEN
    connect_host 192.0.2.10
    publish edge.example.net
    server server-uuid-one edge.example.net
}
```

API requests connect to `192.0.2.10:443` with `cloud.example.com` for HTTPS verification and HTTP routing. DNS queries for `cloud.example.com` and discovered applications return CNAMEs targeting `edge.example.net`. Hostname targets are preserved during refresh; they are resolved through the CoreDNS plugin chain at query time, including by `coredns-tailscale` when it follows `coolify` in the compiled plugin order.

The dashboard record works even when the first API refresh fails. It takes precedence over discovered resources with the same hostname, and collisions log a warning. Publishing requires a DNS hostname in the API URL within the CoreDNS server block's zones. Malformed targets and dashboard CNAMEs pointing to themselves are rejected at configuration time. At least one `server`, `servers auto`, or `publish` directive is required.

## DNS and refresh behavior

IPv4 targets produce A records, IPv6 targets produce AAAA records, and hostname targets produce CNAME records. For A/AAAA and ANY queries, the plugin follows hostname targets and includes their address records in the same answer:

```text
app.example.com.     60 IN CNAME server.example.com.
server.example.com.  60 IN A     198.51.100.10
```

Alias targets inside the configured zones are read from the same snapshot. Targets absent from the snapshot or outside the configured zones are resolved through the next plugin (for example, `coredns-tailscale`, `hosts`, or `forward`) during the DNS query, preserving upstream TTLs and negative responses. Responses that use the next plugin are not marked authoritative. Without a next plugin, unresolved targets return only the CNAME chain. Explicit CNAME queries return only the alias. Local loops or chains exceeding 16 names return SERVFAIL.

Names are case insensitive. Both bare API domain names and HTTP(S) domain URLs (with ports or paths) are accepted. Names must use ASCII DNS hostname syntax; use punycode for internationalized names. Targets must be plain IPs or hostnames, without schemes, ports, or paths.

Dashboard publishing is initialized during configuration. Application discovery refreshes in the background on startup and then periodically. Queries read an in-memory snapshot and never wait for HTTP. A failed request or invalid response retains that server's last successful records and logs a warning; other servers can still update. A successful empty array removes that server's records. Before the first successful fetch, that server contributes no records. Records remain available throughout an outage. Refresh requests run sequentially, so a discovery cycle can take up to the configured timeout per API request, including the server list request when auto discovery is enabled. Initial discovery does not block DNS startup. If the server list request fails or returns invalid data, the plugin retains the previous list and continues refreshing those servers. A successful list removes records for departed auto-discovered servers; explicit servers remain configured.

Within one server, multiple addresses for the same name form an A/AAAA record set and duplicates are removed. Conflicting CNAME targets, CNAME/address combinations, and self aliases reject that server's refresh. If different servers contain the same name, the first explicit server with records for it takes precedence, followed by auto-discovered servers sorted by UUID. An explicit dashboard publish record takes precedence over all servers.

Only names inside the CoreDNS server block's zones are answered. Unknown names always pass to the next plugin (usually `forward`). Known names with no requested record type return NOERROR with an empty answer. If no next plugin handles a missing name, CoreDNS returns SERVFAIL.

`cache` can be added for upstream queries, but it can also retain Coolify answers until their TTL expires after a refresh removes or changes a record.

## Development

```sh
go test -race ./...
go vet ./...
```

Tests use a mock Coolify HTTP API and cover authentication, configuration validation, Host and TLS verification, immediate dashboard publishing, API outages, A/AAAA/CNAME answers, plugin chain resolution, overrides, auto discovery, precedence, deletion, retained snapshots, refused redirects, and concurrent queries during refresh.

## License

MIT. See [LICENSE](LICENSE).
