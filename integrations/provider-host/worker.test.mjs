import { test, expect } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

test("all real bundled plugins initialize behind the same private Unix socket", async () => {
  const dir = await mkdtemp(join(tmpdir(), "wb2api-provider-test-"))
  const socket = join(dir, "plugin.sock")
  process.env.WB2API_PROVIDER_SOCKET = socket
  process.env.WB2API_PROVIDER_AUTH_FILE = join(dir, "empty-provider-auth.json")
  let server
  try {
    const mod = await import("./worker.mjs")
    server = await mod.start()
    const result = await fetch("http://localhost/health", { unix: socket })
    expect(result.status).toBe(200)
    const info = await result.json()
    expect(info.ready).toBe(true)
    expect(info.providers).toEqual([
      "zcode", "qoder", "qoder-cn", "qwenwork", "qwenwork-enterprise", "codearts",
    ])
    expect(info.credential_count).toBe(0)
    const models = await fetch("http://localhost/models", { unix: socket })
    expect(models.status).toBe(200)
    expect((await models.json()).data).toEqual([])
    const unknown = await fetch("http://localhost/chat/unknown", {
      unix: socket, method: "POST", body: "{}",
    })
    expect(unknown.status).toBe(404)
  } finally {
    server?.stop(true)
    await rm(dir, { recursive: true, force: true })
  }
}, 20_000)
