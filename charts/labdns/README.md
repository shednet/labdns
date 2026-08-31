# labdns chart

This standalone chart installs labdns and the cluster-scoped `DNSProvider`
CRD. It has no chart dependencies and does not install ExternalDNS or the
ExternalDNS `DNSEndpoint` CRD.

Follow the repository's ordered installation guide before installing the
chart. Gateway API access is disabled by default. Metrics are disabled by
default and can be exposed as ordinary HTTP or authenticated HTTPS; see
`values.yaml` and the configuration guide.
