# labdns

labdns helps ExternalDNS publish DNS records for Kubernetes applications.

Kubernetes does not always provide an address that ExternalDNS can publish for
an Ingress or HTTPRoute. For example, an ingress controller may run on every
Node as a DaemonSet and have only a ClusterIP Service, rather than a
LoadBalancer address. labdns fills this gap by using addresses you assign to
the Nodes that can serve each application.

It watches opted-in Ingresses and, when enabled, HTTPRoutes. For each hostname,
labdns follows the backend Service to the Nodes that are serving it, reads the
publishable IP addresses from Node labels, and creates an ExternalDNS
`DNSEndpoint` object.

ExternalDNS reads those objects and updates your DNS service. You install and
manage ExternalDNS separately.

## How it works

1. You define a `DNSProvider`. Despite its name, this is only a labdns settings
   object. It says which DNS zones and Node address labels to use.
2. You add labdns annotations to an Ingress or HTTPRoute and choose one or more
   `DNSProvider` objects.
3. labdns finds the ready backend endpoints and the Nodes that host them.
4. labdns creates one `DNSEndpoint` for each selected `DNSProvider`.
5. A separately installed ExternalDNS deployment reads the matching
   `DNSEndpoint` objects and publishes them.

You can use different `DNSProvider` objects and ExternalDNS deployments to
publish different answers for the same hostname. For example, public DNS can
use public Node addresses while private DNS uses private addresses.

## Get started

labdns requires Kubernetes 1.35 or newer (although it's been verified to work with 1.34),
the ExternalDNS `DNSEndpoint` CRD, and separately managed ExternalDNS deployment(s).

- Follow the [quick start](docs/quick-start.md) to publish an existing Ingress.
- Follow the [installation guide](docs/installation.md) for a production setup
  or to enable Gateway API support.
- Read the [configuration guide](docs/configuration.md) for annotations,
  `DNSProvider` settings, address selection, record removal, and metrics.
- Use the [`labdns`](docs/cli.md) command to check controller health, inspect
  generated records, and optionally compare them with a DNS resolver.

## Development

Development requires Go 1.26.1 and Helm. The Makefile installs its Go tools and
Kubernetes test assets in `bin/`; Helm must already be available on `PATH`.

Run the required checks before submitting a change:

```sh
make manifests generate build lint test check-generated check-packaging
```

Do not edit generated CRDs, RBAC, or `zz_generated.*` files by hand. Update
them with the Makefile targets instead.

Maintainer and release commands also require
[`just`](https://just.systems/) on `PATH`:

```sh
just --list
just check
just release patch
```

`just release` accepts `patch`, `minor`, or `major`. It checks that `main` is
clean and up to date, runs the release checks, updates the chart version,
creates a commit and tag, and asks before pushing them. Live end-to-end tests
are a separate `just test-e2e` command and require Kind.

## License

Apache License 2.0. Copyright 2026 Konstantinos Kalyvas.
