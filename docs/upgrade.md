# Upgrade from legacy labdns

This release is a breaking fresh-install replacement. There is no automatic
migration, conversion webhook, compatibility schema, or legacy fallback.

1. Manually export the desired zones, Node-label sources, TTLs, deletion
   delays, and provider-specific settings from old `DNSProvider` objects.
2. Stop and uninstall the old controller.
3. Resolve or remove every old labdns-owned A, AAAA, and TXT record that could
   conflict with ExternalDNS ownership. Confirm this at each DNS backend.
4. Install the official DNSEndpoint CRD, separate ExternalDNS deployments, and
   the new labdns controller in the order documented in
   [installation.md](installation.md).
5. Recreate `DNSProvider` profiles with the new schema and annotate sources.

Do not point old and new controllers at the same zones during the transition.
The new repository intentionally does not import old branches, tags, runtime
state, credentials, or provider configuration.
