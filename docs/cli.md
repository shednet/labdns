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

`--source-kind` accepts `Ingress` or `HTTPRoute`, and `--record-type` accepts
`A` or `AAAA`; values are case-insensitive. Other values are rejected instead
of being treated as filters that happen to match no records.

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
`--request-timeout`, `--output table|json`, and `--color auto|always|never`.
Colour defaults to `auto`: table output is coloured only when stdout is a
terminal, `NO_COLOR` is unset or empty, and `TERM` is not `dumb`. `always`
forces colour even when output is redirected, while `never` disables it.
JSON output is always plain and is not currently a versioned compatibility API.
When a managed object's lifecycle annotation is invalid, `activeTargets` is
`null` because labdns cannot safely distinguish active and retiring targets.
Help, version, and error output are also always plain. The colour palette is
semantic: green marks healthy/current/observed or fully ready values, yellow
marks transitional or warning values, and red marks failures and invalid
values. Ordinary identifiers, headings, names, targets, and other data remain
uncoloured.

`status` discovers Deployments with `app.kubernetes.io/name=labdns`. Customized
installations can use `--controller-namespace` and `--controller-name`.
