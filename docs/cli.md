# labdns CLI

The `labdns` command inspects publication state through the Kubernetes API. It
uses the active kubeconfig and never reads provider credentials or Kubernetes
Secrets.

## Installation

Release archives contain static `labdns` binaries for Linux and macOS on amd64
and arm64. Verify the archive against `checksums.txt` from the same GitHub
release before installing it. From a checkout, build `bin/labdns` with:

```sh
make build-cli
```

Nix users can build or run the flake package:

```sh
nix build .#labdns
nix run .#labdns -- status
```

## Commands

Summarize prerequisites, controller readiness, and publication state:

```sh
labdns status
```

List every labdns-managed record, or narrow the result:

```sh
labdns list
labdns list --namespace app --provider www --record-type A
labdns list --output json | jq '.items[] | select(.externalDNSState != "observed")'
```

Show every provider/source publishing one DNS name:

```sh
labdns show app.example.com
labdns show app.example.com --provider www
```

Live comparison is opt-in because the same name may intentionally have
different split-horizon answers. Supply the resolver for the DNS view being
inspected:

```sh
labdns show app.example.com --provider vpn --dns-server 10.0.0.53
```

The comparison covers A and AAAA answers and expects the exact complete target
set in the `DNSEndpoint`, including targets still inside their deletion delay.
A matching answer proves only what the selected resolver currently returns; it
does not prove the state of an underlying DNS provider.

All commands accept `--kubeconfig`, `--context`, `--namespace`,
`--request-timeout`, and `--output table|json`. JSON is a simple interface for
lightweight `jq` use and is not currently a versioned compatibility API.

`status` discovers Deployments with `app.kubernetes.io/name=labdns`. Customized
installations can use `--controller-namespace` and `--controller-name`.
