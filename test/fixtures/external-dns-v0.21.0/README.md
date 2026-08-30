# ExternalDNS DNSEndpoint CRD test fixture

`dnsendpoints.externaldns.k8s.io.yaml` is an unmodified copy of
`config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml` from the upstream
ExternalDNS v0.21.0 release. It is Apache-2.0 licensed by the Kubernetes
Authors and exists only for envtest and isolated test-cluster setup.

This fixture is intentionally outside `config/`. labdns does not install or
uninstall this prerequisite CRD.
