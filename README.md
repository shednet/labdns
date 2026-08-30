# labdns

labdns is a Kubernetes operator that projects Ingress and Gateway API sources
into ExternalDNS `DNSEndpoint` resources. ExternalDNS remains separately
installed and is solely responsible for talking to DNS providers.

This repository is a greenfield Kubebuilder v4 project. The controller and
deployment documentation are introduced in later implementation stages; the
current scaffold contains the cluster-scoped `DNSProvider` API and manager
health/readiness endpoints.

## Development

The project requires Go 1.26.1. All project tools are pinned and installed
under `bin/` by the Makefile.

```sh
make manifests generate
make build
make lint
make test
make verify-generated
```

Generated CRDs, RBAC, and `zz_generated.*` files must be updated through the
generator targets and never edited by hand.

## License

Apache License 2.0. Copyright 2026 Konstantinos Kalyvas.
