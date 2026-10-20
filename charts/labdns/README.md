# labdns chart

This standalone chart installs labdns and the cluster-scoped `DNSProvider`
CRD. It has no chart dependencies and does not install ExternalDNS or the
ExternalDNS `DNSEndpoint` CRD.

Follow the repository's ordered installation guide before installing the
chart, or use the quick start when ExternalDNS is already publishing your
zone:

- [Quick start](https://github.com/shednet/labdns/blob/main/docs/quick-start.md)
- [Installation](https://github.com/shednet/labdns/blob/main/docs/installation.md)
- [Configuration](https://github.com/shednet/labdns/blob/main/docs/configuration.md)

Gateway API access for HTTPRoute, TCPRoute, and UDPRoute is disabled by
default. Enable `gatewayAPI.enabled` only after installing the Gateway API
v1.6.0 CRDs. Metrics are disabled by default and
can be exposed as ordinary HTTP or authenticated HTTPS. See the repository's
[`values.yaml`](https://github.com/shednet/labdns/blob/main/charts/labdns/values.yaml)
for chart settings and the configuration guide for manager flags, source
annotations, and `DNSProvider` fields.
