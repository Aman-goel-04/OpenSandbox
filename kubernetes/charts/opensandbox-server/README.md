# opensandbox-server Helm Chart

OpenSandbox Lifecycle API server: provides sandbox create/delete and other lifecycle APIs, typically used with BatchSandbox/Pool on Kubernetes.

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.0+
- OpenSandbox CRDs installed (deploy opensandbox-controller first)

## Install

```bash
# Server only (default namespace opensandbox-system)
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --namespace opensandbox-system \
  --create-namespace

# With custom image and config
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --set server.image.repository=your-registry/opensandbox/server \
  --set server.image.tag=v0.1.0 \
  --namespace opensandbox-system \
  --create-namespace
```

### Deploy server and ingress-gateway together

To run both the Lifecycle API server and the ingress gateway (components/ingress) in one release, set `server.gateway.enabled=true`. The chart will deploy the server and the gateway (Deployment, Service, RBAC), and write server config `[ingress] mode = "gateway"` so the server returns the correct gateway address to clients.

```bash
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --namespace opensandbox-system \
  --create-namespace \
  --set server.gateway.enabled=true \
  --set server.gateway.host=gateway.example.com
```

Optional: override gateway image, replicas, or resources (see `server.gateway.*` in Configuration).

### OSEP-0011 secure-access keys

To enable signed, expiring sandbox routes, provide the signing keys either
inline (plaintext in values — fine for local dev only):

```bash
--set server.gateway.secureAccess.activeKey=a \
--set 'server.gateway.secureAccess.keys[0].key_id=a' \
--set 'server.gateway.secureAccess.keys[0].key=<base64-secret>'
```

or from an existing Secret (`server.gateway.secureAccess.existingSecret`) with
two data entries: `keys` (`a=<base64-secret>[,b=...]`) and `active-key` (`a`).
The chart delivers the Secret to the server and gateway containers as
environment variables, so key material stays out of values, the server
ConfigMap, and pod args. The two forms are mutually exclusive.

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `server.image.repository` | Server image repository | `sandbox-registry.../opensandbox/server` |
| `server.image.tag` | Server image tag | Chart `appVersion` |
| `server.replicaCount` | Server replicas | `2` |
| `server.resources` | CPU/memory requests and limits | See values.yaml |
| `namespaceOverride` | Deployment namespace | `opensandbox-system` |
| `configToml` | config.toml content ([ingress] block generated from server.gateway) | See values.yaml |
| `server.gateway.enabled` | When true: set server config to gateway and deploy components/ingress gateway | `false` |
| `server.gateway.host` | config `gateway.address` (address returned to clients) | `opensandbox.example.com` |
| `server.gateway.gatewayRouteMode` | server config and gateway route mode (header/uri) | `header` |
| `server.gateway.env` | Additional environment variables for the ingress-gateway container (e.g. `OTEL_EXPORTER_OTLP_ENDPOINT`) | `[]` |
| `server.gateway.secureAccess.keys` | OSEP-0011 signing key ring, plaintext in values | `[]` |
| `server.gateway.secureAccess.existingSecret` | Name of a Secret holding `keys` + `active-key`; alternative to plaintext `keys` | `""` |
| `server.gateway.*` | Gateway image, replicas, port, dataplaneNamespace, providerType, resources | See values.yaml |

Versioning note:

- `server.image.tag` defaults to the chart `appVersion`.
- The chart package `version` and the image/app `appVersion` are intentionally
  separate. A server release branch or tag does not automatically imply a new
  Helm chart package version.
- If you want the chart to deploy a specific server release, override
  `server.image.tag` explicitly or consume a Helm package release whose chart
  version was published for that purpose.

**Gateway**: When `server.gateway.enabled=true`, the chart writes `[ingress] mode = "gateway"` in config.toml and deploys **components/ingress** Deployment/Service/RBAC; gateway `--mode` matches config. External access must be configured separately.

Set `[kubernetes].namespace` in config for the sandbox workload namespace. Override `api_key` via Secret or values in production.

## Upgrade and uninstall

```bash
helm upgrade opensandbox-server ./kubernetes/charts/opensandbox-server -n opensandbox-system
helm uninstall opensandbox-server -n opensandbox-system
```

## References

- [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
- [Helm deployment docs](../../docs/HELM-DEPLOYMENT.md)
