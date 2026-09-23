# Installing the plugin without the chart's DaemonSet

Use this when the nodes get the kubelet credential-provider plugin from
somewhere else: a Talos system extension, a baked node image, or your own
configuration management (ADR-0024, issue #46).

## 1. Install the chart with `plugin.enabled=false`

```yaml
clusterName: prod
harbor:
  url: https://harbor.example.com
  adminCredsSecret:
    name: harbor-admin
plugin:
  enabled: false
  audience: harbor-bridge-prod   # still required: kubelet needs RBAC for it
tls:
  issuerRef:
    name: my-cluster-issuer
```

The chart then renders the bridge, its Services, and the ClusterRole that lets
every kubelet request ServiceAccount tokens for `plugin.audience` (ADR-0017).
It renders no DaemonSet, no plugin ConfigMap, and no plugin ServiceAccount.
`helm install` prints the provider entry to use (NOTES).

## 2. Put the plugin binary on every node

The binary is in the plugin image (multi-arch, cosign-signed, SBOM-attested):

```bash
crane export ghcr.io/aetherizegmbh/harbor-workload-identity-bridge-plugin:<version> - \
  | tar -x plugin/harbor-bridge-plugin
```

Verify the image signature first (`cosign verify`, see SECURITY.md). The file
must be named `harbor-bridge-plugin` and be executable by kubelet.

## 3. Give kubelet the provider entry

kubelet needs `--image-credential-provider-bin-dir` (the directory with the
binary) and `--image-credential-provider-config` (a `CredentialProviderConfig`
that contains this entry):

```yaml
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: harbor-bridge-plugin
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages:
      - harbor.example.com          # bare host: matchImages globs only the domain
    defaultCacheDuration: 1h
    env:
      - name: HARBOR_BRIDGE_ENDPOINT
        value: https://127.0.0.1:31443   # the bridge NodePort on the node itself
      - name: HARBOR_BRIDGE_CA_BUNDLE
        # A file path OR the PEM itself. Inline PEM suits platforms where
        # you cannot place files next to kubelet.
        value: |
          -----BEGIN CERTIFICATE-----
          ...ca.crt of the bridge TLS Secret...
          -----END CERTIFICATE-----
    tokenAttributes:
      serviceAccountTokenAudience: harbor-bridge-prod   # = plugin.audience
      requireServiceAccount: true
      cacheType: ServiceAccount
```

Get the CA with:

```bash
kubectl -n harbor-bridge-system get secret harbor-bridge-tls -o jsonpath='{.data.ca\.crt}' | base64 -d
```

Notes:

- `HARBOR_BRIDGE_ENDPOINT` is not templated here. The chart's installer
  replaces a literal `$(NODE_IP)`; without the installer, write a concrete URL.
  If your dataplane does not route loopback NodePorts, use a node IP or an
  internal load balancer in front of the bridge Service.
- The CA rotates when cert-manager reissues it. With the inline form you must
  roll the new CA out yourself; prefer a long-lived CA issuer.
- With `bridge.mTLS.enabled=true` the plugin must also get
  `HARBOR_BRIDGE_CLIENT_CERT` and `HARBOR_BRIDGE_CLIENT_KEY` (file paths). The
  chart does not issue the client certificate when `plugin.enabled=false`.
- kubelet must be 1.34 or newer (KEP-4412 ServiceAccount tokens for credential
  providers, beta and on by default).

## Talos Linux

Talos (1.6 and newer) configures the two kubelet flags itself from
`machine.kubelet.credentialProviderConfig`, and expects provider binaries in
`/usr/local/lib/kubelet/credentialproviders`, delivered by a system extension.

1. Build a system extension that installs `harbor-bridge-plugin` into
   `/usr/local/lib/kubelet/credentialproviders/` (see the Talos extensions
   repository for the layout; the ECR provider extension is the closest model).
   Include it in your installer image (Image Factory or `imager`).
2. Add the provider entry from step 3 to the machine configuration:

   ```yaml
   machine:
     kubelet:
       credentialProviderConfig:
         apiVersion: kubelet.config.k8s.io/v1
         kind: CredentialProviderConfig
         providers:
           - name: harbor-bridge-plugin
             # … exactly as in step 3, with the CA inline …
   ```

3. Apply the machine configuration. Talos restarts kubelet with the flags.

This path is documented from the Talos documentation and is not covered by the
project's e2e tests.
