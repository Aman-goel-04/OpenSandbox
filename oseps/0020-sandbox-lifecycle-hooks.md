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
| R4 | `prePause` and `postResume` are **triggered** through the orchestrated channel: the server POSTs an event-only trigger to execd (`/v1/lifecycle/run`, no command in the request); execd resolves the command from its in-sandbox config file. `preTerminate` runs in-sandbox via SIGTERM — no server in the path | Must |
| R5 | Each hook has `timeoutSeconds` (default 60) and `failurePolicy` (`Abort` default for `prePause`/`postResume`; `preTerminate` and `periodic` are fixed `Continue` — `Abort` is rejected for them) | Must |
| R6 | A timed-out or failed hook with `failurePolicy: Abort` aborts the transition and leaves the sandbox in the pre-transition state with a machine-readable reason | Must |
| R7 | Hooks never block sandbox **termination**: `preTerminate` runs only while the sandbox is `Running`; when the sandbox is not `Running` (e.g. `Paused`), no SIGTERM handler runs and termination proceeds. This fail-open rule is termination-only — see R6/R18 for orchestrated hooks | Must |
| R18 | For orchestrated hooks (`prePause`/`postResume`) with `Abort`, **execd unreachability is treated as hook failure**: a nominally `Running` sandbox whose execd cannot be reached aborts the transition (R6), never silently continues; only sandboxes that are legitimately not `Running` (`Paused`/`Failed`/pod gone) skip the hook | Must |
| R8 | `periodic` hooks run on a cron schedule inside the sandbox via an execd ticker, without server liveness dependency | Must |
| R9 | A `periodic` hook whose previous run is still in flight skips the current tick (no queueing) | Must |
| R10 | Hook config survives server restarts (Docker: file-backed store; K8s: BatchSandbox/AgentSandbox annotation) | Must |
| R11 | PATCH affects only future transitions; an in-flight transition uses the config snapshot taken when it started | Must |
| R12 | Hook execution results are observable (execd records last run, exit code, consecutive failures) | Should |
| R13 | Pool-mode sandboxes support the same hook contract via the Pool pod template | Should |
| R14 | The effective lifecycle config is **persisted inside the sandbox** (a config file on the sandbox filesystem), not kept only in memory: execd restarts and pause/resume cycles (including K8s rootfs-snapshot resume) must preserve runtime-PATCHed schedules and hooks | Must |
| R15 | Termination grace must cover the `preTerminate` hook: the server sets the K8s pod `terminationGracePeriodSeconds` / Docker `stop` timeout to `preTerminate.timeoutSeconds` + buffer, so SIGKILL never cuts the hook off | Must |
| R16 | Lifecycle PATCH is rejected when the sandbox is not `Running` (409) — a paused/failed sandbox cannot receive the live execd update and deferred application would leave persisted config and live schedule inconsistent | Must |
| R17 | Lifecycle configuration requires execd lifecycle-API capability: create/PATCH is rejected for sandboxes running an execd without the lifecycle endpoints, so an `Abort`-default hook never 404s mid-transition on an old daemon | Must |

## Proposal

### Hook Set

| Hook | Channel | Declarative | Runtime PATCH | Failure default | Fires |
|---|---|---|---|---|---|
| `preStart` | in-process (bootstrap.sh) | yes | **no** | Abort | before entrypoint starts, on every boot (including K8s resume) |
| `prePause` | orchestrated (server → execd) | yes | yes | Abort | before pause begins (manual, idle, or TTL-adjacent) |
| `postResume` | orchestrated (server → execd) | yes | yes | Abort | after runtime resume, before the sandbox returns to `Running` |
| `preTerminate` | **in-sandbox signal-driven (execd catches SIGTERM)** | yes | yes | Continue (only; `Abort` rejected) | on any platform termination of a `Running` sandbox (delete, TTL, eviction, `docker stop`) |
| `periodic[]` | execd ticker | yes (injected) | yes (hot update) | Continue (always) | on cron schedule while the sandbox runs |

### Execution Channels

```
┌─────────────────────────────── sandbox lifecycle hooks ───────────────────────────────┐
│                                                                                        │
│  IN-SANDBOX channels                        ORCHESTRATED channel                        │
│  (self-contained; no server in the path)    (server drives the transition)             │
│                                                                                        │
│  process-ordering:                 │   Create: persist config (label/annotation)       │
│   bootstrap.sh materializes        │   PATCH: update config, push live update          │
│   OPEN_SANDBOX_LIFECYCLE →         │   transition → server reads config snapshot       │
│   /var/execd/lifecycle.toml        │   → POST execd /v1/lifecycle/run (timeout)        │
│   → run preStart → exec entrypoint │   → advance or abort transition                   │
│                                   │                                                    │
│  signal-driven:                    │   events: prePause, postResume                    │
│   execd catches SIGTERM →          │                                                    │
│   run preTerminate → shutdown      │                                                    │
│                                   │                                                    │
│  timer-driven:                     │                                                    │
│   execd cron ticker → periodic     │                                                    │
└───────────────────────────────────┴────────────────────────────────────────────────────┘
```

Why multiple channels: `preStart` is a **process-ordering** semantic — the
entrypoint is launched by `bootstrap.sh` immediately after execd, with no window
for a server round trip; worker restarts and K8s resume (pod recreation) re-run
it with no server involvement. `preTerminate` is **signal-driven**: every
platform termination path (K8s pod delete for TTL expiry, user delete,
eviction, node drain; Docker `docker stop`) ends in a SIGTERM to the container,
and `bootstrap.sh` already forwards it to execd — so execd runs the hook
itself, with no server in the path, which makes TTL- and eviction-triggered
termination as reliable as user-initiated ones. The remaining transition hooks
are **state-machine** semantics owned by the server, which is the only
component that sees both the stored hook config and the lifecycle event. execd
executes commands, runs the cron ticker, and handles SIGTERM; it never
interprets pause/resume semantics itself.

### Notes/Constraints/Caveats

- **`preStart` cannot be PATCHed.** It is injected into the sandbox at creation and executed by `bootstrap.sh` before the entrypoint; by the time a PATCH could reach the sandbox, that boot is over. The PATCH API must reject `preStart` changes explicitly rather than silently accepting a never-effective value.
- **Paused-state termination skips `preTerminate`.** The spec allows `Paused → Stopping`. A paused Docker sandbox is frozen (its processes do not respond to signals); a paused K8s sandbox has no pod and therefore no execd at all. In both cases no SIGTERM handler runs, which naturally matches the "skipped, not fatal" semantics — termination is unaffected.
- **Idle-pause (#1448) reuses the same `prePause` hook** — it is the same transition, only triggered by the platform.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Hook config is lost on server restart | Persist per provider: Docker container label, K8s BatchSandbox annotation (R10); both are re-read when the server re-discovers sandboxes |
| PATCH races with an in-flight transition | Snapshot semantics: the transition uses the config read when it started (R11) |
| Hook hangs forever | `timeoutSeconds` enforced at the execd runner (kill + error) and bounded by the transition state machine |
| `prePause` fails after the runtime has partially advanced | Ordering guarantees: hooks run to completion (or policy result) *before* the runtime pause/resume/delete call |
| SIGKILL cuts `preTerminate` off (grace too short) | R15: server sets `terminationGracePeriodSeconds` (K8s) / `stop` timeout (Docker) to `preTerminate.timeoutSeconds` + buffer; execd treats grace remaining as the hard deadline |
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
    command: ["/opt/hooks/pre-terminate.sh"]   # failurePolicy not allowed; fixed Continue
  periodic:
    - name: checkpoint           # required; identity for dedup and observability
      schedule: "*/5 * * * *"    # standard 5-field cron; robfig descriptors (@hourly, @every 30s) allowed
      timeoutSeconds: 60
      command: ["/opt/hooks/checkpoint.sh"]
```

Schema notes:

- `command` is an argv array (no shell expansion; wrap in a script when needed), consistent with the task-executor `LifecycleHandler` (`kubernetes/pkg/task-executor/types.go`).
- `failurePolicy` enum: `Abort` | `Continue`. Defaults: `Abort` for `prePause`/`postResume`. `preTerminate` and `periodic` are **fixed `Continue`** — supplying `Abort` is a validation error, because a signal-driven pre-termination hook is inherently best-effort (SIGKILL follows the grace period regardless) and a periodic failure must never affect the sandbox.
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
- **Rejects PATCH on a non-`Running` sandbox** with 409 (`SandboxNotRunning`): a paused/failed sandbox cannot receive the live execd update, and deferred application would leave the persisted file and the live schedule inconsistent (Docker frozen / K8s pod gone). The client patches after resume.
- **Rejects `failurePolicy: Abort` for `preTerminate`/`periodic`** with 400 (fixed `Continue`).
- **Bounds `preTerminate.timeoutSeconds` to the provisioned grace** (R15): the pod's `terminationGracePeriodSeconds` / Docker `stop` timeout is set at creation from the then-current `preTerminate` timeout. A PATCH that raises the timeout beyond `grace - buffer` is rejected with 400 (`GraceTooSmall`) — TTL deletion, eviction, and node drain use the pod's original grace and bypass the server, so a later increase could never be honored.
- Applies to future transitions only; an in-flight transition keeps its start-time snapshot (R11).
- No optimistic locking in v1 (single-writer assumption, same caveat documented for metadata PATCH).
- Provider persistence: Docker file-backed store / K8s workload CR annotation; for in-sandbox effects, push the updated config to execd live (`POST /v1/lifecycle/config`, atomically persisted by execd); `preTerminate` needs no push at transition time — execd reads the file fresh on SIGTERM, and the live push keeps that file current.
- **Capability gate**: creating or PATCHing lifecycle config requires the sandbox's execd to support the lifecycle API (versioned `/v1/lifecycle/capabilities` or the existing capabilities endpoint extended); sandboxes running an older execd image reject lifecycle configuration, so `Abort`-default hooks never 404 on an old daemon mid-transition.

### 3. Config Persistence by Provider

There is no general server-side sandbox record today (only snapshot records), so
hook config survives server restarts by riding the existing per-provider state:

| Provider | Storage | Rationale |
|---|---|---|
| Docker | **file-backed store**, same mechanism metadata/expiration already use (`services/docker/metadata.py`) | Docker labels on running containers are **immutable** — PATCH cannot update them; the existing file-backed store is the established writable pattern |
| K8s | BatchSandbox **or AgentSandbox** annotation `sandbox.opensandbox.io/lifecycle` (JSON), whichever workload CR the configured provider manages (`provider_factory.py` registers both) | annotations are schemaless — no CRD change; the operator controller ignores the key entirely (server-only contract) |
| Both | in-sandbox env `OPEN_SANDBOX_LIFECYCLE` (TOML content) at create | **transport only**: `bootstrap.sh` materializes it to `/var/execd/lifecycle.toml` on first provision (if absent); execd reads only the file. Contains all five in-sandbox hooks: `preStart`, `prePause`, `postResume`, `preTerminate` (read by the SIGTERM handler), and `periodic` |

The K8s annotation is added to the annotation contract list in
`kubernetes/AGENTS.md` when implemented; readers/writers must be updated
together.

### 4. execd Lifecycle API

New routes in execd (`pkg/web/router.go`), documented in `specs/execd-api.yaml`:

```
POST /v1/lifecycle/run          # trigger a transition hook by event; execd resolves the command
{ "event": "prePause" }         # from its config file (no command/env in the request)
→ 200 { "executed": true,  "exitCode": 0, "stdout": "...", "stderr": "...", "durationMs": 1240 }
→ 200 { "executed": true,  "exitCode": 1, "stdout": "...", "stderr": "...", "durationMs": 320 }  # hook failed
→ 504 { "executed": false, "reason": "hook timeout" }            # killed by the file's timeout
→ 200 { "executed": false, "reason": "not_configured" }          # no hook for this event
→ 200 { "executed": false, "reason": "config_unavailable" }      # file missing/unreadable

POST /v1/lifecycle/config       # replace the whole in-sandbox hook config (hot update on PATCH)
{ "prePause": { "command": ["/opt/hooks/pre-pause.sh"], "timeout_seconds": 60 },
  "preTerminate": { "command": ["/opt/hooks/pre-terminate.sh"], "timeout_seconds": 30 },
  "periodic": [ { "name": "checkpoint", "schedule": "*/10 * * * *",
                  "timeoutSeconds": 60, "command": ["/opt/hooks/checkpoint.sh"] } ] }

GET  /v1/lifecycle/status        # per-hook state (observability; server may proxy)
{ "periodic": [ { "name": "checkpoint", "lastRunAt": "...", "lastExitCode": 0,
                  "consecutiveFailures": 0, "nextRunAt": "..." } ] }
```

Design notes:

- `run` reuses the existing command runtime machinery (`pkg/runtime/command.go`)
  in a synchronous, bounded mode; it does not reuse `/command`'s background +
  polling shape because hooks need synchronous completion with a hard deadline.
  It serves only the orchestrated transition hooks (`prePause`/`postResume`).
- **The request carries only the event — never the command.** execd resolves
  the hook (command, timeout, env) from its config file, which is the in-sandbox
  authority for all hooks; the server stores config only for validation,
  display, and re-provisioning, and its transition logic does not embed hook
  commands. This makes every hook follow the same file-read pattern
  (`preStart`, `preTerminate` on SIGTERM, `periodic` ticks, and now
  `prePause`/`postResume` triggers), with a single source of truth inside the
  sandbox and no drift between annotation, request body, and file.
- **`executed: false` is an explicit, differentiated outcome.** The server
  compares it against its own config knowledge: a hook it believes is
  configured reporting `not_configured`/`config_unavailable` is treated as hook
  failure (R18) — the configured state flush must not silently vanish.
- execd keeps no lifecycle semantics: it does not know what "pause" means, only
  that a command from its config must run with a timeout. The server supplies
  the event.
- **`preTerminate` has no endpoint**: execd installs a SIGTERM handler (its
  existing signal-notify path, `main.go`) that runs the `preTerminate` command
  from the config file and then exits — see Design §9.
- Periodic scheduling is a `robfig/cron/v3` ticker inside execd (5-field cron +
  descriptors; container-local timezone). The ticker starts from the injected
  `OPEN_SANDBOX_LIFECYCLE` env at boot; `POST /v1/lifecycle/config` replaces
  the whole in-sandbox config (including `preTerminate`) live.
- **Config is persisted inside the sandbox, never memory-only, and never as a
  monolithic daemon-state blob.** execd follows one-concern-one-file
  persistence under `/var/execd/`: the lifecycle config is a dedicated
  **TOML config file** (`/var/execd/lifecycle.toml`) holding only the hook
  definitions (desired state — small, low-frequency writes). TOML matches
  execd's existing config convention (`--isolation-config` /
  `EXECD_ISOLATION_CONFIG`, `configs/isolation.example.toml`), but is a
  **separate per-sandbox file**: the isolation TOML is a static
  operator/image-level policy and is not runtime-PATCHable, while this file is
  per-sandbox and updated by PATCH. It is written atomically (temp file +
  rename) and carries a `version` key for future migration:

  ```toml
  # /var/execd/lifecycle.toml
  version = 1

  [preStart]
  command = ["/opt/hooks/pre-start.sh"]
  timeout_seconds = 60

  [prePause]
  command = ["/opt/hooks/pre-pause.sh"]
  timeout_seconds = 60

  [postResume]
  command = ["/opt/hooks/post-resume.sh"]
  timeout_seconds = 60

  [preTerminate]
  command = ["/opt/hooks/pre-terminate.sh"]
  timeout_seconds = 30

  [[periodic]]
  name = "checkpoint"
  schedule = "*/5 * * * *"
  command = ["/opt/hooks/checkpoint.sh"]
  timeout_seconds = 60
  ```

  `/var/execd` is deliberately **not** any mounted volume — it is
  not the K8s `opensandbox-bin` emptyDir (`/opt/opensandbox`, wiped on pod
  recreation) nor the isolation volume (`/var/lib/execd/isolation`), so the
  file lives in the writable layer that is committed into the K8s rootfs
  snapshot on pause and restored on resume. `bootstrap.sh` creates the
  directory when it can (root images). For **non-root images** (which cannot
  create directories under `/var`, e.g. the `examples/chrome` and
  `examples/vscode` images), execd resolves a writable state directory at
  startup with the same order `bootstrap.sh` uses, so the boot path and execd
  always agree: try `/var/execd`, fall back to `$HOME/.opensandbox` — both on
  the container rootfs, so the K8s snapshot guarantee holds either way. On
  startup execd loads **persisted file first, injected env as fallback** —
  `bootstrap.sh` writes the file only when it is absent (first provision), so
  a runtime-PATCHed schedule survives:
  - **execd restarts** — the updated schedule is reloaded from the file.
  - **Docker pause/resume** — the container rootfs persists, config intact.
  - **K8s pause/resume** — the pod is recreated from the rootfs snapshot,
    which contains the config file, so PATCHed schedules survive resume; the
    creation-time env only serves as the boot default on first provision.
  The server-side file-backed store/annotation remains the source of truth for
  re-provisioning; the in-sandbox file is the runtime authority that keeps
  execd self-sufficient across restarts and resume.

  **General execd persistence principles (apply to any future daemon state):**
  - **Config files are TOML.** execd's existing config surface is TOML
    (`--isolation-config`); new config files follow the same format and
    parsing conventions, never JSON.
  - **Config vs. runtime state are separated.** Config (desired state, e.g.
    lifecycle hooks) is persisted; runtime state that is rebuildable (hook run
    results, `lastRunAt`/`consecutiveFailures` counters, session records)
    stays in memory and may be reset on restart — `GET /v1/lifecycle/status`
    reflects the current execd process only.
  - **One concern = one file or directory.** Variable-cardinality data uses
    one file per entity (`/var/execd/sessions/<id>.json`), never a JSON array
    that is rewritten on every change; events use append-only JSONL with
    rotation (reusing the `lumberjack` setup from `internal/supervisor`), not
    a JSON file that is rewritten per append.
  - **Load is best-effort and fail-open.** A missing, malformed, or
    unsupported-version config file is logged and falls back to the injected
    env (or defaults); it never blocks execd startup or the sandbox boot,
    matching OSEP-0018's fail-open stance.
- Concurrency: one in-flight run per `name`; a tick that finds its previous run
  still active is skipped (no queueing). Docker pause freezes execd, so the
  ticker naturally suspends; K8s resume recreates the pod and the ticker
  restarts from the injected env.

### 5. In-process Channel: preStart

Today `bootstrap.sh` already supports `EXECD_BOOTSTRAP_PRE_SCRIPT` (sourced
env-preparation script) and the separate `opensandbox-supervisor` binary has
worker-level `--pre-start`/`--post-exit` hooks with timeouts. This OSEP adds a
structured `preStart` to the contract:

- At create, the server serializes the full in-sandbox lifecycle (all five
  hooks) as TOML into the `OPEN_SANDBOX_LIFECYCLE` env on the container/pod;
  `bootstrap.sh` materializes it to `/var/execd/lifecycle.toml` when the file
  does not exist yet (first provision), so execd and the boot path read the
  file only.
- `bootstrap.sh` parses `/var/execd/lifecycle.toml` `[preStart]` and, if
  present, runs it as a child process (exec semantics — not sourced; sourced
  env preparation stays with `EXECD_BOOTSTRAP_PRE_SCRIPT`) with a
  `timeout`-bounded wait, before starting execd and the entrypoint. Non-zero
  exit or timeout aborts the boot (the container start fails, matching
  `preStart` Abort semantics).
- When OSEP-0018 (execd as sandbox init) lands, execution moves into execd's
  launch path before the entrypoint is spawned; the contract is unchanged.

### 6. Periodic Hooks (cron)

- Scheduling lives **in execd** (in-sandbox ticker), not in the server — robust
  against server downtime, naturally suspended by Docker freeze, automatically
  restarted on K8s pod recreation.
- Initial schedule from injected env; runtime updates via PATCH → server →
  `POST /v1/lifecycle/config`. Both the boot config and every runtime update
  are **persisted to the in-sandbox config file** (§4): the ticker on a
  restarted execd (or a resumed sandbox) reflects the latest PATCHed schedule,
  never just the creation-time env or an in-memory copy.
- Failure is always `Continue`: a failed periodic run is recorded (status
  endpoint, consecutive-failure counter) and the schedule continues; a periodic
  hook failure never affects sandbox health or lifecycle transitions.
- `prePause` does **not** wait for in-flight periodic runs; the final flush is
  the `prePause` hook's own responsibility (documented separation).

### 7. Orchestration: Server-Side Transitions

The server (both providers) executes hooks around its existing transitions.
`preTerminate` is **not** orchestrated — see Design §9 (signal-driven):

| Transition | Sequence |
|---|---|
| Pause (manual, idle, TTL-adjacent) | trigger `prePause` (`POST /v1/lifecycle/run {event}`) → execd runs its file-resolved hook → policy result → Docker `pause` / K8s patch `spec.pause=true` |
| Resume | Docker `unpause` / K8s patch `pause=false` → wait for pod re-creation + execd `/ping` → trigger `postResume` → success → public state `Running`; abort → roll back to `Paused` |
| Terminate (user delete, Docker `stop`) | server issues the delete; the runtime sends SIGTERM into the sandbox; execd runs `preTerminate` (if `Running`) and shuts down. Server only ensures the termination grace covers the hook timeout (R15) |
| K8s TTL expiry / eviction / node drain | controller/kubelet deletes the pod; kubelet SIGTERMs the container; execd runs `preTerminate` — **no server involvement, no pre-scan** |

**K8s resume state gating.** The BatchSandbox controller sets the CR phase to
`Succeed` as soon as the recreated pod is ready (`continueResume` returns to
normal reconciliation), independently of the server-side `postResume` call —
the CR phase therefore cannot express "resuming, hooks in progress". The
**server** owns the public `SandboxState` mapping and gates it explicitly:
while a resume is in flight the server keeps reporting `Resuming` until
`postResume` completes, then reports `Running` (the server marks the resume
intent in its per-sandbox state, e.g. the same file-backed store/annotation
that holds the hook config). On `Abort`, the server re-patches `spec.pause=true`
so the controller re-pauses the sandbox and the public state returns to
`Paused` — no controller/CRD change is needed.

State visibility for clients is unchanged: transitions already surface as
`Pausing`/`Resuming`/`Stopping` in `SandboxStatus`, and hook results are carried
in `reason`/`message` (and 409 on aborted transitions).

### 8. Failure, Timeout, and Degradation Semantics

- **Timeout**: enforced at the execd runner (kill on expiry, `504`), never
  counted as a success. For `preTerminate`, the effective deadline is
  `min(timeoutSeconds, termination grace remaining)` — SIGKILL after the grace
  period is the hard backstop (R15).
- **Abort** (`prePause`/`postResume` default): hook fails or times out →
  transition aborts, sandbox returns to its pre-transition state
  (`Running`/`Paused`), reason recorded (e.g. `prePause_hook_failed`). The user
  may retry the transition.
- **Continue** (`preTerminate` and `periodic`; `Abort` rejected for both):
  failure is recorded and the transition proceeds. For `preTerminate` this is
  inherent — SIGKILL follows the grace period regardless, so the hook can only
  ever be best-effort.
- **Unreachable sandbox — two distinct cases** (R7/R18):
  - Sandbox is legitimately not `Running` (`Paused`/`Failed`/pod gone): no
    SIGTERM handler runs (`preTerminate`), and orchestrated hooks are skipped
    with a recorded reason — termination never blocks on a hook.
  - Sandbox state is `Running` but execd cannot be reached (crashed or
    unresponsive daemon): for `Abort`-policy orchestrated hooks this is
    **hook failure** — the transition aborts (R6/R18) instead of silently
    proceeding without the configured state flush.
- **Idempotency**: transition hooks are executed once per transition — the
  server's existing phase state machine (`Pausing`/`Resuming`/`Stopping`) and
  generation gating (K8s `PauseObservedGeneration`) already prevent re-entry;
  the hook itself should be idempotent (documented contract), since a failed
  `Abort` transition may be retried. A `preTerminate` hook runs at most once
  per container lifetime: execd runs it on SIGTERM and exits immediately
  after.

### 9. Signal-Driven preTerminate

Every platform termination path converges on the same primitive — a SIGTERM
sent to the sandbox container:

- **K8s** (TTL expiry, user delete, eviction, node drain): kubelet SIGTERMs
  the container's PID 1 during pod deletion.
- **Docker** (`docker stop` from the server or an operator): SIGTERM to the
  container's PID 1.

Today the container PID 1 is `bootstrap.sh`, which already traps TERM and
forwards it to execd (`bootstrap.sh` `_shutdown_children`); under OSEP-0018
execd becomes PID 1 and receives the external SIGTERM directly. In both
topologies execd sees the signal:

```go
// execd main.go: extend the existing signal-notify path
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
go func() {
    <-ctx.Done()
    if hook := lifecycle.Load("/var/execd/lifecycle.toml").PreTerminate; hook != nil {
        runHook(hook, timeUntilGraceDeadline())   // reads the file fresh, not a cached copy
    }
    shutdownServerAndExit()
}()
```

Guarantees and properties:

1. **Covers all platform-driven terminations, with no server in the path** —
   TTL expiry (controller deletes the pod while the server may be down),
   eviction, node drain, user delete, and `docker stop` are equivalent.
2. **`Running`-only by construction**: a `Paused` Docker sandbox is frozen
   (does not respond to signals) and a `Paused` K8s sandbox has no pod — no
   handler runs, termination proceeds (R7).
3. **Inherently best-effort (fixed `Continue`)**: SIGKILL follows the grace
   period no matter what, so the hook cannot block termination and `Abort` is
   rejected for it.
4. **Grace period must cover the hook** (R15): the server sets K8s pod
   `terminationGracePeriodSeconds` and the Docker `stop` timeout to
   `preTerminate.timeoutSeconds` + buffer (default 30 s or configured value);
   execd treats the grace deadline as the hard cap even if the hook's own
   timeout is larger.
5. **Runs concurrently with the app's own shutdown** in today's topology
   (`bootstrap.sh` forwards TERM to both execd and the user command): the hook
   contract is state flush, not application coordination.
6. **Config is read fresh at signal time** from `/var/execd/lifecycle.toml`
   (never a cached copy), so the latest PATCHed definition applies.
7. **OSEP-0018 interaction**: when execd becomes PID 1, it must distinguish
   the *external* container-stop SIGTERM from an in-namespace `kill 1` by a
   workload process (an open item already tracked in OSEP-0018 §3); the v1
   topology (bootstrap forwards only trusted external TERM to execd) is
   unaffected.

## Test Plan

**Unit**

- Schema validation: `lifecycle` shapes, `periodic.name` uniqueness, cron
  parsing (valid/invalid schedules), PATCH merge semantics, `preStart` PATCH
  rejection (400).
- execd `run`: timeout kill, output capture, exit-code propagation, env
  injection.
- execd SIGTERM handler: reads `[preTerminate]` from the config file, runs it
  with the grace-bounded deadline, exits after; no handler when the hook is
  absent; `kill 1` from an in-namespace process is ignored in the v1 topology
  (execd receives TERM only via bootstrap forwarding).
- execd periodic: cron scheduling (descriptor + 5-field), in-flight skip
  (slow hook), hot schedule replacement, consecutive-failure recording.
- execd lifecycle config file: TOML parse (valid/invalid/unsupported
  `version`), atomic write (temp + rename), startup load order (persisted file
  wins over injected env), malformed-file fail-open fallback.
- Failure-policy resolution: Abort/Continue per event and per default.

**Integration**

- `bootstrap.sh` runs `preStart` before execd/entrypoint (marker-file ordering
  test); non-zero exit aborts boot; exec semantics (no env pollution).
- Server orchestration against a fake execd: pause/resume/terminate sequences
  fire hooks in order, abort paths return the sandbox to the pre-transition
  state, unreachable sandbox skips + records.

**E2E (Kind for K8s, Docker for the Docker runtime)**

- Docker: `prePause` writes a marker before `docker pause`; `postResume` runs
  after `unpause`; periodic hook writes checkpoints on schedule; pause freezes
  the ticker; `preTerminate` runs on `docker stop` (marker file in the
  container rootfs).
- K8s: `prePause` before `spec.pause=true`; `postResume` after pod Ready +
  execd `/ping`, before `Running`; resume-from-snapshot re-runs `preStart`.
- K8s termination paths: TTL expiry, `kubectl delete` pod, and eviction all
  produce the `preTerminate` marker (signal-driven, no server in the path);
  a `Paused` sandbox deleted directly produces no marker and terminates
  normally.
- PATCH mid-flight: transition uses its start snapshot; a PATCH lands for the
  next transition.
- Server restart: hook config restored from container label / BatchSandbox
  annotation; periodic schedule resumes.
- **Config persistence across execd restart and pause/resume**: (1) execd
  process restart after a PATCH — the updated schedule is reloaded from the
  in-sandbox config file, not the creation-time env; (2) Docker pause/resume —
  the PATCHed schedule survives; (3) K8s pause/resume — the PATCHed schedule
  survives the rootfs-snapshot recreation (config file present in the snapshot
  image), and the ticker resumes with it.

## Drawbacks

1. **Contract surface grows** (`CreateSandboxRequest.lifecycle` + PATCH endpoint + execd API) — mitigated by keeping every field optional and additive.
2. **execd takes on a scheduler** — cron ticker + lifecycle run endpoint extend the in-sandbox control plane; isolated from user code by the existing access token and (eventually) OSEP-0018 hardening.
3. **Multiple execution channels** — in-process, signal-driven, and orchestrated; the contract explicitly maps each event to exactly one channel to avoid ambiguity.
4. **`preTerminate` is bounded by the termination grace period** — a hook longer than `terminationGracePeriodSeconds - buffer` is cut off by SIGKILL; the server must set the grace accordingly (R15) and hooks must stay small.
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

1. execd lifecycle API (`run` for `prePause`/`postResume`, `periodic`, `status`) + SIGTERM `preTerminate` handler + unit/integration tests; execd image release.
2. `specs/sandbox-lifecycle.yml` `lifecycle` field + PATCH endpoint; server orchestration in the Docker provider (including `stop` timeout ≥ hook timeout, R15); e2e.
3. K8s provider orchestration + annotation persistence + `terminationGracePeriodSeconds` wiring (R15); e2e (pause/resume/TTL/eviction).
4. `preStart` in `bootstrap.sh` (env injection) on both providers.
5. Pool-mode parity (per-sandbox injection via the existing task/alloc path, e.g. `spec.taskTemplate` env — the Pool pod template is shared across allocations and cannot carry per-sandbox hook config).

**Docs**: `docs/` lifecycle hook guide (per hook, failure policies, idempotency
contract, cron reference), `docs/kubernetes/` annotation contract note, SDK
regeneration for the new `lifecycle` field and PATCH endpoint.
