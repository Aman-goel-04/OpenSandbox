---
title: Sandbox Fleets Runtime (fast-sandbox backend)
authors:
  - "@fengcone"
  - "@Pangjiping"
creation-date: 2026-02-08
last-updated: 2026-07-27
status: provisional
---
# OSEP-0007: Sandbox Fleets Runtime (fast-sandbox backend)

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Why Fast-Sandbox is Fast](#why-fast-sandbox-is-fast)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [The `fleets` runtime type and API reuse model](#the-fleets-runtime-type-and-api-reuse-model)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [How Fast-Sandbox Reduces Creation-Path Overhead](#how-fast-sandbox-reduces-creation-path-overhead)
  - [Kubernetes Ecosystem Integration](#kubernetes-ecosystem-integration)
- [Integration Conditions & Feasibility](#integration-conditions--feasibility)
  - [Lifecycle](#lifecycle-integration)
  - [execd Injection](#execd-injection)
  - [Ingress / Endpoint Access](#ingress--endpoint-access)
  - [Egress / Network Policy](#egress--network-policy)
- [Construction Phases](#construction-phases)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)

<!-- /toc -->

## Summary

Introduce a new OpenSandbox backend type, **`sandbox fleets`** (runtime `type = "fleets"`), whose sole implementation is backed by [fast-sandbox](https://github.com/opensandbox-group/fast-sandbox). A fleet runs many sandboxes as isolated runtimes (container / gVisor / Kata) inside pre-warmed **Fastlet** pods, reached through fast-sandbox's gRPC Fast-Path control plane and its authenticated proxy data plane. The architecture removes the per-sandbox K8s scheduler, watch-propagation, and kubelet path for latency-sensitive workloads; any cross-backend latency claim still requires a reproducible OpenSandbox end-to-end benchmark.

`fleets` is **additive and parallel** to the existing `docker` and `kubernetes` backends; it does **not** replace the pod-per-sandbox `kubernetes` backend. The integration is deliberately scoped:

- **Create** is a *simplified* subset of `CreateSandboxRequest`. Pod-identity-dependent fields (`volumes`, `platform` node-selectors, `resource_requests`, `credential_proxy`, `snapshot_id`, pause/resume) are explicitly rejected; `network_policy` and `secure_access` are staged (contract kept, enforced in phase 1b).
- **Lifecycle** (get / delete / renew-expiration / list / metadata), **execd** (exec / file), and **egress** (`network_policy`) reuse the *existing public API contracts* unchanged, so upstream SDKs are unaffected.

**Current performance evidence**:

- At fast-sandbox [`3af0222`](https://github.com/opensandbox-group/fast-sandbox/tree/3af02227a85d3ae872e67ef718b63c60e777edac), the repository explicitly has **no release-grade Sandbox Create benchmark**. Its [dated engineering baseline](https://github.com/opensandbox-group/fast-sandbox/blob/3af02227a85d3ae872e67ef718b63c60e777edac/docs/guides/performance.md) measured 20 concurrency-1, warm-image runc creates through `RuntimeReady`: mean **76.02 ms**, p50 **75.95 ms**, and p95 **83.15 ms**.
- That baseline excludes Infra readiness, route publication, `DataPlaneReady`, and the OpenSandbox server/gateway path. It is evidence about the current fast-sandbox implementation, not a `fleets` release target or a comparison with the BatchSandbox pool.

> **Correction from earlier drafts**: fast-sandbox does **not** implement a "Fast Mode" (container-first / async-CRD / eventual-consistency) path. Every create ranks in-memory candidates, persists one Sandbox CRD, and only then performs atomic Fastlet admission/runtime creation. Here, **CRD-first** means durable intent precedes runtime creation; it does not mean the CRD write precedes candidate ranking. The gRPC entry avoids the K8s *scheduler* and *watch propagation*, not the CRD/etcd write. This OSEP also uses the real fast-sandbox terminology (**Fastlet** / **SandboxPool**), not the "Agent" / "AgentPool" terms from earlier drafts.

> **Note**: The observations above assume the container image and runtime artifacts are already cached on the Fastlet's host node. Cache misses add pull/unpack work and must be reported separately.

## Motivation

OpenSandbox currently supports Docker and Kubernetes runtimes. The Kubernetes runtime provides scalability, but its per-sandbox path can include an API write, scheduler and watch propagation, kubelet reconciliation, container runtime startup, and an image pull on a cache miss. Their costs vary materially by cluster and workload, so this OSEP does not assign them universal latency values.

### OpenSandbox's Existing Pool Optimization

OpenSandbox's Kubernetes runtime already supports a **pool-based optimization** via the `poolRef` field in BatchSandbox CRD. When `poolRef` is specified:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: my-sandbox
spec:
  poolRef: my-pool              # Reference to pre-warmed pool
  taskTemplate:
    spec:
      process:
        command: ["python", "app.py"]
```

**How it works**:

- Users create a pool of pre-provisioned pods (managed by BatchSandbox controller)
- When creating a sandbox, OpenSandbox assigns a task from the pool
- Only `entrypoint` and `env` are customizable; image and resources are pre-defined
- Controller and OpenSandbox Server watch K8s API for state changes

**Performance with pool**:

- Eliminates scheduler wait and pod startup time
- Still requires K8s API write + watch propagation overhead
- Image must be pre-pulled in pool pods
- No reproducible report currently establishes a portable allocation-latency number; the comparison benchmark in this OSEP must measure both backends under the same environment and readiness boundary

This is an effective optimization for many use cases. However, fast-sandbox aims to push latency even lower through additional innovations described below.

For AI Agent and Serverless scenarios that require rapid sandbox provisioning, removing scheduler/watch/kubelet work from the per-sandbox hot path is valuable even though fast-sandbox retains a synchronous CRD write.

### Why Fast-Sandbox is Fast

fast-sandbox reduces creation-path overhead through three key design choices:

**Comparison: OpenSandbox Pool vs fast-sandbox**


| Aspect                          | OpenSandbox BatchSandbox Pool                      | fast-sandbox (fleets)                       |
| ------------------------------- |----------------------------------------------------|---------------------------------------------|
| **Allocation mechanism**        | K8s API write → Controller watch → Task assignment | gRPC → in-memory Top-K → CRD write → Fastlet admission |
| **Latency (with cached image)** | No comparable release-grade report                 | No comparable release-grade report          |
| **Scheduling**                  | K8s Scheduler places pool pods (one-time)          | In-memory Top-K registry with image affinity |
| **Image awareness**             | Pool pods have fixed image                         | Registry ranks by image cache availability  |
| **Customization**               | entrypoint, env only                               | entrypoint, env, image per request          |
| **Container creation**          | pre-warmed                                         | Direct containerd socket inside Fastlet     |
| **Consistency**                 | Durable K8s state is the source of truth            | Sandbox CRD is persisted synchronously before runtime creation |
| **Failure recovery**            | K8s Controller reconciliation                      | NodeJanitor + AutoRecreate policy           |

Both approaches use pre-provisioned resource pools to eliminate cold start overhead. fast-sandbox's key advantage is bypassing the K8s **scheduler and watch propagation** for container placement while still committing durable intent through a CRD write.

#### 1. gRPC Fast-Path Allocation, Bypassing the K8s Scheduler

Traditional K8s sandbox creation follows this control flow:

```
Client → K8s API Server → etcd → Scheduler → etcd → Kubelet → Container Runtime
```

fast-sandbox uses a gRPC Fast-Path that is **CRD-first for every create** — it does not bypass etcd, but it bypasses the scheduler queue and watch propagation:

```
Client → gRPC Fast-Path → in-memory Top-K candidate ranking
       → K8s API (Sandbox CRD write, IO 1)
       → atomic Fastlet admission/create (IO 2) → containerd
```

**With uncached image**: additional image pull time applies.

The Fast-Path Server maintains an **in-memory registry** for placement, eliminating:

- scheduler queue wait time
- watch propagation delays

It does **not** eliminate the CRD/etcd write — that write (IO 1) is on the synchronous happy path and precedes the Fastlet create (IO 2). The durable CRD is the source of truth; there is no eventual-consistency "fast mode".

#### 2. In-Memory Top-K Scheduling with Image Affinity

fast-sandbox's registry ranks candidate Fastlets (it does not use a single additive score). Ordering, in priority:

```
1. image-cache hit (Fastlets with the image cached rank first)
2. lower normalized load (used / capacity)
3. stable hash tiebreak (request stable key + Fastlet ID)
```

Key characteristics:

- **In-memory placement**: No disk I/O, no database queries
- **Image affinity**: Prioritizes Fastlets with cached images
- **Atomic admission**: The selected Fastlet is the authority that consumes a capacity slot; the registry ranking is advisory, and Fastlet atomic admission is final
- **Top-K with retry**: The Fast-Path picks the top candidate and can retry the next candidate on rejection

This is fundamentally different from the K8s scheduler which:

- Runs as a separate process with IPC overhead
- Doesn't track image cache state
- Schedules pods without considering image availability

#### 3. Kubernetes Ecosystem Reuse with Direct Containerd Access

fast-sandbox achieves speed while maintaining K8s compatibility:


| Aspect                     | fast-sandbox Approach                                          | K8s Benefit                               |
| -------------------------- | -------------------------------------------------------------- | ----------------------------------------- |
| **Resource Accounting**    | Fastlet Pods tracked in K8s                                    | Resource visibility via`kubectl get pods` |
| **Scheduling Constraints** | Node selectors, taints, tolerations on the Fastlet Pod         | K8s scheduler places Fastlet Pods optimally |
| **Container Creation**     | Direct containerd socket access (bypasses kubelet)             | Removes kubelet from the per-sandbox path |
| **Security Containers**    | Supports gVisor/Kata Containers via containerd runtime handler | Same workflow, different runtime class    |
| **Network Namespace**      | Each sandbox gets its own netns + private IP inside the Fastlet Pod | K8s CNI plugins carry the Fastlet Pod's traffic |

The key insight: **use K8s for what it's good at** (resource accounting, cluster management, scheduling constraints at the Fastlet-pool granularity), but **bypass the K8s scheduler for the hot path** (container placement + creation).

### Goals

- Add a `fleets` runtime type (`config.runtime.type = "fleets"`) implemented as a new `FleetSandboxService` (a `SandboxService`, **not** a Kubernetes `WorkloadProvider`)
- Reuse the existing lifecycle API (`get` / `delete` / `renew-expiration` / `list` / `metadata`) with no changes to routes or SDKs
- Reuse the existing execd exec/file access pattern (`get_endpoint(id, 44772)` → in-sandbox execd HTTP) unchanged
- Reuse the existing egress contract (`network_policy` at create; `egress-api.yaml` `/policy` at runtime), and **enforce per-sandbox egress** on fast-sandbox
- Provide a simplified Create that maps a well-defined subset of `CreateSandboxRequest` to fast-sandbox's gRPC `CreateSandbox`, and cleanly rejects unsupported fields
- Demonstrate lower p50 and p95 user-visible creation latency than the `kubernetes` pool backend, measured from SDK create start until `Running` and the execd endpoint are usable under the same environment, cache state, workload, and concurrency; this OSEP sets no universal absolute-millisecond threshold
- Provide flexible deployment: users can bring their own fast-sandbox or use OpenSandbox-provided charts

### Non-Goals

- Replacing or removing the existing Docker or Kubernetes runtimes
- Supporting `volumes` (PVC / host / ossfs), `platform` node-selectors, `resource_requests`, `credential_proxy`, or `snapshot_id` on `fleets`
- Supporting `pause` / `resume` / snapshot on `fleets` (fast-sandbox states these as explicit non-goals)
- Implementing `fleets` as a Kubernetes `WorkloadProvider`
- Implementing a full Kubernetes operator for fast-sandbox (it has its own controller)
- Changing the OpenSandbox sandbox lifecycle API or SDKs in a breaking way
- Direct management of fast-sandbox `Sandbox` / `SandboxPool` CRDs or Fastlet pods (owned by the fast-sandbox controller)

## Requirements

- Must register as a new `SandboxService` under `config.runtime.type = "fleets"`; must not modify the `WorkloadProvider` contract
- Must not change the public lifecycle, execd, or egress API contracts or the SDKs
- Simplified Create must reject unsupported fields with a clear, actionable error rather than silently ignoring them
- Must resolve `get_endpoint(id, port)` for at least ports 44772 (execd) and 18080 (egress policy) and arbitrary user ports, via the ingress gateway
- Per-sandbox egress must be enforced (not advisory) once `network_policy` is accepted; see [Construction Phases](#construction-phases) for the staged rollout within the first release
- Must handle status mapping between fast-sandbox and OpenSandbox states
- Must preserve tenant isolation: when `[tenants]` is configured, every namespaced Fast-Path call (create/get/list/update/delete/endpoint) must resolve the current tenant to a fast-sandbox namespace; otherwise `fleets` must **reject** tenant configuration (a shared/default namespace would leak other tenants' sandboxes through the namespace-only `ListSandboxes` and ID-based operations)
- gRPC reachability from the OpenSandbox Server to the fast-sandbox Fast-Path Server is required

## Proposal

Introduce a new backend type **`sandbox fleets`**, implemented as a `FleetSandboxService` that communicates with the fast-sandbox Fast-Path Server via the gRPC Fast-Path API. It is selected by `config.runtime.type = "fleets"` and registered alongside `docker` and `kubernetes` in `server/opensandbox_server/services/factory.py`.

> **Why a new backend type, not a new `WorkloadProvider`**: OpenSandbox has two abstraction layers — the top-level `SandboxService` (selected by `config.runtime.type`; the seam for `docker` / `kubernetes`) and the Kubernetes-internal `WorkloadProvider` (`batchsandbox` / `agent-sandbox`). The `WorkloadProvider` ABC is saturated with K8s semantics (namespace, CR metadata, pod-spec mutation) and cannot host a separate gRPC control plane. fast-sandbox is therefore a new `SandboxService`. This choice is what lets the lifecycle routes, exec/file access, and egress access patterns be reused unchanged, because all of them funnel through the `SandboxService` ABC and the `get_endpoint` + in-sandbox HTTP contracts rather than through pod semantics.

**Architecture Overview**:

```
+-------------------------------------------------------------------------+
|                        OpenSandbox Control Plane                        |
+-------------------------------------------------------------------------+
|                                                                         |
|   lifecycle routes ---> SandboxService (ABC)                            |
|                              |                                          |
|                     FleetSandboxService                                 |
|                     |                    |                              |
|         gRPC FastPathService (9090)   get_endpoint()                    |
|                     |                    | (via ingress gateway)        |
|                     v                    v                              |
|   +-----------------------+     +----------------------+                |
|   | fast-sandbox          |     | ingress gateway      |                |
|   | Fast-Path Server      |     | (sandbox_id + port)  |                |
|   +----------+------------+     +----------+-----------+                |
|              |                             |                            |
|              | CRD-first + in-memory       | Sandbox Proxy /            |
|              | Top-K placement             | Fastlet Proxy              |
|              v                             v                            |
|   +---------------------------------------------------+                 |
|   | Fastlet Pod (K8s Managed)                         |                 |
|   |   Fastlet control + Fastlet Proxy sidecar         |                 |
|   |   many sandbox runtimes via direct containerd     |                 |
|   |   each: own netns + private IP; execd :44772      |                 |
|   +---------------------------------------------------+                 |
|                                                                         |
+-------------------------------------------------------------------------+
                                ^
                                | K8s API Server (Fastlet Pod mgmt + Sandbox CRD)
                                |
+-------------------------------------------------------------------------+
|                    Kubernetes Control Plane (CRD path)                  |
|  - Fastlet Pod lifecycle (create/monitor/delete)                        |
|  - Sandbox / SandboxPool CRDs (durable intent, reconciliation, audit)   |
|  - Resource accounting (visible in kubectl); scheduling constraints     |
+-------------------------------------------------------------------------+
```

**Data Flow Comparison** (assuming cached image):

```
Standard K8s Runtime:
OpenSandbox Server → K8s API → etcd → Scheduler → etcd → Kubelet → containerd

Sandbox Fleets (fast-sandbox, CRD-first — the only path):
OpenSandbox Server → gRPC Fast-Path → in-memory Top-K → K8s API (CRD write)
                   → atomic Fastlet admission → containerd
      (scheduler + watch propagation bypassed; latency must be measured end-to-end)
```

### The `fleets` runtime type and API reuse model

The three "reused" API areas reuse *different* things. Being precise here avoids the biggest integration trap: **there is no server-side exec or egress API endpoint.** exec/file and egress runtime control are HTTP contracts that clients speak *directly to components inside the sandbox*; the server's only role is `get_endpoint`.

| Area | What is reused | What must be built for `fleets` |
| --- | --- | --- |
| Lifecycle (get/delete/renew/list/metadata) | Public routes + `SandboxService` ABC remain unchanged | Map each method to fast-sandbox gRPC and add the missing read/delete semantics described below |
| execd exec/file | `specs/execd-api.yaml` (client → in-sandbox execd:44772) is backend-agnostic | Run execd inside the sandbox; implement `get_endpoint(id, 44772)` |
| egress `network_policy` | `NetworkPolicy` schema + `egress-api.yaml` `/policy` (client → egress proxy:18080) | Deliver + enforce policy per sandbox netns; implement `get_endpoint(id, 18080)` with auth header |
| Endpoint resolution | `SandboxService.get_endpoint` → `Endpoint`; ingress-gateway routing is backend-neutral | Route fleets ports through the ingress gateway |

**Simplified Create.** The `fleets` Create is a subset of the full `CreateSandboxRequest`. The table below classifies every field as **kept** (mapped through), **downgraded** (accepted but semantics change), **staged** (contract kept, enforced in a later phase), or **rejected** (HTTP 400 with a clear message). The common thread among rejected fields is that they assume *1 sandbox = 1 dedicated K8s Pod* (own rootfs, node, volume set, netns), which does not hold in the shared-Fastlet model.

| `CreateSandboxRequest` field | fleets | Mapping / reason |
| --- | --- | --- |
| `image.uri` | **kept** | → `CreateRequest.image` |
| `entrypoint` | **kept** | → `command` (+ `args`) |
| `env` | **kept** | → `envs` |
| `timeout` | **kept** (see note) | → `expire_time_seconds`. `CreateRequest` has no expiry field today, so it is set via a follow-up `UpdateSandbox`; a failed update must not leave an immortal sandbox — the create **rolls back** (deletes the sandbox) on update failure. Target: add `expire_time_seconds` to `CreateRequest` (proto, ask-first) to make it atomic |
| `metadata` | **kept** (see note) | → CRD user labels. `CreateRequest` has no labels field today, so initial labels share the post-create `UpdateSandbox` call and rollback rule; PATCH deletion requires an additive delete operation (see Lifecycle) |
| `extensions.pool_ref` | **kept** | → `pool_ref` (else config default) |
| `resource_limits` | **downgraded** | fast-sandbox enforces the pool's immutable `sandboxResources`; the request value is validated for pool compatibility, not applied per-sandbox |
| `network_policy` | **staged (1b)** | Contract kept; rejected in phase 1a, enforced per-slot netns in phase 1b (see [Egress / Network Policy](#egress--network-policy)) |
| `secure_access` | **staged (1b)** | Naturally aligned: fast-sandbox already returns `required_headers` with a short-lived Ed25519 bearer per `ResolveEndpoint`. Server-issued access headers are layered on the gateway route by the fleets ingress adapter; deferred to 1b (see [Ingress / Endpoint Access](#ingress--endpoint-access)) |
| `image.auth` | **rejected** | Private-registry credentials are not carried to fast-sandbox; an authenticated image is rejected rather than attempting an unauthenticated pull (future: map to a pool-level imagePullSecret) |
| `snapshot_id` | **rejected** | No snapshot capability in fast-sandbox (explicit non-goal) |
| `platform` | **rejected** | No per-sandbox node scheduling; scheduling is per Fastlet pool |
| `resource_requests` | **rejected** | No K8s requests / Burstable QoS; resources are fixed by the pool profile |
| `credential_proxy` | **rejected (all phases)** | Rides the per-pod egress mitmproxy sidecar, which has no place in the shared-Fastlet model. Rejected even after 1b accepts `network_policy`, so it is never silently ignored |
| `volumes` | **rejected** | Fastlet child containers cannot receive dynamic PVC/CSI mounts |

In short, `fleets` Create keeps the "**what to run**" fields (image / entrypoint / env) plus "**which pool, how long, what tags**" (pool_ref / timeout / metadata), and drops or stages the pod-level isolation / storage / snapshot / signed-network fields.

### Notes/Constraints/Caveats

- The fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy) and Fastlet pools + NodeJanitor must be deployed separately (by the user or via OpenSandbox-provided Helm charts)
- fast-sandbox uses its own CRD types (`Sandbox`, `SandboxPool`, group `sandbox.fast.io/v1alpha1`) - OpenSandbox does not manipulate these directly; missing lifecycle semantics are addressed through additive Fast-Path fields/operations rather than coupling the server to CRD internals
- gRPC communication requires network reachability from OpenSandbox Server to the fast-sandbox Fast-Path Server
- execd is injected via fast-sandbox's **Infra Component** mechanism (see [execd Injection](#execd-injection)), not the K8s init-container copy used by the pod backend
- Because all sandboxes in a Fastlet pod share that pod's K8s network identity (SNAT to one pod IP), **standard Kubernetes NetworkPolicy cannot express per-sandbox egress** for fleets; per-sandbox egress is enforced inside each sandbox's netns (see [Egress / Network Policy](#egress--network-policy))
- **Tenant isolation** relies on fast-sandbox namespaces: `ListSandboxes` is namespace-only, so a shared namespace would expose tenants to each other. `FleetSandboxService` must map each OpenSandbox tenant to a distinct namespace on every call, or reject `[tenants]` configuration outright (phase 1a)

### Risks and Mitigations


| Risk                                                                     | Mitigation                                                                                                                         |
| ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| Fast-Path Server becomes a single point of failure                       | Fast-Path Servers are multi-active; `FleetSandboxService` retries; optionally fall through to the declarative CRD create path      |
| gRPC API changes in fast-sandbox could break integration                 | Version pinning in deployment; compatibility matrix documentation                                                                  |
| Network partition between OpenSandbox Server and fast-sandbox Fast-Path  | Configurable timeouts; health check endpoint integration                                                                           |
| State drift if sandboxes are managed outside OpenSandbox                 | OpenSandbox tracks sandbox IDs; periodic state reconciliation via gRPC GetSandbox                                                  |
| "Reusing egress" misread as reusing a server API, underestimating cost  | This OSEP states explicitly that egress enforcement is bespoke; only the HTTP contract + endpoint/token plumbing are reused        |
| Carrying `network_policy` needs a proto/CRD change on fast-sandbox       | Additive, backward-compatible field; gated behind an "ask first" review with fast-sandbox maintainers                             |
| gVisor/Kata do not honor host-netns egress rules                         | Phase 1 restricts `network_policy` to the `container` (runc) runtime; reject egress on gVisor/Kata                                 |
| Orphaned sandboxes on Fastlet/node loss                                  | fast-sandbox NodeJanitor + `AutoRecreate` failure policy (note: not state-preserving)                                             |
| Users expect volumes/snapshot on fleets                                  | Simplified Create rejects them with a clear message pointing to the `kubernetes` backend                                          |

## Design Details

### How Fast-Sandbox Reduces Creation-Path Overhead

The fast-sandbox architecture is built around three performance-critical design choices:

#### 1. Bypassing the K8s Scheduler for the Hot Path (CRD write retained)

```
┌──────────────────────────────────────────────────────────────────────────┐
│              CRD-first Creation Flow (image cached, happy path)           │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          |
│  Prerequisite: Image is cached on the Fastlet's host node (containerd)   │
│                                                                          |
│  1. OpenSandbox Server → gRPC CreateSandbox request                      │
│                                                                          |
│  2. In-memory Top-K placement (registry-only, no K8s API)                │
│     • Filter by pool, namespace, runtime/profile, capacity               │
│     • Rank by: image-hit, then used/capacity, then stable hash           │
│                                                                          |
│  3. IO 1: Sandbox CRD write to K8s API / etcd                            │
│     • Durable intent + idempotency by request_id                         │
│                                                                          |
│  4. IO 2: atomic Fastlet admission → runtime create/start (cached image) │
│     • Direct socket access to host containerd                            │
│     • No image pull (cached); sandbox gets its own netns + private IP    │
│                                                                          |
│  5. Fast-Path returns {sandbox_uid, sandbox_name, fastlet_pod}           │
│     • Returns at RuntimeReady; endpoints resolved later via ResolveEndpoint │
│                                                                          |
│  Measure: client-observed Create through RuntimeReady                    │
│  Do not infer this total by adding estimates from different runs.        │
│                                                                          |
│  If image is NOT cached: image pull time is added to step 4              │
└──────────────────────────────────────────────────────────────────────────┘
```

The difference is steps 2-4 of the K8s path (scheduler queue + watch propagation + kubelet). fast-sandbox keeps the etcd write (as IO 1) but replaces the scheduler/watch/kubelet steps with in-memory placement + a direct Fastlet create.

The current public engineering baseline found that warm runc runtime work, not candidate ranking, dominated the tested path: mean RuntimeDriver work was 67.76 ms of a 76.02 ms client-observed Create, including 21.95 ms in `NewContainer`, 36.87 ms in `NewTask`, and 7.89 ms in `Start`. These are nested observations from one dated environment, not budgets for this diagram.

#### 2. Registry Top-K Ranking

The registry does not compute a single additive score; it hard-filters candidates then sorts them. Simplified from the real `TopK` in `internal/controlplane/placement/registry.go`:

```go
// Hard filter: namespace, pool, readiness, capacity, runtime/profile match.
// Then sort the survivors:
sort(candidates, func(a, b) bool {
    if a.imageHit != b.imageHit {
        return a.imageHit          // image-cache hit ranks first
    }
    if a.used*b.capacity != b.used*a.capacity {
        return a.used*b.capacity < b.used*a.capacity   // lower normalized load
    }
    return stableHash(reqKey, a.id) < stableHash(reqKey, b.id)  // stable tiebreak
})
// Return top K; Fastlet atomic admission is the final authority on capacity.
```

The repository currently provides `BenchmarkRegistryTopK1000` for same-machine regression comparisons, but publishes no raw result that supports a portable `100 Fastlets` or `1000 Fastlets` latency claim. This microbenchmark excludes Kubernetes, Fastlet admission, runtime/network creation, Infra readiness, and routing; it must not be presented as Sandbox Create latency.

#### 3. Direct Containerd Integration

Fastlet Pods run with access to the host containerd socket and create sandbox containers directly:

```go
// fast-sandbox internal/runtime/containerd/driver.go (illustrative)

client, _ := containerd.New("/run/containerd/containerd.sock",
    containerd.WithDefaultNamespace("k8s.io"))

// Direct container creation - bypasses kubelet entirely
container, _ := client.NewContainer(
    ctx, sandboxID,
    containerd.WithImage(image),                 // Already cached
    containerd.WithNewSnapshot(...),             // Snapshot setup still runs with a cached image
    // runc by default; "io.containerd.runsc.v1" (gVisor) or Kata shim per pool runtime
    oci.WithLinuxNamespace(networkNamespace),    // sandbox's own netns
)

task, _ := container.NewTask(ctx, cio.NewCreator(...))
task.Start(ctx)
```

This approach:

- Eliminates kubelet reconciliation from the per-sandbox creation path
- Enables image cache reuse (the Fastlet Pod shares the node's containerd image store)
- Supports alternative runtimes (gVisor via `runsc`, Kata) via the pool's immutable runtime handler

### Kubernetes Ecosystem Integration

Despite bypassing the K8s scheduler for the hot path, fast-sandbox maintains full compatibility:

#### Resource Accounting via K8s Pods

Fastlet Pods are normal K8s Pods:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fast-sandbox-fastlet-node-1
  labels:
    app: fast-sandbox-fastlet
    pool-ref: default-pool
spec:
  containers:
  - name: fastlet
    image: fast-sandbox/fastlet:latest
    resources:
      requests:
        cpu: "2000m"
        memory: "4Gi"
      limits:
        cpu: "4000m"
        memory: "8Gi"
    volumeMounts:
    - name: containerd-socket
      mountPath: /run/containerd/containerd.sock
  volumes:
  - name: containerd-socket
    hostPath:
      path: /run/containerd/containerd.sock
```

These Pods are visible in `kubectl get pods` and count against:

- Node resource allocation (visible to cluster autoscaler)
- Resource quotas (namespace limits enforced)
- Scheduler decisions (node affinity, taints, tolerations)

#### CRD for Reconciliation and Auditing

fast-sandbox defines two CRDs:

```yaml
# SandboxPool - manages Fastlet Pod lifecycle (fields illustrative; see fast-sandbox CRD)
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: default-pool
  namespace: default
spec:
  capacity:
    poolMin: 2
    poolMax: 10
    bufferMin: 1
    bufferMax: 3
  maxSandboxesPerPod: 5
  runtime: container               # immutable: container | gvisor | kata-qemu | kata-clh | ...
  sandboxResources:                # immutable per-sandbox profile, enforced by Fastlet
    cpu: "500m"
    memory: "512Mi"
    pids: 256
  infraProfile: opensandbox-execd-quickstart   # injects execd on 44772
  warmImages:                      # asynchronously pre-pulled, protected from cache GC
    - python:3.11
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:latest
        volumeMounts:
        - name: containerd-socket
          mountPath: /run/containerd/containerd.sock
      volumes:
      - name: containerd-socket
        hostPath:
          path: /run/containerd/containerd.sock

---
# Sandbox - durable intent + audit trail (created by the Fast-Path or declaratively)
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-sandbox                 # = request_id (OpenSandbox sandbox_id)
  namespace: default
  labels:
    sandbox.fast.io/created-by: fastpath
spec:
  image: python:3.11
  poolRef: default-pool
  command: ["python", "-m", "http.server", "8000"]
  failurePolicy: AutoRecreate         # or "Manual"
  recoveryTimeoutSeconds: 60
status:
  runtimeState: Ready               # ObservedState: Pending/Creating/Ready/Draining/Stopped/Failed/Unavailable
  dataPlaneState: Ready
  assignment:
    fastletName: fast-sandbox-fastlet-node-1
    fastletPodUID: ...
    nodeName: node-1
```

Note: the `Sandbox` CRD carries **no `exposedPorts` field and no inline endpoints** — endpoints are resolved on demand via `ResolveEndpoint`, which returns an authenticated proxy route. There is a single `created-by: fastpath` label (no fast/strong variants).

These CRDs serve as:

- **Durable intent + audit trail**: the CRD is the source of truth; the Fast-Path writes it first (IO 1)
- **Self-healing**: leader-elected Reconcilers converge state and clean up orphaned sandboxes
- **Observability**: Standard K8s tools (kubectl, metrics-server) work

#### Security Container Support

fast-sandbox supports gVisor/Kata Containers via the pool's immutable runtime handler:

```
container   → io.containerd.runc.v2
gvisor      → io.containerd.runsc.v1
kata-qemu   → containerd Kata shim (QEMU)
kata-clh    → containerd Kata shim (Cloud Hypervisor)
```

The runtime is a `SandboxPool` field (immutable per pool), so OpenSandbox selects isolation level by targeting a pool, without changing the integration layer.

> **Caveat for egress**: gVisor and Kata run their network stack in a user-space kernel / guest VM, so host-netns iptables rules do not reliably filter their egress. Phase 1 restricts per-sandbox `network_policy` to the `container` (runc) runtime (see [Egress / Network Policy](#egress--network-policy)).

#### NodeJanitor: Orphan Cleanup

Because a sandbox is bound to one Fastlet Pod, orphaned containerd resources can arise if:
- The Fastlet Pod is unexpectedly deleted (crash, node drain, eviction)
- The `Sandbox` CRD is deleted while a container still exists
- A CRD is recreated with a new UID (UID mismatch)

fast-sandbox provides a **NodeJanitor DaemonSet** on each node that performs fenced cleanup a lost Fastlet can no longer do.

**How NodeJanitor detects orphans:**

| Orphan Type | Detection Method | Cleanup Trigger |
|-------------|-------------------|-----------------|
| Fastlet Pod disappeared | Pod UID not found in K8s API | After orphan timeout |
| Sandbox CRD deleted | CRD not found | After orphan timeout |
| UID mismatch (recreated CRD) | Container label ≠ CRD UID | After orphan timeout |

**Scan process (per node):** enumerate fast-sandbox-managed containerd resources from durable per-slot state, perform a fresh Kubernetes ownership check and an orphan-age check, and only then tear down the task/container, snapshot, network namespace, and any Infra state.

**Note for the egress work (phase 1b):** in-netns rules are reaped automatically when the slot's netns is deleted, so they need no janitor change; any out-of-netns state (e.g. a host-side DNS proxy or ipset) would require extending NodeJanitor.

### Configuration Extension

Add `FleetsRuntimeConfig` to `server/opensandbox_server/config.py`:

```python
class FleetsRuntimeConfig(BaseModel):
    """sandbox fleets (fast-sandbox) runtime configuration."""

    fastpath_endpoint: str = Field(
        default="fast-sandbox-fastpath.opensandbox.svc:9090",
        description="fast-sandbox Fast-Path Server gRPC endpoint.",
    )
    default_pool_ref: str = Field(
        default="default-pool",
        description="Default SandboxPool when extensions.pool_ref is unset.",
    )
    execd_port: int = Field(default=44772, description="execd port inside the sandbox.")
    egress_port: int = Field(default=18080, description="egress policy port inside the sandbox.")
    require_ingress_gateway: bool = Field(
        default=True,
        description="fleets resolves endpoints via the ingress gateway only.",
    )
```

Update `AppConfig` to include the new config block and validation logic.

### TOML Configuration Example

```toml
[server]
host = "0.0.0.0"
port = 8080
api_key = "your-secret-key"

[runtime]
type = "fleets"
execd_image = "opensandbox/execd:v1.0.21"

[fleets]
fastpath_endpoint = "fast-sandbox-fastpath.opensandbox.svc:9090"
default_pool_ref = "default-pool"
execd_port = 44772
egress_port = 18080
require_ingress_gateway = true
```

### New Code Structure

```
server/opensandbox_server/services/fleets/
├── __init__.py
├── fleet_service.py         # New: FleetSandboxService(SandboxService)
├── fastpath_client.py       # New: gRPC client wrapper for fast-sandbox FastPathService
├── create_mapping.py        # New: CreateSandboxRequest subset → CreateRequest; field rejection
└── status_mapping.py        # New: fast-sandbox Sandbox status → OpenSandbox states
# Modified: server/opensandbox_server/services/factory.py (register "fleets")
```

### API Mapping


| OpenSandbox API (`SandboxService`)      | fast-sandbox gRPC                    | Notes                                          |
| --------------------------------------- | ------------------------------------ | ---------------------------------------------- |
| `POST /sandboxes` (simplified)          | `CreateSandbox`                      | Returns `{uid, name, fastlet_pod}`, no endpoints |
| `GET /sandboxes/{id}`                   | `GetSandbox`                         | Requires labels + expiry in `SandboxInfo` (see below) |
| `GET /sandboxes` (list)                 | `ListSandboxes`                      | Requires label filtering and labels/expiry in items |
| `DELETE /sandboxes/{id}`                | `DeleteSandbox`                      | Async (finalizer); caller polls for NotFound   |
| `POST /sandboxes/{id}/renew-expiration` | `UpdateSandbox` (expire_time_seconds)| Reconciler-enforced                            |
| `PATCH /sandboxes/{id}/metadata`        | `UpdateSandbox` (labels + delete keys)| Requires explicit delete semantics for RFC 7396 |
| `GET /sandboxes/{id}/endpoints/{port}`  | `ResolveEndpoint`                    | Returns proxy URL + `Authorization` header     |
| diagnostics (logs/inspect/events)       | `GetSandboxDiagnostics`              | Lifecycle events only; no process stdout/stderr |
| `pause` / `resume` / snapshot           | (unsupported)                        | Clear "unsupported on fleets" error            |

### Request Parameter Mapping

```python
# OpenSandbox CreateSandboxRequest (accepted subset) → fast-sandbox CreateRequest
{
    "image": {"uri": "python:3.11"},               # → image
    "entrypoint": ["python", "-m", "http.server"],  # → command (+ args)
    "env": {"PYTHONUNBUFFERED": "1"},              # → envs
    "resource_limits": {"cpu": "500m"},            # → validated against SandboxPool profile
    "timeout": 3600,                              # → post-create UpdateSandbox expire_time_seconds
    "network_policy": {...},                       # → new additive CreateRequest field (phase 1b)
    "metadata": {...},                             # → labels in the same post-create UpdateSandbox
    "extensions": {"pool_ref": "default-pool"},    # → pool_ref (else config default)
    # request_id (idempotency key + Sandbox CRD name) = OpenSandbox sandbox_id
}
# Rejected (HTTP 400): volumes, platform, resource_requests, credential_proxy, snapshot_id, image.auth
# Staged (contract kept, enforced in phase 1b): network_policy, secure_access
```

### Status Mapping


| fast-sandbox `Sandbox` state                        | OpenSandbox State |
| --------------------------------------------------- | ----------------- |
| RuntimeState Ready + DataPlaneState Ready           | Running           |
| Pending / Creating                                  | Pending           |
| Draining (delete in progress)                       | Stopping          |
| Stopped / Expired (reconciler-stopped, CRD retained)| Terminated        |
| Failed / Unavailable                                | Failed            |
| (deleted / NotFound)                                | Terminated        |

fast-sandbox splits `RuntimeReady` (runtime up) from `DataPlaneReady` (route + Infra published). OpenSandbox reports **Running only when both are Ready**, matching the existing "endpoint usable" expectation.

> **Important**: On expiry, fast-sandbox's reconciler sets `Stopped/Expired` and **does not delete the CRD**. This state **must** map to `Terminated` so expiration polling reaches a terminal state instead of stalling on an unknown value; `Draining` maps to `Stopping`.

### Extensions Field Support

The `extensions` field in `CreateSandboxRequest` supports fleets-specific options:


| Extension Key    | Type                       | Description                                  |
| ---------------- | -------------------------- | -------------------------------------------- |
| `pool_ref`       | string                     | Target SandboxPool name (overrides default)  |
| `failure_policy` | "manual" \| "auto_recreate"| fast-sandbox failure recovery policy         |

## Integration Conditions & Feasibility

This section records, per integration area, whether fast-sandbox **today** provides what the `fleets` backend needs, and — where it does not — the concrete feasibility plan. Verdicts are based on the current fast-sandbox source, not on documentation aspiration.

### Lifecycle integration

**Verdict: CONDITIONAL.** The current gRPC surface covers the operation names but lacks response fields and update semantics required by the existing OpenSandbox lifecycle contract. Phase 1a therefore depends on the additive Fast-Path extensions below (ask-first review with fast-sandbox maintainers).

The gRPC `FastPathService` (`CreateSandbox` / `GetSandbox` / `ListSandboxes` / `DeleteSandbox` / `UpdateSandbox` / `GetSandboxDiagnostics`) covers the `SandboxService` lifecycle surface, but the semantics differ from the pod backend:

| Method | Condition today | Plan |
| --- | --- | --- |
| Create | Idempotent by `request_id` (= Sandbox CRD name); returns at `RuntimeReady` | Use OpenSandbox `sandbox_id` as `request_id` |
| Get | Reports 3 `ObservedState` strings; **no labels / expiry in payload** | Add labels + expiry to `SandboxInfo`, then map the complete lifecycle response in `status_mapping.py` |
| Renew-expiration | `UpdateSandbox(expire_time_seconds)` persists expiry in the CRD; reconciler marks an expired sandbox `Stopped/Expired` without deleting the CRD | Expose the persisted expiry in `SandboxInfo`; treat enforcement as eventual and map `Stopped/Expired → Terminated` |
| Metadata | `UpdateSandbox(labels)` **merges** labels — it cannot delete a key | Add explicit user-label delete keys (or replace semantics) to `UpdateRequest`; reject changes to reserved system labels |
| Delete | **Async** (finalizer-driven teardown); NotFound = success | Treat delete as async; poll for NotFound |
| Diagnostics | `GetSandboxDiagnostics` returns **lifecycle events only, not stdout/stderr** | Back `inspect`/`events`; command output flows through execd, not this RPC |
| List | **Namespace-only, no label selector; `SandboxInfo` omits labels / expiry** | Add a label selector to `ListRequest` and return the extended `SandboxInfo` |

**Gaps and feasibility:**

1. **Lifecycle read/list fields** — Add `labels` and an optional expiry to `SandboxInfo`, plus a label selector to `ListRequest`. The values come from the durable Sandbox CRD, so get/list continue to work after an OpenSandbox server restart; an in-memory server-side mapping is not sufficient.
2. **RFC 7396 metadata deletion** — Add explicit delete keys (for example, `repeated string delete_labels`) or full replace semantics to `UpdateRequest`. Omitting a key from the existing merge-only `labels` map does not delete it. The implementation must preserve fast-sandbox-owned labels and reject attempts to update/delete reserved keys.
3. **Sandbox logs (`get_sandbox_logs`) not implementable via execd** — `specs/execd-api.yaml` only exposes `/command/{id}/logs` for a known detached command ID; it has no endpoint for the sandbox entrypoint or an arbitrary container. The lifecycle diagnostics route has no command ID. *Plan (phase 1)*: `get_sandbox_logs` returns a clear **"unsupported on fleets"** error; `inspect`/`events` are backed by `GetSandboxDiagnostics` (lifecycle events only). A Fastlet/containerd log API or a backend extension is a future item, not a phase-1 claim.
4. **Delete/expiry are eventual, not synchronous** — *Plan*: `FleetSandboxService` models both as async and preserves the lifecycle contract's poll-for-state semantics via status mapping (`Stopped/Expired → Terminated`, `Draining → Stopping`).

### execd Injection

**Verdict: READY for development, config-only for production.**

fast-sandbox has a real, tested **Infra Component** mechanism, and it ships two execd profiles selectable via `SandboxPool.spec.infraProfile`:

- `opensandbox-execd-quickstart` — `Configured: true`, runnable today. The execd binary (pinned digest, "OpenSandbox Execd v1.0.21") is **baked into the Fastlet image** at build time and supervised inside the sandbox by `sandbox-init` (activation `EntrypointSupervisor`). Service on **port 44772, HTTP, readiness `GET /ping`**. Credential `EXECD_ACCESS_TOKEN` → upstream header `X-EXECD-ACCESS-TOKEN` is injected on the upstream hop only. An e2e test creates a sandbox with this profile and actually runs `opensandbox exec` + file upload/stat/read/download through the official OpenSandbox Go SDK.
- `opensandbox-execd` (production) — deliberately `Configured: false` (placeholder OCI ref + zero digest); the injection mechanism is complete, only the supply-chain binding is missing.

**What OpenSandbox must provide:**

1. **Quick path (phase 1a)**: nothing new — set `SandboxPool.spec.infraProfile = "opensandbox-execd-quickstart"`; execd is already in the Fastlet image.
2. **Production**: bind the `opensandbox-execd` profile with a real immutable OCI reference + valid sha256 digest and configure an `OCIArtifactOpener`. This is **profile registration + supply-chain binding (data/config), no code change to the injection mechanism**.

Readiness contract: `RuntimeReady` (Create returns) is distinct from `DataPlaneReady` (execd probed ready AND route published). OpenSandbox reports Running only at `DataPlaneReady`.

### Ingress / Endpoint Access

**Verdict: FEASIBLE, but requires a new fleets-aware ingress adapter.** `get_endpoint` resolves through fast-sandbox `ResolveEndpoint`, but the *existing* ingress component cannot consume the result as-is.

`ResolveEndpoint` returns `proxy_endpoint` (`SandboxProxyBaseURL` + `/v1/sandboxes/{uid}/ports/{port}`), `required_headers` (`{"Authorization": "Bearer <ed25519-credential>"}`), `route_generation`, and `expires_at_unix_seconds`. fast-sandbox's own `pkg/sandboxclient` shows the "resolve on behalf of a caller, hand back endpoint + headers" pattern, so an intermediary is a first-class supported caller.

**Why the current ingress component cannot be reused unchanged**: `components/ingress` supports only BatchSandbox / AgentSandbox providers, and its `EndpointInfo` carries only a host + secure-access token while the proxy appends `:{port}` itself. It therefore **cannot** consume the path-bearing `ResolveEndpoint` URL (`/v1/sandboxes/{uid}/ports/{port}`) nor inject the required `Authorization` header. Without changes, the phase-1a SDK flow would hit failed/unauthorized upstreams.

**The fleets ingress adapter (phase 1a work item)**:

- Add a **fleets provider** to the ingress component that resolves a sandbox → fast-sandbox Sandbox Proxy route (path form `/v1/sandboxes/{uid}/ports/{port}`) and **injects the `Authorization` bearer** on the upstream hop.
- The gateway exposes a **stable, backend-neutral URL** to the SDK (keyed on `sandbox_id + port`); it holds and **refreshes the ephemeral fast-sandbox credential internally** on expiry / reassignment. This keeps the SDK contract unchanged (no SDK changes) while satisfying fast-sandbox's short-lived credential model — the SDK always calls the same gateway URL and never sees the rotating bearer.

**Integration facts to design around:**

1. **ID mapping** — `ResolveEndpoint` requires the **K8s CRD UID**, not a name. OpenSandbox's `sandbox_id` is used as `request_id` at create (becomes CRD `name`); the server must resolve `name → uid` (via `GetSandbox`, or persist `sandbox_id → uid`). There is no resolve-by-name on `ResolveEndpoint`.
2. **Ephemeral, instance-fenced credentials** — the bearer is short-lived and fenced on `uid + port + FastletPodUID + AssignmentAttempt + RouteGeneration`. The **ingress adapter** (not the SDK) re-resolves (or `IssueRouteCredential`) on expiry and after any reset/reassignment, behind the stable gateway URL. This is what preserves the "no SDK changes" claim; a design that handed the raw bearer to the SDK would require SDK-side refresh and break that claim.
3. **HTTP only** — the transparent proxy supports HTTP/SSE/WebSocket-over-HTTP; raw TCP is not supported. (execd:44772 and egress:18080 are HTTP, so this is fine.)
4. **Route path scheme** — fast-sandbox uses `/v1/sandboxes/{uid}/ports/{port}/...`, deterministic from uid+port; the adapter constructs it.
5. **Any port resolvable** — `ResolveEndpoint` accepts any 1–65535 port with no declared-port allowlist, so 44772, 18080, and user ports all work; OpenSandbox knows the well-known ports out-of-band.

### Egress / Network Policy

**Verdict: FEASIBLE for runc; requires a self-contained new enforcement layer (phase 1b). NOT a reuse of Kubernetes NetworkPolicy.**

fast-sandbox today programs only NAT `MASQUERADE` + sibling `REJECT` — it has **no** FQDN filtering, DNS interception, nftables policy, or per-sandbox ACL. But each sandbox owns its own netns, which is the hook.

- **Injection point**: `internal/fastlet/network/linux_driver.go` — the per-slot netns `OUTPUT` chain where the existing gateway-ACCEPT / sibling-REJECT rules are applied (`ip netns exec <slot> iptables ...`). The `components/egress` nftables + DNS-interception logic (default-drop OUTPUT, DNS-learned `allowed_ips` set, FQDN/CIDR rules) is ported to run **per slot netns** instead of as a pod sidecar.
- **Timing blocker (must be handled)**: slots are **pre-warmed with no owner** and `Prepare()` runs before the sandbox identity exists. A per-sandbox policy therefore **cannot** be applied in `Prepare()`; it needs a new "apply on bind" driver step invoked from `Acquire` (when the owner/policy is known), or pre-warm disabled for policy-bearing sandboxes.
- **Policy delivery (additive contract change, ask-first)**: `network_policy` must flow Create → gRPC `CreateRequest` (new field) → `Sandbox` CRD `SandboxSpec` (new field) → Fastlet protocol `SandboxSpec` → bound to the `Slot`. None of these carry a policy field today; the additions are backward-compatible but touch a public proto/CRD, so they require review with fast-sandbox maintainers.
- **Runtime restriction (evidence-justified)**: the runtime driver attaches the netns uniformly for runc/gVisor/Kata with no per-runtime network compensation, and Kata translates the interface into a guest NIC (its own stack). In-netns `OUTPUT` iptables therefore reliably filter **runc** only. Phase 1 restricts `network_policy` to the `container` runtime and rejects egress on gVisor/Kata.
- **Runtime `/policy` API**: a small per-slot policy endpoint on 18080 reachable via the gateway serves `egress-api.yaml` GET/PATCH/DELETE, or maps onto a fast-sandbox control-plane call.
- **Janitor**: in-netns rules are reaped for free by netns deletion (no janitor change); any out-of-netns state (e.g. a host-side DNS proxy or ipset) would need NodeJanitor awareness.
- **DNS**: no DNS proxy exists (each slot copies `resolv.conf`). FQDN policy requires a net-new per-netns DNS listener + a `resolv.conf` pointing at it.

## Construction Phases

Both phases are part of the first release. The split exists so the cross-repo network work (1b) does not block end-to-end validation of the service seam (1a). In phase 1a, `network_policy` is **rejected** (not silently ignored), so the backend never claims an enforcement it does not have.

### Phase 1a — Service seam, lifecycle, execd, ingress (OpenSandbox-mostly, plus additive lifecycle RPC fields)

- Add `FleetSandboxService` + register `"fleets"` in `factory.py`; add `FleetsRuntimeConfig`.
- Implement `fastpath_client` (Create / Get / Delete / Update / List / Diagnostics / ResolveEndpoint); add the lifecycle proto/implementation fields described above (`SandboxInfo` labels + expiry, list label selector, metadata delete keys) after ask-first review with fast-sandbox maintainers.
- Simplified Create mapping + **explicit rejection** of `volumes` / `platform` / `resource_requests` / `credential_proxy` / `snapshot_id` / `image.auth`; **reject `network_policy` and `secure_access`** ("coming in fleets phase 1b"). No unsupported field is silently ignored.
- Create+`timeout`/initial metadata: apply expiry and labels in one follow-up `UpdateSandbox` and **roll back (delete) on update failure**, so a failed update cannot leave an immortal or incorrectly labelled sandbox.
- **Tenant handling**: map each OpenSandbox tenant to a fast-sandbox namespace on every call, or **reject `[tenants]` configuration** for fleets if the mapping is not implemented.
- **Fleets ingress adapter**: add a fleets provider to `components/ingress` that routes to the fast-sandbox Sandbox Proxy path form and injects the `Authorization` bearer; expose a stable gateway URL and refresh the ephemeral credential internally (keeps SDK unchanged).
- Implement `get_endpoint` via `ResolveEndpoint`; persist `sandbox_id → uid`; credential lifecycle handled by the ingress adapter, not the SDK.
- execd via `opensandbox-execd-quickstart` Infra profile; verify exec/file end-to-end through the SDK.
- Lifecycle get/delete/renew/list/metadata working (labels + expiry survive server restart; delete/expiry async; `Stopped/Expired → Terminated`, `Draining → Stopping`; metadata deletion uses explicit backend delete keys and protects reserved labels); `get_sandbox_logs`, pause/resume/snapshot return clear unsupported errors.
- **Exit criteria**: full SDK flow (create → exec → file → delete) passes on a Kind cluster with fast-sandbox, no SDK changes; tenant isolation verified (or `[tenants]` rejected); unsupported-field rejection verified.

### Phase 1b — Per-sandbox egress enforcement + secure access (cross-repo, higher risk)

- Additive `network_policy` field on fast-sandbox `CreateRequest` (proto), `Sandbox` CRD, Fastlet protocol `SandboxSpec`, and `Slot` (ask-first review with fast-sandbox maintainers).
- New "apply on bind" driver step invoked from `Acquire` (resolves the pre-warm timing blocker); port `components/egress` nft + DNS logic into the per-slot netns.
- Per-slot `/policy` runtime endpoint on 18080 via the gateway; validate against `egress-api.yaml`.
- Restrict `network_policy` to the `container` (runc) runtime; reject egress on gVisor/Kata.
- Extend NodeJanitor for any out-of-netns state (DNS proxy / ipset).
- Flip Create to **accept and enforce** `network_policy`.
- **Secure access**: accept `secure_access`; layer server-issued access headers onto the ingress-gateway route, reusing fast-sandbox's `required_headers` / short-lived Ed25519 credential model (OSEP-0011 semantics).
- **Exit criteria**: a fleets sandbox with a deny-by-default FQDN allowlist blocks non-allowed egress and permits allowed FQDNs, end-to-end; co-located sandboxes with different policies do not interfere; a `secure_access` sandbox returns access headers and rejects unauthenticated endpoint access.

## Test Plan

- **Unit Tests**: `fastpath_client` gRPC wrapper; Create field acceptance/rejection matrix; status mapping; `get_endpoint` gateway routing
- **Integration Tests**: Deploy fast-sandbox in a Kind cluster; test create/get/delete/renew/list/metadata flows; execd connectivity on 44772
- **E2E Tests**: Full OpenSandbox SDK flow using the `fleets` runtime, asserting behavior identical to the pod backend for lifecycle + exec/file
- **Egress Tests (phase 1b)**: deny-by-default block, FQDN allowlist permit, per-sandbox isolation between co-located sandboxes, `/policy` GET/PATCH/DELETE, runc-only guard
- **Performance Tests**: create latency and density vs the `kubernetes` pool backend, reported per fast-sandbox methodology (commit/env/runtime/cache-state/concurrency/percentiles); compare the user-visible `Running` + execd-usable milestone separately from fast-sandbox's internal `RuntimeReady`

### Test Scenarios

1. Basic lifecycle: create → status query → delete (delete is async; poll for NotFound)
2. Expiration: renewal (eventual; poll status); expired sandbox reaches `Terminated` (Stopped/Expired mapping), not a stuck/unknown state
3. Create+`timeout` update failure triggers rollback (no immortal sandbox left behind)
4. Simplified Create: `volumes` / `platform` / `resource_requests` / `credential_proxy` / `snapshot_id` / `image.auth` rejected with clear errors; `network_policy` / `secure_access` rejected in 1a, accepted in 1b; `credential_proxy` still rejected in 1b
5. Tenant isolation: a tenant cannot see/operate another tenant's sandboxes via list or ID; or `[tenants]` is rejected for fleets
6. Metadata PATCH with a `null` value deletes the key through the explicit backend delete operation, preserving system labels; get/list return the result after a server restart
7. Pool selection via `extensions.pool_ref`; `resource_limits` incompatible with pool profile rejected
8. Ingress adapter: SDK reaches execd/user ports through the stable gateway URL; credential rotates internally without SDK involvement
9. Image affinity: record candidate/cache-hit state for repeated creates and report it separately from end-to-end latency
10. Failure: Fast-Path unavailable, invalid pool ref
11. Concurrent sandbox creation (stress test)
12. Egress (phase 1b): deny-by-default blocks; allowed FQDN permits; co-located sandboxes with different policies do not interfere
13. `get_sandbox_logs` / `pause` / `resume` / snapshot return clear "unsupported on fleets" errors

### Performance Benchmarks

The figures below are the current fast-sandbox engineering evidence, not `fleets` targets:

| Scope | Observation | Measurement boundary |
| --- | --- | --- |
| Warm container (`runc`) | 20 samples: mean 76.02 ms, p50 75.95 ms, p95 83.15 ms | Concurrency 1; cached image/artifacts; minimal Infra profile; client Fast-Path Create through `RuntimeReady` |
| gVisor (`runsc`) | 10 samples: mean 644.29 ms | Small diagnostic batch; cached artifacts; Execd readiness excluded |
| Kata Cloud Hypervisor | 10 samples: mean 1,359.59 ms | Small diagnostic batch under nested KVM; Execd readiness excluded |
| Kata QEMU | 10 samples: mean 2,125.58 ms | Small diagnostic batch under nested KVM; Execd readiness excluded |
| BatchSandbox pool vs fleets | No comparable report yet | Must run both through the same OpenSandbox endpoint, environment, cache state, concurrency, and readiness boundary |
| Registry Top-K | No portable latency claim | `BenchmarkRegistryTopK1000` is a scheduler microbenchmark, not Sandbox Create |

Source: fast-sandbox [`3af0222` performance guide](https://github.com/opensandbox-group/fast-sandbox/blob/3af02227a85d3ae872e67ef718b63c60e777edac/docs/guides/performance.md), whose measurements describe base revision `42fe03549598c3ab730b989c7757634b486697cf`.

There is no absolute-millisecond exit threshold. The performance goal passes only when the matched OpenSandbox benchmark shows lower fleets p50 and p95 from SDK create start through `Running` and an execd readiness probe than the BatchSandbox pool. fast-sandbox `RuntimeReady` is reported as a separate diagnostic milestone and is not substituted for that user-visible comparison.

The `fleets` acceptance report must record commit SHA and command; hardware, virtualization, Kubernetes, and containerd versions; component replicas; runtime and Infra profile; image/cache/network-slot state; concurrency and request rate; start/end milestones; p50/p95/p99/max; failures, admission rejections, and retries. It must report `RuntimeReady`, `DataPlaneReady`, and OpenSandbox client-observed latency separately. Image-affinity and Top-K microbenchmarks remain supporting diagnostics and must not substitute for the end-to-end comparison.

## Drawbacks

- **Added Dependency**: Requires deploying and managing the fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy), Fastlet pools, and NodeJanitor DaemonSet
- **Feature gap vs. pod backend**: no volumes, no pause/resume/snapshot, no per-sandbox K8s NetworkPolicy, no per-sandbox node scheduling — fleets is a deliberate subset
- **Bespoke egress layer (phase 1b)**: per-sandbox egress is a new network layer with a cross-repo proto/CRD change — the highest-cost part of the integration; gVisor/Kata + egress is unsupported in phase 1
- **Operational Complexity**: Teams need to understand both OpenSandbox and fast-sandbox concepts
- **gRPC Protocol**: Introduces gRPC on the server's backend surface (vs pure HTTP/REST)
- **Limited Ecosystem**: fast-sandbox is a newer project with a smaller community than vanilla K8s

## Alternatives

1. **Full replacement of the pod backend**: Rejected — the pod backend's volumes, snapshots, and per-sandbox K8s NetworkPolicy have no equivalent in the shared-Fastlet model
2. **Implement fleets as a K8s `WorkloadProvider`**: Rejected — that ABC is Kubernetes-internal (namespace/CR/pod-spec) and cannot host a separate gRPC control plane
3. **Only the declarative fast-sandbox CRD path (no gRPC)**: Rejected — loses the in-memory-placement latency benefit that motivates fleets
4. **Reuse Kubernetes NetworkPolicy for egress**: Rejected — all sandboxes SNAT to one Fastlet pod IP, so K8s NetworkPolicy cannot distinguish sandboxes
5. **Direct `FastletPodIP:port` endpoints**: Deferred — workable but requires port allocation + auth handling in OpenSandbox; the ingress gateway is backend-neutral and reuses fast-sandbox's existing authenticated proxy chain

## Infrastructure Needed

- **CI/CD**: Kind cluster with fast-sandbox (Fast-Path, Reconcilers, Sandbox Proxy, a Fastlet pool, NodeJanitor) for integration/e2e
- **Documentation**: fleets deployment guide; execd-as-Infra-Component setup; egress enforcement guide (phase 1b); compatibility matrix
- **Helm Charts** (optional): Unified charts deploying OpenSandbox Server + fast-sandbox components
- **Cross-repo coordination**: with fast-sandbox maintainers for the additive lifecycle RPC fields/delete semantics and the `network_policy` proto/CRD field

## Upgrade & Migration Strategy

- **Backwards Compatible**: Default runtime unchanged; `fleets` is opt-in via `config.runtime.type = "fleets"`
- **No Migration**: Existing Docker/Kubernetes runtime users unaffected
- **Enable by Config**: Set `type = "fleets"` and add the `[fleets]` block; deploy fast-sandbox and an ingress gateway wired to its Sandbox Proxy
- **Rollback**: Switch `type` back to `kubernetes` or `docker`; fleets sandboxes are ephemeral (no persistent state to migrate)
- **Choosing a backend**: use `kubernetes` when you need volumes, pause/resume/snapshot, per-sandbox K8s NetworkPolicy, or per-sandbox node scheduling; use `fleets` for latency-sensitive, stateless, high-density workloads needing only lifecycle + exec/file + per-sandbox FQDN egress
