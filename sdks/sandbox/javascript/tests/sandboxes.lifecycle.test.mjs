import assert from "node:assert/strict";
import test from "node:test";

import { SandboxesAdapter } from "../dist/internal.js";

function createAdapter() {
  const requests = [];
  const client = {
    async POST(path, options) {
      assert.equal(path, "/sandboxes");
      requests.push(JSON.parse(JSON.stringify(options.body)));
      return {
        data: {
          id: "sandbox-1",
          status: { state: "Running" },
          entrypoint: ["sleep", "infinity"],
          createdAt: "2026-08-20T00:00:00Z",
          expiresAt: null,
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };
  return { adapter: new SandboxesAdapter(client), requests };
}

test("createSandbox forwards lifecycle hooks in the standard request body", async () => {
  const { adapter, requests } = createAdapter();
  const lifecycle = {
    preStart: { command: ["/opt/hooks/restore.sh"], timeoutSeconds: 60 },
    periodic: [
      {
        name: "checkpoint",
        schedule: "*/5 * * * *",
        command: ["/opt/hooks/checkpoint.sh"],
        timeoutSeconds: 30,
      },
    ],
  };

  await adapter.createSandbox({ lifecycle });

  assert.deepEqual(requests[0].lifecycle, lifecycle);
});

test("createSandbox omits lifecycle when it is not configured", async () => {
  const { adapter, requests } = createAdapter();

  await adapter.createSandbox({ lifecycle: undefined });

  assert.equal(Object.hasOwn(requests[0], "lifecycle"), false);
});
