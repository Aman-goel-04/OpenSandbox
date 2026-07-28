# OpenTelemetry Metrics (Current Egress Support)

This page lists the OpenTelemetry metrics currently implemented in egress.

## Meter

- `opensandbox/egress`

## Metrics

| Metric | Type | Unit | Meaning |
|---|---|---|---|
| `egress.dns.query.duration` | Histogram | `s` | Upstream DNS forward latency (recorded for allowed queries). |
| `egress.dns.query.failed_total` | Counter | - | Queries the proxy could not resolve, by `reason`. |
| `egress.policy.denied_total` | Counter | - | Number of DNS queries denied by policy. |
| `egress.nftables.rules.count` | Observable Gauge | `{element}` | Approximate policy size after last successful static apply. |
| `egress.nftables.updates.count` | Counter | - | Number of successful nftables updates (static apply + dynamic IP add). |
| `egress.nftables.updates.failed_total` | Counter | - | nftables updates that failed, by `operation`. |
| `egress.system.memory.usage_bytes` | Observable Gauge | `By` | System memory used bytes (Linux: gopsutil; non-Linux build: `0`). |
| `egress.system.cpu.utilization` | Observable Gauge | `1` | CPU busy ratio in `[0,1]` (Linux: gopsutil; non-Linux build: `0`). |

## Failure Signals

`egress.dns.query.failed_total` and `egress.policy.denied_total` answer different
questions, and confusing them inverts the diagnosis:

- **denied** — the policy did its job. The workload asked for something it is not allowed
  to reach. Expected traffic in a working system.
- **failed** — the sidecar could not do its job. The workload asked for something allowed
  and got `SERVFAIL`. Never expected.

`reason` comes from a closed set, so the counter's cardinality is fixed and neither the
queried name nor the error text is ever attached:

| `reason` | Meaning |
|---|---|
| `no_upstreams` | No resolvers configured or discovered. |
| `upstream_error` | Every resolver failed to answer (network error, timeout). |
| `empty_response` | A resolver returned a nil message. |
| `rcode` | The last resolver answered with a failover-worthy rcode, e.g. `SERVFAIL`. |

`egress.nftables.updates.failed_total` covers the other silent failure. Its `operation`
attribute is one of `static_apply`, `dynamic_add` or `remove`; `dynamic_add` is the one to
alert on, because a failed add means the kernel never learned about IPs the policy allows,
so the chain drops traffic that should pass — which looks exactly like a policy denial from
inside the sandbox while `egress.policy.denied_total` stays flat.

## Shared Attributes

All egress metrics may include shared attributes:

- `sandbox_id` from `OPENSANDBOX_EGRESS_SANDBOX_ID` (when set)
- extra key/value attributes from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` (when set)

## OTEL Endpoint Configuration

Metric export is enabled only when at least one OTLP endpoint is set.

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (preferred)
- `OTEL_EXPORTER_OTLP_ENDPOINT` (fallback)

If both are unset, egress keeps metrics local (no OTLP export).

### Minimal Example

```bash
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://otel-collector:4318"
```

### Service Name

`service.name` is set by egress code as `opensandbox-egress-<version>`.

## Structured logs (JSON)

Egress structured logs are emitted by zap (typically to stdout). OTLP log export is not implemented in-tree.

### Common fields

- `sandbox_id` is included when `OPENSANDBOX_EGRESS_SANDBOX_ID` is set.
- key/value pairs from `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` are merged into the root logger.
- `opensandbox.event` identifies the event family.

### Outbound DNS logs

- `opensandbox.event=egress.outbound`
- emitted on allow-path DNS handling (success or forward error)
- common payload keys:
  - `target.host` (normalized query name)
  - `target.ips` (resolved A/AAAA addresses, when present)
  - `peer` (IP-only destination path)
  - `error` (forward failure message)

### Policy lifecycle logs

- `opensandbox.event=egress.loaded` (initial effective policy loaded)
- `opensandbox.event=egress.updated` (policy update applied)
- `opensandbox.event=egress.update_failed` (policy update failed)

Common policy fields:

- `egress.default` (`allow` / `deny`)
- `rules` (rule summary; for `egress.updated`, reflects current request body semantics)
- `error` (present for `egress.update_failed`)
