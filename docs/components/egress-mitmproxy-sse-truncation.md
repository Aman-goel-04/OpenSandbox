---
title: Egress Mitmproxy SSE Truncation
description: Why large SSE / streamed bodies can be truncated at the tail when an HTTP/1.1 upstream closes its connection with unread request data (TCP RST flushes the receiver's kernel buffer), with reproduction scripts and status.
---

# Egress Mitmproxy: Large SSE Chunks Truncated at the Tail

## Symptom

Sandboxes calling an LLM API through the egress sidecar (transparent mitmproxy mode) occasionally receive a **truncated SSE stream**: a single `data:` event larger than ~1 MB is cut off near the end, and the SDK reports malformed JSON or a dropped connection.

The truncation point is nondeterministic (anywhere in the last few hundred KB), and the amount lost varies between runs — from the final bytes of the last event up to several hundred KB.

## Root Cause

Byte-level instrumentation of the full data path (transport read loop, TLS layer, h11 parser) shows this is **standard TCP RST semantics**, not a mitmproxy or OpenSandbox bug:

1. The upstream closes its connection while **unread data is still in its own receive buffer** (for example a gateway that responds without ever consuming the client's request body). The kernel then sends **TCP RST** instead of a clean FIN.
2. Per [RFC 793](https://www.rfc-editor.org/rfc/rfc793), a RST **discards the receiver's unread kernel receive buffer**. mitmproxy has typically consumed ~90% of the body by then, but the tail (last tens to hundreds of KB) is still sitting in mitmproxy's kernel receive buffer and is flushed by the RST — the application never sees those bytes.
3. h11's `ChunkedReader.read_eof()` then hard-errors with `incomplete chunked read`, the flow is killed, and the client connection is closed — so the sandbox sees a truncated SSE stream.

An earlier analysis pointed at an asyncio/sslproto race; that was wrong (mitmproxy 11 terminates TLS with its own pyOpenSSL memory-BIO layer, and the loss happens before any userspace code sees the bytes). The mitmproxy upstream issue was closed as not-a-bug after the corrected analysis.

## Verification (mitmproxy 11.0.2, 4–8 MiB SSE events)

| Upstream behavior | Result |
|---|---|
| TLS HTTP/1.1, reads the request, closes cleanly (FIN) right after the body | Complete, 14/14 |
| TLS HTTP/1.1, closes with unread request data (→ RST) | Truncated, reliable |
| Plain TCP, closes with unread request data (→ RST) | Truncated |
| TLS HTTP/1.1, keeps the connection open ~1 s after the body | Complete |
| HTTP/2 upstream (h2 client) | Complete |

No userspace change helps: raising the read chunk size and `asyncio.open_connection(limit=1 MiB)` still lost data (10/10 truncated) because the RST flush happens in the kernel before the application can read. A direct (non-proxied) client loses less only because it consumes the kernel buffer faster; a client with a slow read loop loses data against such a server too.

## Reproducing

Repro scripts live in [`components/egress/tests/mitm-sse-truncation/`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/tests/mitm-sse-truncation/run.sh). They drive a local `mitmdump` in regular mode (same options as the egress transparent mode) against a synthetic SSE upstream that closes with unread request data, and verify whether the full body arrives.

Prerequisites: `python3`, `openssl`, and `mitmdump` on `PATH` (or pass `--mitmdump /path/to/mitmdump`).

```bash
cd components/egress

# Reproduce the truncation (default: TLS upstream, 4 MiB SSE event, RST close)
./tests/mitm-sse-truncation/run.sh

# Controls that must pass (no truncation)
./tests/mitm-sse-truncation/run.sh --plain
./tests/mitm-sse-truncation/run.sh --delay-close 1
./tests/mitm-sse-truncation/run.sh --read-request

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

Options: `--size BYTES`, `--iterations N`, `--delay-close SECONDS`, `--plain`, `--read-request` (clean-FIN control), `--probe`, `--mitmdump PATH`, `--port N`, `--upstream-port N`, `--keep-workdir`.

## Status and Action

- mitmproxy upstream: [mitmproxy/mitmproxy#8364](https://github.com/mitmproxy/mitmproxy/issues/8364) — closed as not-a-bug (TCP RST semantics).
- OpenSandbox tracking: [opensandbox-group/OpenSandbox#1461](https://github.com/opensandbox-group/OpenSandbox/issues/1461) — awaiting field confirmation.

There is no code fix in mitmproxy or OpenSandbox that prevents this — the bytes are discarded by the kernel. The fix belongs to the **upstream server**. The rest of this section is a practical guide for the gateway owner: how to confirm the gateway is the problem, and what to change.

### 1. Confirm the gateway is sending RST

Run these on (or in front of) the gateway while a sandbox reproduces the truncation:

```bash
# 1) Watch for RST flags on the gateway's port (the definitive check)
tcpdump -ni any 'tcp port 443 and tcp[tcpflags] & (tcp-rst) != 0'

# 2) Counter check: total resets sent/received by the host
netstat -s | grep -i reset

# 3) From a client, the difference between a clean close and a reset:
curl -v https://gateway.example/v1/chat/completions   # ...
# clean close:  "Empty reply from server" or normal EOF
# reset:        "Recv failure: Connection reset by peer"  / curl: (56)
```

Typical signature of this bug: the RST appears **immediately after the last response byte**, and `tcpdump` shows no FIN for that connection. A client-side symptom that *also* points at the gateway: a non-proxied client that reads slowly (e.g. `curl --limit-rate 1m`) loses the tail too, while a fast client gets everything.

The root trigger is almost always: **the gateway closes the connection while the request body (or the client's trailing data) is still unread in its receive buffer**. LLM prompts are large bodies, so a gateway that stops reading mid-request and then closes after streaming is exactly the profile that produces this.

### 2. Fix per gateway type

The rule is simple: **before closing a connection, consume whatever the client sent; for SSE, prefer not closing at all.**

- **nginx (reverse proxy / front for the LLM service)**
  ```nginx
  # Defaults are safe for buffered requests. Only these matter if you had
  # proxy_request_buffering off (streaming request bodies):
  lingering_close on;        # drain client data before closing (default on — make sure it was not disabled)
  lingering_time 30s;
  lingering_timeout 5s;
  ```
  With `proxy_request_buffering on` (default), nginx consumes the request body before proxying and is not the source of the RST.

- **Python: FastAPI / Starlette / uvicorn (very common for LLM gateways)**
  ```python
  @app.post("/v1/chat/completions")
  async def chat(request: Request):
      body = await request.body()      # consume the full request body BEFORE streaming
      async def gen():
          ...
      return StreamingResponse(gen(), media_type="text/event-stream")
  ```
  If the handler never reads `request.body()`/`request.stream()` and the app closes the connection after the streaming response, uvicorn can close with unread request data → RST. Reading the body first (or enabling keep-alive) fixes it.

- **Go: net/http server**
  ```go
  // read the full body before writing the streamed response:
  body, _ := io.ReadAll(r.Body)
  // or drain it explicitly if you only need the head:
  // io.Copy(io.Discard, r.Body)
  ```
  Go does not drain the request body for you on close; an unread body + `Connection: close` produces a RST. Also make sure nothing sets `SO_LINGER` to 0 (which forces RST on close):
  ```go
  // never do this for streaming responses:
  // l := &net.TCPListener{}; (tcpConn).SetLinger(0)  // forces RST on close
  ```

- **Generic guidance (all servers / LBs / ingress controllers)**
  - Prefer **keep-alive**: end chunked bodies with `0\r\n\r\n` and keep the connection open; close only on idle timeout (clean FIN).
  - Never close with `SO_LINGER` 0 after streaming a response.
  - If a connection must be closed, first drain the socket until the request is fully consumed (or at least until the receive buffer is empty).

- **HTTP/2**: connections over h2 are not close-delimited per response; upgrading the endpoint (or fronting it with an h2-capable proxy) removes the whole class of failure.

### 3. If the gateway cannot be changed

- Switch the sandbox to an **HTTP/2** path to the gateway (verified unaffected).
- Front the gateway with a small reverse proxy that reads the request body and forwards keep-alive (e.g. nginx per above).
- Track it in the OpenSandbox issue so the truncation reports stay attributable until the gateway is fixed.

### Why not a bigger read buffer on the egress side?

Measured (4 MiB SSE event, RST-closing upstream, 6 runs per cell):

| Upstream pacing (simulated network rate) | Loss with 64 KiB reads | Loss with 1 MiB reads |
|---|---|---|
| Burst (loopback-like) | ~188 KiB | ~117 KiB |
| ~128 MB/s (0.5 ms / 64 KiB) | 8 B | 8 B |
| ~32 MB/s (2 ms / 64 KiB) | 8 B | 8 B |

Faster reads shrink the loss window at burst rate, and on any realistically paced path the loss collapses to the last few bytes — but even those few bytes cut the trailing SSE event, so the client stream breaks either way. The mitigation does not change the user-visible outcome, so no patch is shipped.
