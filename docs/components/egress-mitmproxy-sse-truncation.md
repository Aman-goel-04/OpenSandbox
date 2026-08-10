---
title: Egress Mitmproxy SSE Truncation
description: Known mitmproxy bug where large SSE / streamed bodies are truncated at the tail when the TLS HTTP/1.1 upstream closes the connection right after the body, with reproduction scripts and status.
---

# Egress Mitmproxy: Large SSE Chunks Truncated at the Tail

## Symptom

Sandboxes calling an LLM API through the egress sidecar (transparent mitmproxy mode) occasionally receive a **truncated SSE stream**: a single `data:` event larger than ~1 MB is cut off near the end, and the SDK reports malformed JSON or a dropped connection.

The truncation point is nondeterministic (anywhere in the last few hundred KB), and the amount lost varies between runs — from the final bytes of the last event up to several hundred KB.

## Root Cause

This is a bug in mitmproxy's pure-asyncio TLS read path (upstream report: [mitmproxy/mitmproxy#8364](https://github.com/mitmproxy/mitmproxy/issues/8364)), not in OpenSandbox code:

1. When the upstream serves the response over **TLS + HTTP/1.1** and **closes the connection immediately after the last byte**, mitmproxy's `asyncio.StreamReader`/`sslproto` read loop can deliver the connection EOF while plaintext for the tail of the body is still pending.
2. mitmproxy's HTTP/1 client then calls h11 `ChunkedReader.read_eof()`, which hard-errors on an incomplete chunk:

   ```
   HTTP/1 protocol error: peer closed connection without sending complete message body (incomplete chunked read)
   ```

3. The flow is killed (`error` hook) and the client connection is closed, so the client sees a body cut mid-chunk.

The race window widens with body size — payloads of 4 MiB and up reproduce it reliably, which matches the field report that only chunks larger than ~1 MB truncate.

::: warning Not the `stream_large_bodies` option
`stream_large_bodies: 1m` (config.yaml) is **not** the cause. The truncation reproduces with that option removed, and it also affects non-SSE responses (the buffered path fails with a 502). The "1 MB" figure in reports is an empirical threshold of the race, not a proxy buffer limit.
:::

## Scope (verified empirically, mitmproxy 11.0.2)

| Upstream behavior | Result |
|---|---|
| TLS HTTP/1.1, closes immediately after body (≥ 4 MiB) | Truncated, reliable |
| TLS HTTP/1.1, closes immediately after body (≤ 2 MiB) | Truncated, timing-dependent |
| TLS HTTP/1.1, keeps connection open ~1 s after body | Complete |
| Plain TCP (no TLS) HTTP/1.1, closes immediately | Complete |
| HTTP/2 upstream (h2 client) | Complete |

HTTP/2 upstreams (e.g. OpenAI, Anthropic) are **not** affected. The bug only hits TLS HTTP/1.1 upstream connections that close right after the body — a common pattern for `Connection: close` responses and self-hosted gateways.

## Reproducing

Repro scripts live in [`components/egress/tests/mitm-sse-truncation/`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/tests/mitm-sse-truncation/run.sh). They drive a local `mitmdump` in regular mode (same options as the egress transparent mode) against a synthetic SSE upstream and verify whether the full body arrives.

Prerequisites: `python3`, `openssl`, and `mitmdump` on `PATH` (or pass `--mitmdump /path/to/mitmdump`).

```bash
cd components/egress

# Reproduce the bug (default: TLS upstream, 4 MiB SSE event, immediate close)
./tests/mitm-sse-truncation/run.sh

# Controls that must pass (no truncation)
./tests/mitm-sse-truncation/run.sh --plain
./tests/mitm-sse-truncation/run.sh --delay-close 1

# Show the error hook in the mitmdump log (root-cause evidence)
./tests/mitm-sse-truncation/run.sh --probe --keep-workdir
```

Output looks like:

```
run 3: 3621267 bytes, 57 chunks, 3620757 data, terminated=no, expected=4194326 -> TRUNCATED
```

With `--probe`, the mitmdump log contains:

```
PROBE error hook fired: HTTP/1 protocol error: peer closed connection without sending complete message body (incomplete chunked read)
```

Options: `--size BYTES`, `--iterations N`, `--delay-close SECONDS`, `--plain`, `--read-request` (realistic upstream; the race then triggers only under load), `--probe`, `--mitmdump PATH`, `--port N`, `--upstream-port N`, `--keep-workdir`.

## Status and Workarounds

- Upstream bug: [mitmproxy/mitmproxy#8364](https://github.com/mitmproxy/mitmproxy/issues/8364) — the affected read loop is unchanged through v12.2.3.
- OpenSandbox tracking: [opensandbox-group/OpenSandbox#1461](https://github.com/opensandbox-group/OpenSandbox/issues/1461).

Until mitmproxy fixes the transport race, options are:

- **Use HTTP/2 upstreams** for large streaming responses (h2 negotiation avoids the bug entirely).
- **Pin/fix mitmproxy version** in the egress image once an upstream fix lands; re-run the repro to confirm.
- **Report affected endpoints**: if your LLM gateway serves over TLS HTTP/1.1 with `Connection: close`, large SSE events are at risk.
