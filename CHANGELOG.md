# Changelog

## 1.3.0

- Replace `connect_ip` with `connect_host`, accepting IP addresses or hostnames such as `host.docker.internal` while preserving HTTP routing and TLS verification.
- Resolve hostname connection overrides for dashboard A/AAAA records on each refresh.
- Document the Docker host mapping for deployments on the Coolify host.

## 1.2.0

- Include target A/AAAA records alongside CNAMEs by following local aliases and resolving other targets through the next CoreDNS plugin.
- Detect local CNAME loops and limit chain traversal to 16 names.

## 1.1.0

- Add `connect_ip` to connect directly to an IPv4 or IPv6 address while preserving the API hostname for HTTPS verification and routing.
- Add `coolify_domain` to publish dashboard A/AAAA records using `connect_ip` or the resolved API hostname addresses.
- Retain dashboard addresses when DNS lookups fail, and give the configured dashboard domain precedence over resource records.
- Add coverage for direct connections, TLS hostname verification, dashboard records, and address refreshes.

## 1.0.0

First release of the standalone CoreDNS plugin:

- Discover domains from one Coolify instance and multiple servers.
- Override each server's targets with IPv4, IPv6, or a hostname.
- Answer A, AAAA, and CNAME queries.
- Refresh records periodically and retain previous records on API failures.
- Authenticate with a bearer token configured directly or through an environment variable.
- Delegate unknown names to the next CoreDNS plugin.
