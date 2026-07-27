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
  - [How Fast-Sandbox Achieves Millisecond-Scale Latency](#how-fast-sandbox-achieves-millisecond-scale-latency)
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

Introduce a new OpenSandbox backend type, **`sandbox fleets`** (runtime `type = "fleets"`), whose sole implementation is backed by [fast-sandbox](https://github.com/fengcone/fast-sandbox). A fleet runs many sandboxes as isolated runtimes (container / gVisor / Kata) inside pre-warmed **Fastlet** pods, reached through fast-sandbox's gRPC Fast-Path control plane and its authenticated proxy data plane. By leveraging the Fast-Path API and warm Fastlet pools, OpenSandbox can achieve **millisecond-scale cold start latency** (compared to ~1 second with OpenSandbox's BatchSandbox pool, or 2-5 seconds with standard K8s runtime) for AI Agents, Serverless functions, and other latency-sensitive workloads.

`fleets` is **additive and parallel** to the existing `docker` and `kubernetes` backends; it does **not** replace the pod-per-sandbox `kubernetes` backend. The integration is deliberately scoped:

- **Create** is a *simplified* subset of `CreateSandboxRequest`. Pod-identity-dependent fields (`volumes`, `platform` node-selectors, `secure_access`, `snapshot_id`, pause/resume) are explicitly rejected.
- **Lifecycle** (get / delete / renew-expiration / list / metadata), **execd** (exec / file), and **egress** (`network_policy`) reuse the *existing public API contracts* unchanged, so upstream SDKs are unaffected.

**Performance Characteristics** (with cached images on Fastlet nodes):

- Create returns at `RuntimeReady`: **~50-100ms base + K8s API write latency** (CRD-first, typically 20-50ms via etcd). Required Infra services (e.g. execd) and route publication continue asynchronously until `DataPlaneReady`.

> **Correction from earlier drafts**: fast-sandbox does **not** implement a "Fast Mode" (container-first / async-CRD / eventual-consistency <50ms) path. Every create is **CRD-first** (one synchronous etcd write, then one atomic Fastlet admission). The gRPC entry avoids the K8s *scheduler* and *watch propagation*, not the CRD/etcd write. This OSEP also uses the real fast-sandbox terminology (**Fastlet** / **SandboxPool**), not the "Agent" / "AgentPool" terms from earlier drafts.

> **Note**: The millisecond-scale latency assumes the container image is already cached on the Fastlet's host node. Cold starts with uncached images incur additional image pull time.

## Motivation

OpenSandbox currently supports Docker and Kubernetes runtimes. While the Kubernetes runtime provides scalability, sandbox creation typically takes 2-5 seconds due to:

- K8s scheduler latency (~100-500ms)
- etcd write and watch propagation (~50-200ms)
- Kubelet pod creation and container runtime startup (~1-3s)
- Image pull when cache miss occurs (~1-10s)

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

**Performance with pool** (measured):

- Approximately **1 second** latency for pool-based allocation
- Eliminates scheduler wait and pod startup time
- Still requires K8s API write + watch propagation overhead
- Image must be pre-pulled in pool pods

This is an effective optimization for many use cases. However, fast-sandbox aims to push latency even lower through additional innovations described below.

For AI Agent and Serverless scenarios that require rapid sandbox provisioning, reducing even the K8s API overhead is valuable.

### Why Fast-Sandbox is Fast

fast-sandbox achieves millisecond-scale cold start through three key design innovations:

**Comparison: OpenSandbox Pool vs fast-sandbox**


| Aspect                          | OpenSandbox BatchSandbox Pool                      | fast-sandbox (fleets)                       |
| ------------------------------- |----------------------------------------------------|---------------------------------------------|
| **Allocation mechanism**        | K8s API write → Controller watch → Task assignment | gRPC → CRD write → in-memory Top-K → Fastlet admission |
| **Latency (with cached image)** | ~1 second (measured)                               | ~50-100ms + API write (CRD-first)           |
| **Scheduling**                  | K8s Scheduler places pool pods (one-time)          | In-memory Top-K registry with image affinity |
| **Image awareness**             | Pool pods have fixed image                         | Registry ranks by image cache availability  |
| **Customization**               | entrypoint, env only                               | entrypoint, env, image per request          |
| **Container creation**          | pre-warmed                                         | Direct containerd socket inside Fastlet     |
| **Consistency**                 | Strong (K8s etcd)                                  | Strong (CRD-first, K8s etcd)                |
| **Failure recovery**            | K8s Controller reconciliation                      | NodeJanitor + AutoRecreate policy           |

Both approaches use pre-provisioned resource pools to eliminate cold start overhead. fast-sandbox's key advantage is bypassing the K8s **scheduler and watch propagation** for container placement while still committing durable intent through a CRD write.

#### 1. gRPC Fast-Path Allocation, Bypassing the K8s Scheduler

Traditional K8s sandbox creation follows the slow path:

```
Client → K8s API Server → etcd → Scheduler → etcd → Kubelet → Container Runtime
 (~2-5 seconds total)
```

fast-sandbox uses a gRPC Fast-Path that is **CRD-first for every create** — it does not bypass etcd, but it bypasses the scheduler queue and watch propagation:

```
Client → gRPC Fast-Path → K8s API (Sandbox CRD write, IO 1)
       → in-memory Top-K placement → atomic Fastlet admission/create (IO 2) → containerd
       (~50-100ms base + 20-50ms API write, image cached)
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
| **Container Creation**     | Direct containerd socket access (bypasses kubelet)             | <10ms container creation vs ~500ms        |
| **Security Containers**    | Supports gVisor/Kata Containers via containerd runtime handler | Same workflow, different runtime class    |
| **Network Namespace**      | Each sandbox gets its own netns + private IP inside the Fastlet Pod | K8s CNI plugins carry the Fastlet Pod's traffic |

The key insight: **use K8s for what it's good at** (resource accounting, cluster management, scheduling constraints at the Fastlet-pool granularity), but **bypass the K8s scheduler for the hot path** (container placement + creation).

### Goals

- Add a `fleets` runtime type (`config.runtime.type = "fleets"`) implemented as a new `FleetSandboxService` (a `SandboxService`, **not** a Kubernetes `WorkloadProvider`)
- Reuse the existing lifecycle API (`get` / `delete` / `renew-expiration` / `list` / `metadata`) with no changes to routes or SDKs
- Reuse the existing execd exec/file access pattern (`get_endpoint(id, 44772)` → in-sandbox execd HTTP) unchanged
- Reuse the existing egress contract (`network_policy` at create; `egress-api.yaml` `/policy` at runtime), and **enforce per-sandbox egress** on fast-sandbox
- Provide a simplified Create that maps a well-defined subset of `CreateSandboxRequest` to fast-sandbox's gRPC `CreateSandbox`, and cleanly rejects unsupported fields
- Achieve sub-100ms create latency (CRD-first, with cached image), for latency-sensitive, stateless workloads
- Provide flexible deployment: users can bring their own fast-sandbox or use OpenSandbox-provided charts

### Non-Goals

- Replacing or removing the existing Docker or Kubernetes runtimes
- Supporting `volumes` (PVC / host / ossfs), `platform` node-selectors, `secure_access` signed endpoints, or `snapshot_id` on `fleets`
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
      (2-5 seconds)

Sandbox Fleets (fast-sandbox, CRD-first — the only path):
OpenSandbox Server → gRPC Fast-Path → K8s API (CRD write) → in-memory Top-K
                   → atomic Fastlet admission → containerd
      (~50-100ms base + 20-50ms API write; scheduler + watch propagation bypassed)
```

### The `fleets` runtime type and API reuse model

The three "reused" API areas reuse *different* things. Being precise here avoids the biggest integration trap: **there is no server-side exec or egress API endpoint.** exec/file and egress runtime control are HTTP contracts that clients speak *directly to components inside the sandbox*; the server's only role is `get_endpoint`.

| Area | What is reused | What must be built for `fleets` |
| --- | --- | --- |
| Lifecycle (get/delete/renew/list/metadata) | Routes + `SandboxService` ABC — free once the ABC is implemented | Map each method to a fast-sandbox gRPC call |
| execd exec/file | `specs/execd-api.yaml` (client → in-sandbox execd:44772) is backend-agnostic | Run execd inside the sandbox; implement `get_endpoint(id, 44772)` |
| egress `network_policy` | `NetworkPolicy` schema + `egress-api.yaml` `/policy` (client → egress proxy:18080) | Deliver + enforce policy per sandbox netns; implement `get_endpoint(id, 18080)` with auth header |
| Endpoint resolution | `SandboxService.get_endpoint` → `Endpoint`; ingress-gateway routing is backend-neutral | Route fleets ports through the ingress gateway |

**Simplified Create** accepts this subset of `CreateSandboxRequest`: `image`, `entrypoint`, `env`, `timeout` (→ `expireTime`), `resource_limits` (advisory; enforced by the target `SandboxPool` profile), `network_policy`, `metadata`, `extensions.pool_ref`. It **rejects** (HTTP 400, clear message): `volumes`, `platform`, `secure_access`, `snapshot_id`, and any pause/resume/snapshot call.

### Notes/Constraints/Caveats

- The fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy) and Fastlet pools + NodeJanitor must be deployed separately (by the user or via OpenSandbox-provided Helm charts)
- fast-sandbox uses its own CRD types (`Sandbox`, `SandboxPool`, group `sandbox.fast.io/v1alpha1`) - OpenSandbox does not manipulate these directly
- gRPC communication requires network reachability from OpenSandbox Server to the fast-sandbox Fast-Path Server
- execd is injected via fast-sandbox's **Infra Component** mechanism (see [execd Injection](#execd-injection)), not the K8s init-container copy used by the pod backend
- Because all sandboxes in a Fastlet pod share that pod's K8s network identity (SNAT to one pod IP), **standard Kubernetes NetworkPolicy cannot express per-sandbox egress** for fleets; per-sandbox egress is enforced inside each sandbox's netns (see [Egress / Network Policy](#egress--network-policy))

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

### How Fast-Sandbox Achieves Millisecond-Scale Latency

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
│     └─────────────────────────────────────────────────> ~1ms             │
│                                                                          |
│  2. In-memory Top-K placement (registry-only, no K8s API)                │
│     └─────────────────────────────────────────────────> ~1-14ms          │
│     • Filter by pool, namespace, runtime/profile, capacity               │
│     • Rank by: image-hit, then used/capacity, then stable hash           │
│                                                                          |
│  3. IO 1: Sandbox CRD write to K8s API / etcd                            │
│     └─────────────────────────────────────────────────> ~20-50ms         │
│     • Durable intent + idempotency by request_id                         │
│                                                                          |
│  4. IO 2: atomic Fastlet admission → containerd.Create() (cached image)  │
│     └─────────────────────────────────────────────────> ~10-30ms         │
│     • Direct socket access to host containerd                            │
│     • No image pull (cached); sandbox gets its own netns + private IP    │
│                                                                          |
│  5. Fast-Path returns {sandbox_uid, sandbox_name, fastlet_pod}           │
│     <───────────────────────────────────────────────── ~1ms              │
│     • Returns at RuntimeReady; endpoints resolved later via ResolveEndpoint │
│                                                                          |
│  Total: ~50-100ms (end-to-end, with cached image)                        │
│                                                                          |
│  If image is NOT cached: image pull time is added to step 4              │
└──────────────────────────────────────────────────────────────────────────┘
```

Compare to standard K8s:

```
1. API Server write to etcd              ~20ms
2. Scheduler watch and decision          ~100-500ms
3. Scheduler write to etcd               ~20ms
4. Kubelet watch and pod creation        ~50-200ms
5. Container runtime start               ~500ms-3s
6. Image pull (if cache miss)            ~1-10s
Total: 2-5s (best case, cache hit)
```

The difference is steps 2-4 of the K8s path (scheduler queue + watch propagation + kubelet). fast-sandbox keeps the etcd write (as IO 1) but replaces the scheduler/watch/kubelet steps with in-memory placement + a direct Fastlet create.

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

**Performance characteristics** (from fast-sandbox benchmarks; to be re-verified per methodology):

- 100 Fastlets: ~1.3ms placement time
- 1000 Fastlets: ~14ms placement time

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
    containerd.WithNewSnapshot(...),             // Instant with cache
    // runc by default; "io.containerd.runsc.v1" (gVisor) or Kata shim per pool runtime
    oci.WithLinuxNamespace(networkNamespace),    // sandbox's own netns
)

task, _ := container.NewTask(ctx, cio.NewCreator(...))
task.Start(ctx)
```

This approach:

- Eliminates kubelet API overhead (~50-200ms)
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
| `GET /sandboxes/{id}`                   | `GetSandbox`                         | Maps 3 ObservedState fields                    |
| `GET /sandboxes` (list)                 | `ListSandboxes`                      | Namespace-only; label filter is a gap (see below) |
| `DELETE /sandboxes/{id}`                | `DeleteSandbox`                      | Async (finalizer); caller polls for NotFound   |
| `POST /sandboxes/{id}/renew-expiration` | `UpdateSandbox` (expire_time_seconds)| Reconciler-enforced                            |
| `PATCH /sandboxes/{id}/metadata`        | `UpdateSandbox` (labels)             | Merged into CRD labels                         |
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
    "timeout": 3600,                              # → UpdateSandbox expire_time_seconds
    "network_policy": {...},                       # → new additive CreateRequest field (phase 1b)
    "metadata": {...},                             # → labels
    "extensions": {"pool_ref": "default-pool"},    # → pool_ref (else config default)
    # request_id (idempotency key + Sandbox CRD name) = OpenSandbox sandbox_id
}
# Rejected (HTTP 400): volumes, platform, secure_access, snapshot_id
```

### Status Mapping


| fast-sandbox `Sandbox` state                        | OpenSandbox State |
| --------------------------------------------------- | ----------------- |
| RuntimeState Ready + DataPlaneState Ready           | Running           |
| Pending / Creating                                  | Pending           |
| Failed / Unavailable                                | Failed            |
| (deleted / finalizer draining)                      | Terminated        |

fast-sandbox splits `RuntimeReady` (runtime up) from `DataPlaneReady` (route + Infra published). OpenSandbox reports **Running only when both are Ready**, matching the existing "endpoint usable" expectation.

### Extensions Field Support

The `extensions` field in `CreateSandboxRequest` supports fleets-specific options:


| Extension Key    | Type                       | Description                                  |
| ---------------- | -------------------------- | -------------------------------------------- |
| `pool_ref`       | string                     | Target SandboxPool name (overrides default)  |
| `failure_policy` | "manual" \| "auto_recreate"| fast-sandbox failure recovery policy         |

## Integration Conditions & Feasibility

This section records, per integration area, whether fast-sandbox **today** provides what the `fleets` backend needs, and — where it does not — the concrete feasibility plan. Verdicts are based on the current fast-sandbox source, not on documentation aspiration.

### Lifecycle integration

**Verdict: READY, with three gaps to design around.**

The gRPC `FastPathService` (`CreateSandbox` / `GetSandbox` / `ListSandboxes` / `DeleteSandbox` / `UpdateSandbox` / `GetSandboxDiagnostics`) covers the `SandboxService` lifecycle surface, but the semantics differ from the pod backend:

| Method | Condition today | Plan |
| --- | --- | --- |
| Create | Idempotent by `request_id` (= Sandbox CRD name); returns at `RuntimeReady` | Use OpenSandbox `sandbox_id` as `request_id` |
| Get | Reports 3 `ObservedState` strings; **no labels / expiry in payload** | Map states in `status_mapping.py` |
| Renew-expiration | `UpdateSandbox(expire_time_seconds)`; reconciler garbage-collects on expiry (marks `Stopped/Expired`, does not delete CRD) | Treat expiry as eventual; poll status |
| Metadata | `UpdateSandbox(labels)` merged into CRD labels | Direct map |
| Delete | **Async** (finalizer-driven teardown); NotFound = success | Treat delete as async; poll for NotFound |
| Diagnostics | `GetSandboxDiagnostics` returns **lifecycle events only, not stdout/stderr** | Back `inspect`/`events`; command output flows through execd, not this RPC |
| List | **Namespace-only, no label selector; `SandboxInfo` omits labels** | **Gap** (see below) |

**Gaps and feasibility:**

1. **Label-filtered list** — OpenSandbox `list` uses label selectors, but `ListSandboxes` takes only a namespace and `SandboxInfo` does not carry labels. *Plan*: additive proto/impl extension on fast-sandbox (label filter + labels in `SandboxInfo`), or server-side filtering by tracking the mapping locally. Additive, non-breaking.
2. **No process log streaming in the control plane** — logs come from execd over the data plane, not `GetSandboxDiagnostics`. *Plan*: `get_sandbox_logs` proxies to execd; `inspect`/`events` use diagnostics.
3. **Delete/expiry are eventual, not synchronous** — *Plan*: `FleetSandboxService` models both as async and preserves the lifecycle contract's poll-for-state semantics via status mapping.

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

**Verdict: FEASIBLE — `get_endpoint` = call `ResolveEndpoint` → return `proxy_endpoint` + `required_headers`.**

`ResolveEndpoint` returns `proxy_endpoint` (`SandboxProxyBaseURL` + `/v1/sandboxes/{uid}/ports/{port}`), `required_headers` (`{"Authorization": "Bearer <ed25519-credential>"}`), `route_generation`, and `expires_at_unix_seconds`. fast-sandbox's own `pkg/sandboxclient` is a reference implementation of exactly this "resolve on behalf of a caller, hand back endpoint + headers to the OpenSandbox SDK" pattern, so an intermediary (the OpenSandbox server / ingress gateway) is a first-class supported caller.

**Integration facts to design around:**

1. **ID mapping** — `ResolveEndpoint` requires the **K8s CRD UID**, not a name. OpenSandbox's `sandbox_id` is used as `request_id` at create (becomes CRD `name`); the server must resolve `name → uid` (via `GetSandbox`, or persist `sandbox_id → uid`). There is no resolve-by-name on `ResolveEndpoint`.
2. **Ephemeral, instance-fenced credentials** — the bearer is short-lived and fenced on `uid + port + FastletPodUID + AssignmentAttempt + RouteGeneration`. The server must treat endpoint+headers as ephemeral: re-resolve (or `IssueRouteCredential`) on expiry and after any reset/reassignment. It cannot cache a long-lived endpoint.
3. **HTTP only** — the transparent proxy supports HTTP/SSE/WebSocket-over-HTTP; raw TCP is not supported. (execd:44772 and egress:18080 are HTTP, so this is fine.)
4. **Route path scheme** — fast-sandbox uses `/v1/sandboxes/{uid}/ports/{port}/...`, deterministic from uid+port. The ingress gateway must construct/consume this form; a formatting adapter, not a blocker.
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

### Phase 1a — Service seam, lifecycle, execd, ingress (OpenSandbox-mostly, low risk)

- Add `FleetSandboxService` + register `"fleets"` in `factory.py`; add `FleetsRuntimeConfig`.
- Implement `fastpath_client` (Create / Get / Delete / Update / List / Diagnostics / ResolveEndpoint).
- Simplified Create mapping + rejection of `volumes` / `platform` / `secure_access` / `snapshot_id`; **reject `network_policy`** ("coming in fleets egress phase").
- Implement `get_endpoint` via `ResolveEndpoint`, wired through the ingress gateway; persist `sandbox_id → uid`; treat credentials as ephemeral.
- execd via `opensandbox-execd-quickstart` Infra profile; verify exec/file end-to-end through the SDK.
- Lifecycle get/delete/renew/list/metadata working (delete/expiry async); pause/resume/snapshot return clear unsupported errors.
- **Exit criteria**: full SDK flow (create → exec → file → delete) passes on a Kind cluster with fast-sandbox, no SDK changes.

### Phase 1b — Per-sandbox egress enforcement (cross-repo, higher risk)

- Additive `network_policy` field on fast-sandbox `CreateRequest` (proto), `Sandbox` CRD, Fastlet protocol `SandboxSpec`, and `Slot` (ask-first review with fast-sandbox maintainers).
- New "apply on bind" driver step invoked from `Acquire` (resolves the pre-warm timing blocker); port `components/egress` nft + DNS logic into the per-slot netns.
- Per-slot `/policy` runtime endpoint on 18080 via the gateway; validate against `egress-api.yaml`.
- Restrict `network_policy` to the `container` (runc) runtime; reject egress on gVisor/Kata.
- Extend NodeJanitor for any out-of-netns state (DNS proxy / ipset).
- Flip Create to **accept and enforce** `network_policy`.
- **Exit criteria**: a fleets sandbox with a deny-by-default FQDN allowlist blocks non-allowed egress and permits allowed FQDNs, end-to-end; co-located sandboxes with different policies do not interfere.

## Test Plan

- **Unit Tests**: `fastpath_client` gRPC wrapper; Create field acceptance/rejection matrix; status mapping; `get_endpoint` gateway routing
- **Integration Tests**: Deploy fast-sandbox in a Kind cluster; test create/get/delete/renew/list/metadata flows; execd connectivity on 44772
- **E2E Tests**: Full OpenSandbox SDK flow using the `fleets` runtime, asserting behavior identical to the pod backend for lifecycle + exec/file
- **Egress Tests (phase 1b)**: deny-by-default block, FQDN allowlist permit, per-sandbox isolation between co-located sandboxes, `/policy` GET/PATCH/DELETE, runc-only guard
- **Performance Tests**: create latency and density vs the `kubernetes` pool backend, reported per fast-sandbox methodology (commit/env/runtime/cache-state/concurrency/percentiles)

### Test Scenarios

1. Basic lifecycle: create → status query → delete (delete is async; poll for NotFound)
2. Expiration renewal (eventual; poll status)
3. Simplified Create: `volumes` / `platform` / `secure_access` / `snapshot_id` rejected with clear errors
4. Pool selection via `extensions.pool_ref`; `resource_limits` incompatible with pool profile rejected
5. Image affinity: second sandbox on same Fastlet (should be faster)
6. Failure: Fast-Path unavailable, invalid pool ref
7. execd connectivity after sandbox creation; ephemeral credential re-resolve on expiry
8. Concurrent sandbox creation (stress test)
9. Egress (phase 1b): deny-by-default blocks; allowed FQDN permits; co-located sandboxes with different policies do not interfere
10. `pause` / `resume` / snapshot return clear "unsupported on fleets" errors

### Performance Benchmarks

Target metrics (to be verified in tests):


| Scenario                                | Target Latency         | Notes                                           |
| --------------------------------------- | ---------------------- | ----------------------------------------------- |
| OpenSandbox BatchSandbox Pool           | ~1 second              | Measured with K8s API + watch overhead          |
| Cold start, image cached (fleets)       | ~50-100ms + API write  | CRD-first; ~20-50ms for K8s API/etcd; scheduler + watch bypassed |
| Cold start, image NOT cached            | Base + image pull time | Image pull depends on size and network          |
| Warm start (reuse same Fastlet)         | <30ms                  | Fastlet already selected                        |
| Registry Top-K placement (100 Fastlets) | ~1.3ms                 | In-memory scheduling                            |
| Registry Top-K placement (1000 Fastlets)| ~14ms                  | In-memory scheduling                            |

> **Important**: The latencies above assume the container image is already cached on the Fastlet's host node. In production, pre-pulling images (via `SandboxPool.spec.warmImages`) or using a common set of base images is recommended for consistent performance. Per fast-sandbox's performance methodology, benchmark results should record commit, environment, runtime, cache state, concurrency, and percentile distribution rather than a single headline number.

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
- **Cross-repo coordination**: with fast-sandbox maintainers for the additive `network_policy` proto/CRD field

## Upgrade & Migration Strategy

- **Backwards Compatible**: Default runtime unchanged; `fleets` is opt-in via `config.runtime.type = "fleets"`
- **No Migration**: Existing Docker/Kubernetes runtime users unaffected
- **Enable by Config**: Set `type = "fleets"` and add the `[fleets]` block; deploy fast-sandbox and an ingress gateway wired to its Sandbox Proxy
- **Rollback**: Switch `type` back to `kubernetes` or `docker`; fleets sandboxes are ephemeral (no persistent state to migrate)
- **Choosing a backend**: use `kubernetes` when you need volumes, pause/resume/snapshot, per-sandbox K8s NetworkPolicy, or per-sandbox node scheduling; use `fleets` for latency-sensitive, stateless, high-density workloads needing only lifecycle + exec/file + per-sandbox FQDN egress
