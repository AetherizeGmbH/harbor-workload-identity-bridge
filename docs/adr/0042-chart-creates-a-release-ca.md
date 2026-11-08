# 42. The chart creates a CA for the release when no issuer is named

## Status

Accepted, 2026-11-08. Refines ADR-0008 (TLS and mTLS between plugin and
bridge) for the chart's default. Answers the threat model's open item O2
(mTLS client identity) for the default install.

## Context

- With `tls.enabled` (the default), the chart asks cert-manager for the
  bridge's serving certificate, and `tls.issuerRef.name` was required. The
  plugin pins the `ca.crt` of that certificate's Secret on every node; the
  sync container copies a changed `ca.crt` to the nodes (every 60 s).
- The README quickstart told users to create a selfSigned ClusterIssuer.
  A selfSigned issuer signs each certificate with that certificate's own
  key and returns the certificate itself as `ca.crt` (cert-manager
  v1.21.2, `pkg/controller/certificaterequests/selfsigned/selfsigned.go`).
  With `rotationPolicy: Always`, every renewal (every 75 days) brings a
  new key and a new `ca.crt`. Until a node's sync pass has copied it, the
  plugin on that node does not trust the bridge, and pulls of matched
  images fail there.
- `bridge.mTLS` needs a CA issuer: the bridge trusts the `ca.crt` of its
  own TLS Secret for client certificates, so a selfSigned issuer cannot
  sign them (values.yaml). Users had to build a CA chain by hand; the e2e
  harness built one (a selfSigned bootstrap ClusterIssuer, a root CA
  Certificate and a CA ClusterIssuer).
- Threat model item O2: with mTLS on, the bridge accepts any client
  certificate its CA signed. With a shared ClusterIssuer, anyone who may
  create cert-manager Certificates in any namespace can get one.

## Decision

1. When `tls.enabled` and `tls.issuerRef.name` is empty, the chart creates,
   in the release namespace:
   - an Issuer `<fullname>-selfsigned` (`selfSigned: {}`);
   - a CA Certificate `<fullname>-ca` from it, with the Secret
     `<fullname>-ca`: ECDSA P-256, valid 10 years, renewed one year before
     it expires, `privateKey.rotationPolicy: Never`, so a renewal keeps the
     key, and the CA certificate a node pins still verifies what the
     renewed CA signs;
   - an Issuer `<fullname>-ca` (`ca.secretName: <fullname>-ca`) that signs
     the serving certificate.
2. With `bridge.mTLS.enabled` and an empty `bridge.mTLS.clientIssuerRef.name`,
   the same Issuer signs the plugin's client certificate. An Issuer signs
   only for its own namespace. With cert-manager's defaults, getting a
   client certificate the bridge accepts then takes creating a Certificate
   or CertificateRequest in the release namespace, or reading the CA
   Secret there.
3. A named issuer works as before. `plugin.namespace` (the plugin in its own
   namespace, ADR-0027) still needs `tls.issuerRef.name`: the plugin
   namespace gets the CA through trust-manager from a source in the trust
   namespace, and a namespaced Issuer cannot sign the client certificate in
   another namespace. With a named issuer, mTLS still needs
   `bridge.mTLS.clientIssuerRef.name`, but only with the chart's plugin: the
   chart issues no client certificate with `plugin.enabled=false`.
4. The chart refuses `harbor.adminCredsSecret.name` or `harbor.caSecret.name`
   equal to a Secret cert-manager writes for the release (`<fullname>-tls`,
   `<fullname>-ca`): cert-manager would overwrite it.

## Consequences

- A first install needs three values: `clusterName`, `harbor.url` and
  `harbor.adminCredsSecret.name` (with ADR-0041). cert-manager must still be
  installed.
- The serving certificate's renewals no longer change the CA the nodes pin.
  `ca.crt` changes when cert-manager renews the CA (after nine years); the
  key stays, so the old and the new CA certificate verify the same
  certificates, and the nodes get the new one with the next serving
  certificate.
- Switching an existing release from a named issuer to the release CA (by
  clearing `tls.issuerRef.name`) changes `ca.crt` once: the plugin on each
  node trusts the new CA after its next sync pass (MIGRATION.md).
- cert-manager keeps a certificate's Secret after `helm uninstall` unless
  it runs with `--enable-certificate-owner-ref`. A new install of the same
  release name into the same namespace then finds the CA Secret and keeps
  the same CA. Delete the Secret `<fullname>-ca` to get a new one.
- The e2e harness's main release uses the release CA in every stage,
  including mTLS; the second release keeps a named CA ClusterIssuer.

## Alternatives considered

- **Keep `tls.issuerRef.name` required.** Rejected: it is the step of the
  quickstart that needs cert-manager knowledge, and the issuer the
  quickstart suggested rotates the pinned CA every 75 days.
- **Generate the CA with Helm's `genCA` at render time.** Rejected: each
  render makes a new CA unless the chart reads the old one back with
  `lookup`, which `helm template` and dry runs cannot do, and the CA key
  would be stored in the Helm release.
- **A ClusterIssuer for the release CA.** Rejected: its CA Secret must be
  in cert-manager's cluster resource namespace, outside the release, and
  anyone who may reference the ClusterIssuer could get client certificates,
  which is O2 again.
