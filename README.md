# labdns

labdns is a Kubernetes operator that projects Ingress and Gateway API sources
into ExternalDNS `DNSEndpoint` resources. ExternalDNS remains separately
installed and is solely responsible for talking to DNS providers.

labdns never writes DNS providers directly and never reads provider Secrets.
It resolves source backends through EndpointSlices and Node labels, then emits
one durable DNSEndpoint per source and logical DNSProvider. Provider-label
filters keep independently managed ExternalDNS deployments isolated.

Start with the ordered [installation guide](docs/installation.md). See
[configuration](docs/configuration.md) for flags, metrics, split-horizon
isolation, and examples. Existing installations must follow the explicit
[breaking upgrade procedure](docs/upgrade.md).

## Development

The project requires Go 1.26.1 and Helm. Go-based build and test tools are
installed under `bin/` by the Makefile; Helm must be available on `PATH`.

```sh
make manifests generate
make build
make lint
make test
make check-generated
make check-packaging
```

Generated CRDs, RBAC, and `zz_generated.*` files must be updated through the
generator targets and never edited by hand.

Maintainer and release recipes require [`just`](https://just.systems/) to be
installed separately and available on `PATH`:

```sh
just --list
just check
just release patch
```

`release` accepts `patch`, `minor`, or `major`. It verifies synchronized clean
`main`, runs the complete non-live-E2E gate, updates the chart version, creates
the release commit and annotated tag, then prompts once before pushing them.
The separately listed `test-e2e` recipe is live and is never part of `check`.
It also requires Kind to be installed separately and available on `PATH`.

## License

Apache License 2.0. Copyright 2026 Konstantinos Kalyvas.
