# Ingress Fleets Route Scope v1

This document freezes the internal route scope shared by the fleets server
adapter and the Go ingress gateway. It is not a replacement for the public
`secure_access` policy. The scope only authenticates the routing tuple that the
server placed in a stable endpoint handle.

## Token

The v1 token is:

```text
f1.<namespace_b64url>.<sandbox_id_b64url>.<port>.<key_id>.<mac_b64url>
```

- `namespace_b64url` and `sandbox_id_b64url` use unpadded RFC 4648 base64url.
- Decoded namespace and sandbox ID are non-empty UTF-8 strings without control
  characters.
- `port` is canonical decimal in the range 1 through 65535, with no leading
  zeroes.
- `key_id` is one lowercase ASCII character in `[0-9a-z]`.
- `mac_b64url` is the unpadded base64url encoding of the first 16 bytes of
  HMAC-SHA256.

The HMAC input is the following UTF-8 string, including the final newline:

```text
opensandbox-fleets-route-v1
<namespace>
<sandbox_id>
<port>
```

The signing key is selected by `key_id`. Phase 1a reuses the configured ingress
signing key ring, but the distinct HMAC input prefix provides domain separation
from OSEP-0011 public secure-access signatures.

The v1 scope has no timestamp because it is the stable routing handle. Key
rotation revokes the signature, while the sandbox lifecycle determines whether
the tuple currently resolves. The FastPath route credential behind it remains
short-lived and is never exposed to the caller.

## Placement

- Header mode places the complete token in `OpenSandbox-Ingress-To`.
- URI mode places the token in the first path segment:
  `/<token>/<application-path>`.
- Wildcard-host mode is not supported for fleets in Phase 1a. A generic
  namespace, sandbox ID, and 128-bit MAC cannot be represented safely within
  one DNS label's 63-byte limit.

The ingress gateway must verify the MAC before using the namespace. It must not
accept a namespace from a separate caller-controlled header or URL segment.
Legacy BatchSandbox and AgentSandbox route formats remain unchanged.

The language-neutral fixture at
[`fixtures/ingress-route-scope-v1.json`](fixtures/ingress-route-scope-v1.json)
is the compatibility vector for Go and Python implementations.

## Renew intent

Fleets ingress requests publish the existing JSON intent with one additive,
required field:

```json
{
  "namespace": "tenant-a",
  "sandbox_id": "sandbox-123",
  "observed_at": "2026-01-01T00:00:00Z",
  "port": 44772,
  "request_uri": "/health"
}
```

Consumers that support fleets must reject an intent with a missing or empty
namespace. Publishers, throttles, consumer locks, and caches use
`(namespace, sandbox_id)` as their identity key.
