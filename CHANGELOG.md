# Changelog

## 1.4.1

- Add `COOLIFY_DEBUG=true` (or `1`) to include underlying HTTP request and response read errors in API failure warnings.
- Document how to enable debug logging and troubleshoot DNS, connection, TLS, and timeout failures.
- Add regression coverage for debug and default error messages without exposing the API token.

## 1.4.0

- Separate API connections (`connect_host`) from dashboard DNS publishing (`publish`).
- Derive the dashboard hostname from the API URL and publish configured A, AAAA, or CNAME records immediately, independently of API availability.
- Preserve hostname targets as CNAMEs for resolution through subsequent CoreDNS plugins.
- Validate dashboard zones, targets, and self aliases; warn when dashboard publishing overrides discovered resources.
- Add `servers auto` with explicit server overrides, periodic membership refresh, and retained discovery on API failures.
- Require explicit HTTP(S) API origins and environment-based tokens; remove legacy configuration directives and dashboard address lookup.

## 1.2.0

- Include target A/AAAA records alongside CNAMEs by following local aliases and resolving other targets through the next CoreDNS plugin.
- Detect local CNAME loops and limit chain traversal to 16 names.

## 1.1.0

- Add a connection override to connect directly to an IPv4 or IPv6 address while preserving the API hostname for HTTPS verification and routing.
- Add dashboard A/AAAA publishing from connection addresses (superseded by independent publishing).
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
