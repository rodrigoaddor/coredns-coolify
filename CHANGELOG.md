# Changelog

## 1.0.0

First release of the standalone CoreDNS plugin:

- Connect to Coolify through an optional fixed IP while preserving the HTTPS hostname.
- Discover domains from one Coolify instance and multiple servers.
- Override each server's targets with IPv4, IPv6, or a hostname.
- Answer A, AAAA, and CNAME queries.
- Refresh records periodically and retain previous records on API failures.
- Authenticate with a bearer token configured directly or through an environment variable.
- Delegate unknown names to the next CoreDNS plugin.
