---
title: Sandbox Lifecycle Hooks
authors:
  - "@pjp"
creation-date: 2026-08-17
last-updated: 2026-08-17
status: draft
---

# OSEP-0020: Sandbox Lifecycle Hooks

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Hook Set](#hook-set)
  - [Two Execution Channels](#two-execution-channels)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Public API: CreateSandboxRequest.lifecycle](#1-public-api-createsandboxrequestlifecycle)
  - [2. Public API: PATCH /sandboxes/{sandboxId}/lifecycle](#2-public-api-patch-sandboxessandboxidlifecycle)
  - [3. Config Persistence by Provider](#3-config-persistence-by-provider)
  - [4. execd Lifecycle API](#4-execd-lifecycle-api)
  - [5. In-process Channel: preStart](#5-in-process-channel-prestart)
  - [6. Periodic Hooks (cron)](#6-periodic-hooks-cron)
  - [7. Orchestration: Server-Side Transitions](#7-orchestration-server-side-transitions)
  - [8. Failure, Timeout, and Degradation Semantics](#8-failure-timeout-and-degradation-semantics)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal adds **sandbox-level lifecycle hooks** to OpenSandbox: `preStart`,
`prePause`, `postResume`, `preTerminate`, and cron-driven `periodic` hooks. Hooks
are **declarative** in `CreateSandboxRequest.lifecycle` and (except `preStart`)
can be updated at runtime via `PATCH /sandboxes/{sandboxId}/lifecycle`. Execution
uses two channels: an **in-process channel** inside the sandbox (bootstrap.sh)
for `preStart`, and an **orchestrated channel** where the server drives execd
(`/v1/lifecycle/run`) for pause/resume/terminate transitions and periodic
scheduling. The contract is runtime-agnostic: identical behavior on Docker and
Kubernetes backends.

## Motivation

Agent sandboxes need to persist application or filesystem state before being
paused or terminated, and restore state after resume. OpenSandbox currently
provides task-level `preStart`/`postStop` hooks, but those are scoped to the
Kubernetes task-executor task model and are not available to the sandbox
lifecycle itself (pause/resume/terminate).

Related issues:

- [#1458](https://github.com/opensandbox-group/OpenSandbox/issues/1458) — Feature request for sandbox lifecycle hooks (`prePause`, `postResume`, `preTerminate`).
- [#1366](https://github.com/opensandbox-group/OpenSandbox/issues/1366) — Credentials need to be re-injected after resume.
- [#1355](https://github.com/opensandbox-group/OpenSandbox/issues/1355) — Filesystem state needs to be synchronized before pause and snapshot.
- [#1448](https://github.com/opensandbox-group/OpenSandbox/issues/1448) — Automatic idle-pause and resume-on-revisit.
- [openkruise/agents#743](https://github.com/openkruise/agents/issues/743) — Similar proposal for extensible sandbox lifecycle hooks.
- [kubernetes-sigs/agent-sandbox#1237](https://github.com/kubernetes-sigs/agent-sandbox/issues/1237) — In-sandbox lifecycle APIs around suspend, snapshot, and resume.

Key events the hooks must cover (TTL expiry, idle-pause, error paths) happen
**without the user being in the loop**; only declarative configuration can
guarantee a hook runs. This drives the declarative-first design.

### Goals

1. Provide sandbox-level hooks for `preStart`, `prePause`, `postResume`, `preTerminate`, and periodic (cron) execution.
2. Runtime-agnostic: the same contract and behavior on Docker and Kubernetes backends.
3. Declarative configuration at sandbox creation, plus runtime updates via a PATCH API for all hooks except `preStart`.
4. Guaranteed execution on platform-driven transitions (TTL expiry, idle-pause, error termination), not only user-initiated ones.
5. Configurable timeout and failure policy per hook; observable results.
6. Backward compatible: all hooks optional; sandboxes without hooks behave exactly as today.

### Non-Goals

1. **`postStart` / post-ready hooks** — the first-boot "sandbox ready" moment is not covered; `preStart` + `postResume` cover boot and resume.
2. **`preSnapshot` / `postSnapshot`** — the Kubernetes pause flow embeds a rootfs snapshot and is already covered by `prePause`; standalone snapshot hooks are deferred (recorded as future work).
3. **Server-side webhooks / notifications** (e.g. `postTerminate` events to an external URL) — out of scope; in-sandbox exec hooks only.
4. **Task-level hook unification** — the Kubernetes task-executor `preStart`/`postStop` (task scope) remain separate and unchanged.
5. **Kubernetes CRD schema changes** — hook config for K8s sandboxes is carried in a BatchSandbox annotation, not a new CRD field; the operator controller is not involved in hook execution.
6. **Windows sandboxes in v1** — the contract is platform-neutral, but v1 implementation targets Linux sandboxes.
7. **Cron expressions beyond standard 5-field cron + robfig descriptors** — no 6-field (second) cron, no timezone support in v1.
8. **Hook chaining/composition** (multiple commands per event, retries, conditional expressions).

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | Hooks are declared in `CreateSandboxRequest.lifecycle`; all hooks optional | Must |
| R2 | `prePause`, `postResume`, `preTerminate`, `periodic` are updatable at runtime via `PATCH /sandboxes/{sandboxId}/lifecycle`; `preStart` is rejected by PATCH | Must |
| R3 | `preStart` executes before the sandbox entrypoint starts, in-sandbox, without any server round trip | Must |
| R4 | Transition hooks run through the orchestrated channel: server → execd `/v1/lifecycle/run` | Must |
| R5 | Each hook has `timeoutSeconds` (default 60) and `failurePolicy` (`Abort` default for `prePause`/`postResume`, `Continue` default for `preTerminate`) | Must |
| R6 | A timed-out or failed hook with `failurePolicy: Abort` aborts the transition and leaves the sandbox in the pre-transition state with a machine-readable reason | Must |
| R7 | Hooks never block sandbox termination: when the sandbox is unreachable or not `Running`, transition hooks are skipped and recorded, never fatal | Must |
| R8 | `periodic` hooks run on a cron schedule inside the sandbox via an execd ticker, without server liveness dependency | Must |
| R9 | A `periodic` hook whose previous run is still in flight skips the current tick (no queueing) | Must |
| R10 | Hook config survives server restarts (Docker: container label; K8s: BatchSandbox annotation) | Must |
| R11 | PATCH affects only future transitions; an in-flight transition uses the config snapshot taken when it started | Must |
| R12 | Hook execution results are observable (execd records last run, exit code, consecutive failures) | Should |
| R13 | Pool-mode sandboxes support the same hook contract via the Pool pod template | Should |

## Proposal

### Hook Set

| Hook | Channel | Declarative | Runtime PATCH | Failure default | Fires |
|---|---|---|---|---|---|
| `preStart` | in-process (bootstrap.sh) | yes | **no** | Abort | before entrypoint starts, on every boot (including K8s resume) |
| `prePause` | orchestrated (server → execd) | yes | yes | Abort | before pause begins (manual, idle, or TTL-adjacent) |
| `postResume` | orchestrated (server → execd) | yes | yes | Abort | after runtime resume, before the sandbox returns to `Running` |
| `preTerminate` | orchestrated (server → execd) | yes | yes | Continue | before termination (delete or TTL), only when the sandbox is reachable |
| `periodic[]` | execd ticker | yes (injected) | yes (hot update) | Continue (always) | on cron schedule while the sandbox runs |

### Two Execution Channels

```
┌─────────────────────────────── sandbox lifecycle hooks ───────────────────────────────┐
│                                                                                        │
│  IN-PROCESS channel                    ORCHESTRATED channel                             │
│  (no external caller exists)           (server drives the transition)                  │
│                                                                                        │
│  Create: inject config (env)      │   Create: persist config (label/annotation)        │
│  bootstrap.sh reads OPEN_         │   PATCH: update config, push periodic hot-update    │
│   SANDBOX_LIFECYCLE.preStart      │                                                   │
│  → run hook → exec entrypoint     │   transition → server reads config snapshot        │
│                                   │   → POST execd /v1/lifecycle/run (timeout)         │
│  events: preStart                 │   → advance or abort transition                    │
│                                   │                                                    │
│                                   │   events: prePause, postResume, preTerminate,      │
│                                   │            periodic (execd ticker)                 │
└───────────────────────────────────┴────────────────────────────────────────────────────┘
```

Why two channels: `preStart` is a **process-ordering** semantic — the entrypoint
is launched by `bootstrap.sh` immediately after execd, with no window for a
server round trip; worker restarts and K8s resume (pod recreation) re-run it
with no server involvement. Transition hooks are **state-machine** semantics
owned by the server, which is the only component that sees both the stored hook
config and the lifecycle event. execd stays stateless: it executes commands and
runs a cron ticker; it never interprets lifecycle semantics itself.

### Notes/Constraints/Caveats

- **`preStart` cannot be PATCHed.** It is injected into the sandbox at creation and executed by `bootstrap.sh` before the entrypoint; by the time a PATCH could reach the sandbox, that boot is over. The PATCH API must reject `preStart` changes explicitly rather than silently accepting a never-effective value.
- **Paused-state termination skips `preTerminate`.** The spec allows `Paused → Stopping`. A paused Docker sandbox is frozen (execd unreachable); a paused K8s sandbox has no pod. The executor records "skipped (sandbox not reachable)" instead of failing the termination.
- **Idle-pause (#1448) reuses the same `prePause` hook** — it is the same transition, only triggered by the platform.
- **K8s TTL expiry deletes pods in the operator controller**, not in the server (see Risks; open implementation question in Design §7).

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Hook config is lost on server restart | Persist per provider: Docker container label, K8s BatchSandbox annotation (R10); both are re-read when the server re-discovers sandboxes |
| PATCH races with an in-flight transition | Snapshot semantics: the transition uses the config read when it started (R11) |
| Hook hangs forever | `timeoutSeconds` enforced at the execd runner (kill + error) and bounded by the transition state machine |
| `prePause` fails after the runtime has partially advanced | Ordering guarantees: hooks run to completion (or policy result) *before* the runtime pause/resume/delete call |
| K8s TTL termination bypasses the server (controller deletes the pod) | v1: server periodically scans near-expiry sandboxes and runs `preTerminate` best-effort before expiry; a controller-driven alternative is an open implementation question (§7) |
| execd unreachable during a transition (crashed pod, frozen container) | Skip + record (R7); the transition never blocks on a hook |
| Cron dependency | Standard `robfig/cron/v3` (K8s CronJob parser), vendored with execd |
| Hook commands run as the sandbox user and share the sandbox environment | Documented: hooks are in-sandbox by design; execd's credential env is already stripped from child processes (OSEP-0018 launcher) |
| Post-resume hook runs before the app inside the sandbox is truly ready | `postResume` fires after execd `/ping` succeeds, before the server marks the sandbox `Running`; app-level readiness is the hook's own responsibility |

## Design Details

> Code snippets are illustrative.

### 1. Public API: CreateSandboxRequest.lifecycle

Add a `lifecycle` object to `CreateSandboxRequest` in `specs/sandbox-lifecycle.yml`
(additive; omitted → no hooks, existing behavior unchanged):

```yaml
lifecycle:
  preStart:                      # declarative only; PATCH rejects it
    timeoutSeconds: 60           # optional, default 60
    command: ["/opt/hooks/pre-start.sh"]
  prePause:
    timeoutSeconds: 60
    failurePolicy: Abort         # Abort | Continue
    command: ["/opt/hooks/pre-pause.sh"]
  postResume:
    timeoutSeconds: 60
    failurePolicy: Abort
    command: ["/opt/hooks/post-resume.sh"]
  preTerminate:
    timeoutSeconds: 30
    failurePolicy: Continue      # default Continue for preTerminate
    command: ["/opt/hooks/pre-terminate.sh"]
  periodic:
    - name: checkpoint           # required; identity for dedup and observability
      schedule: "*/5 * * * *"    # standard 5-field cron; robfig descriptors (@hourly, @every 30s) allowed
      timeoutSeconds: 60
      command: ["/opt/hooks/checkpoint.sh"]
```

Schema notes:

- `command` is an argv array (no shell expansion; wrap in a script when needed), consistent with the task-executor `LifecycleHandler` (`kubernetes/pkg/task-executor/types.go`).
- `failurePolicy` enum: `Abort` | `Continue`. Defaults: `Abort` for `prePause`/`postResume`, `Continue` for `preTerminate`, `Continue` (fixed) for `periodic`.
- `periodic` is a list so multiple independent schedules (checkpoint, cleanup, heartbeat) can coexist; `name` must be unique within the sandbox.
- Event enum stays open: unknown/disabled events are ignored gracefully.

### 2. Public API: PATCH /sandboxes/{sandboxId}/lifecycle

New endpoint, JSON Merge Patch (RFC 7396), mirroring the existing metadata PATCH
pattern:

```
PATCH /sandboxes/{sandboxId}/lifecycle
{ "prePause": { "timeoutSeconds": 120, "failurePolicy": "Abort",
                "command": ["/opt/hooks/pre-pause.sh"] } }
→ 200 Sandbox (with updated lifecycle)
```

Semantics:

- Merge patch on the `lifecycle` object; `null` removes a hook; absent keys unchanged.
- **Rejects `preStart`** with 400 (`PreStartNotPatchable`) — it cannot take effect after creation.
- Applies to future transitions only; an in-flight transition keeps its start-time snapshot (R11).
- No optimistic locking in v1 (single-writer assumption, same caveat documented for metadata PATCH).
- Provider persistence: update the Docker container label / K8s BatchSandbox annotation; for `periodic`, push the new schedule to execd live (`POST /v1/lifecycle/periodic`, §4).

### 3. Config Persistence by Provider

There is no general server-side sandbox record today (only snapshot records), so
hook config survives server restarts by riding the existing per-provider state:

| Provider | Storage | Rationale |
|---|---|---|
| Docker | container label `sandbox.opensandbox.io/lifecycle` (JSON) | matches the existing label-based config pattern (`opensandbox.io/embedding-proxy-port`); re-read by `_restore_existing_sandboxes` |
| K8s | BatchSandbox annotation `sandbox.opensandbox.io/lifecycle` (JSON) | annotations are schemaless — no CRD change; the operator controller ignores the key entirely (server-only contract) |
| Both | in-sandbox env `OPEN_SANDBOX_LIFECYCLE` (JSON) at create | consumed by `bootstrap.sh` (`preStart`) and execd (periodic ticker startup); contains only `preStart` + `periodic` (the in-sandbox halves) |

The K8s annotation is added to the annotation contract list in
`kubernetes/AGENTS.md` when implemented; readers/writers must be updated
together.

### 4. execd Lifecycle API

New routes in execd (`pkg/web/router.go`), documented in `specs/execd-api.yaml`:

```
POST /v1/lifecycle/run          # run one command with timeout, wait for result
{
  "event": "prePause",          # context only (logging/telemetry)
  "command": ["/opt/hooks/pre-pause.sh"],
  "env": { "OPEN_SANDBOX_SANDBOX_ID": "sbx_..." },
  "timeoutSeconds": 60
}
→ 200 { "exitCode": 0, "stdout": "...", "stderr": "...", "durationMs": 1240 }
→ 504 { "error": "hook timeout" }          # process killed

POST /v1/lifecycle/periodic      # replace the periodic schedule (hot update on PATCH)
{ "periodic": [ { "name": "checkpoint", "schedule": "*/10 * * * *",
                  "timeoutSeconds": 60, "command": ["/opt/hooks/checkpoint.sh"] } ] }

GET  /v1/lifecycle/status        # per-hook state (observability; server may proxy)
{ "periodic": [ { "name": "checkpoint", "lastRunAt": "...", "lastExitCode": 0,
                  "consecutiveFailures": 0, "nextRunAt": "..." } ] }
```

Design notes:

- `run` reuses the existing command runtime machinery (`pkg/runtime/command.go`)
  in a synchronous, bounded mode; it does not reuse `/command`'s background +
  polling shape because hooks need synchronous completion with a hard deadline.
- execd keeps no lifecycle semantics: it does not know what "pause" means, only
  that a command must run with a timeout. The server supplies everything.
- Periodic scheduling is a `robfig/cron/v3` ticker inside execd (5-field cron +
  descriptors; container-local timezone). The ticker starts from the injected
  `OPEN_SANDBOX_LIFECYCLE` env at boot; `POST /v1/lifecycle/periodic` replaces
  the schedule live.
- Concurrency: one in-flight run per `name`; a tick that finds its previous run
  still active is skipped (no queueing). Docker pause freezes execd, so the
  ticker naturally suspends; K8s resume recreates the pod and the ticker
  restarts from the injected env.

### 5. In-process Channel: preStart

Today `bootstrap.sh` already supports `EXECD_BOOTSTRAP_PRE_SCRIPT` (sourced
env-preparation script) and the separate `opensandbox-supervisor` binary has
worker-level `--pre-start`/`--post-exit` hooks with timeouts. This OSEP adds a
structured `preStart` to the contract:

- At create, the server serializes `lifecycle.preStart` (plus `periodic`) into
  `OPEN_SANDBOX_LIFECYCLE` env on the container/pod.
- `bootstrap.sh` parses `OPEN_SANDBOX_LIFECYCLE.preStart` and, if present, runs
  it as a child process (exec semantics — not sourced; sourced env preparation
  stays with `EXECD_BOOTSTRAP_PRE_SCRIPT`) with a `timeout`-bounded wait, before
  starting execd and the entrypoint. Non-zero exit or timeout aborts the boot
  (the container start fails, matching `preStart` Abort semantics).
- When OSEP-0018 (execd as sandbox init) lands, execution moves into execd's
  launch path before the entrypoint is spawned; the contract is unchanged.

### 6. Periodic Hooks (cron)

- Scheduling lives **in execd** (in-sandbox ticker), not in the server — robust
  against server downtime, naturally suspended by Docker freeze, automatically
  restarted on K8s pod recreation.
- Initial schedule from injected env; runtime updates via PATCH → server →
  `POST /v1/lifecycle/periodic`.
- Failure is always `Continue`: a failed periodic run is recorded (status
  endpoint, consecutive-failure counter) and the schedule continues; a periodic
  hook failure never affects sandbox health or lifecycle transitions.
- `prePause` does **not** wait for in-flight periodic runs; the final flush is
  the `prePause` hook's own responsibility (documented separation).

### 7. Orchestration: Server-Side Transitions

The server (both providers) executes hooks around its existing transitions:

| Transition | Sequence |
|---|---|
| Pause (manual, idle, TTL-adjacent) | read config snapshot → `run` `prePause` → policy result → Docker `pause` / K8s patch `spec.pause=true` |
| Resume | Docker `unpause` / K8s patch `pause=false` → poll execd `/ping` (not pod readiness) → `run` `postResume` → success → sandbox state `Running` (abort → back to `Paused` + `ResumeFailed`-style reason) |
| Terminate (delete) | if sandbox reachable and `Running`: `run` `preTerminate` (Continue) → delete container/pod; else skip + record |
| K8s TTL expiry | controller deletes the pod; **v1**: server pre-scan of near-expiry sandboxes runs `preTerminate` best-effort before expiry. **Open implementation question**: alternatively hand the hook execution to the operator (controller calls execd), which would place hook orchestration in the controller for this path only |

State visibility for clients is unchanged: transitions already surface as
`Pausing`/`Resuming`/`Stopping` in `SandboxStatus`, and hook results are carried
in `reason`/`message` (and 409 on aborted transitions).

### 8. Failure, Timeout, and Degradation Semantics

- **Timeout**: enforced at the execd runner (kill on expiry, `504`), never
  counted as a success.
- **Abort** (`prePause`/`postResume` default): hook fails or times out →
  transition aborts, sandbox returns to its pre-transition state
  (`Running`/`Paused`), reason recorded (e.g. `prePause_hook_failed`). The user
  may retry the transition.
- **Continue** (`preTerminate` default, always for `periodic`): failure is
  recorded and the transition proceeds.
- **Unreachable sandbox** (`Paused`/`Failed`/pod gone): transition hooks are
  skipped with a recorded reason; termination never blocks on a hook.
- **Idempotency**: transition hooks are executed once per transition — the
  server's existing phase state machine (`Pausing`/`Resuming`/`Stopping`) and
  generation gating (K8s `PauseObservedGeneration`) already prevent re-entry;
  the hook itself should be idempotent (documented contract), since a failed
  `Abort` transition may be retried.

## Test Plan

**Unit**

- Schema validation: `lifecycle` shapes, `periodic.name` uniqueness, cron
  parsing (valid/invalid schedules), PATCH merge semantics, `preStart` PATCH
  rejection (400).
- execd `run`: timeout kill, output capture, exit-code propagation, env
  injection.
- execd periodic: cron scheduling (descriptor + 5-field), in-flight skip
  (slow hook), hot schedule replacement, consecutive-failure recording.
- Failure-policy resolution: Abort/Continue per event and per default.

**Integration**

- `bootstrap.sh` runs `preStart` before execd/entrypoint (marker-file ordering
  test); non-zero exit aborts boot; exec semantics (no env pollution).
- Server orchestration against a fake execd: pause/resume/terminate sequences
  fire hooks in order, abort paths return the sandbox to the pre-transition
  state, unreachable sandbox skips + records.

**E2E (Kind for K8s, Docker for the Docker runtime)**

- Docker: `prePause` writes a marker before `docker pause`; `postResume` runs
  after `unpause`; `preTerminate` runs before delete; periodic hook writes
  checkpoints on schedule; pause freezes the ticker.
- K8s: `prePause` before `spec.pause=true`; `postResume` after pod Ready +
  execd `/ping`, before `Running`; resume-from-snapshot re-runs `preStart`.
- TTL termination with a near-expiry `preTerminate` (best-effort path).
- PATCH mid-flight: transition uses its start snapshot; a PATCH lands for the
  next transition.
- Server restart: hook config restored from container label / BatchSandbox
  annotation; periodic schedule resumes.

## Drawbacks

1. **Contract surface grows** (`CreateSandboxRequest.lifecycle` + PATCH endpoint + execd API) — mitigated by keeping every field optional and additive.
2. **execd takes on a scheduler** — cron ticker + lifecycle run endpoint extend the in-sandbox control plane; isolated from user code by the existing access token and (eventually) OSEP-0018 hardening.
3. **Two execution channels** — more places for behavior to drift; the contract explicitly maps each event to exactly one channel to avoid ambiguity.
4. **K8s TTL path** cannot guarantee `preTerminate` without server pre-scanning or controller involvement (open implementation question).
5. **Hooks are in-sandbox code** — a buggy hook can slow transitions (bounded by timeout) or break boot (`preStart`); operators must keep hooks small and idempotent.

## Alternatives

1. **Runtime-only hooks (server POSTs commands at transition, no spec config)** — rejected: TTL expiry, idle-pause (#1448), and error paths have no caller present, so hooks would silently not run; the feature's core use cases would be uncovered.
2. **All hooks in-sandbox (execd interprets lifecycle events itself)** — rejected: execd cannot observe pause requests (Docker freeze is external; K8s pause is a server-side CR patch); the server is the only component with both config and event visibility.
3. **Hooks in the Kubernetes task-executor only** — rejected: not runtime-agnostic (Docker sandboxes have no task-executor); task hooks are process-scoped, not lifecycle-scoped.
4. **Kubernetes-native `preStop` container hooks** — rejected: bounded by the pod termination grace period, cannot express post-resume or pause behavior, and does not exist in the Docker runtime.
5. **Server-side timers for periodic hooks** — rejected: server downtime stops checkpoints, per-sandbox timers do not scale, and Docker pause would still need client-side suppression; the in-sandbox ticker is simpler and robust.

## Infrastructure Needed

- `robfig/cron/v3` added to `components/execd` (vendored, matching the repo's vendoring convention).
- No new services, storage, or third-party infrastructure.

## Upgrade & Migration Strategy

**Backward compatible.** All new fields are optional; sandboxes without
`lifecycle` behave exactly as today. No existing endpoint or schema is changed;
no CRD schema change (K8s config rides a new, controller-ignored annotation).

**Phased rollout:**

1. execd lifecycle API (`run`, `periodic`, `status`) + unit/integration tests; execd image release.
2. `specs/sandbox-lifecycle.yml` `lifecycle` field + PATCH endpoint; server orchestration in the Docker provider; e2e.
3. K8s provider orchestration + annotation persistence; e2e (pause/resume/TTL).
4. `preStart` in `bootstrap.sh` (env injection) on both providers.
5. Pool-mode parity (pool pod template carries the injected env; hook config via the Pool CRD template).

**Docs**: `docs/` lifecycle hook guide (per hook, failure policies, idempotency
contract, cron reference), `docs/kubernetes/` annotation contract note, SDK
regeneration for the new `lifecycle` field and PATCH endpoint.
