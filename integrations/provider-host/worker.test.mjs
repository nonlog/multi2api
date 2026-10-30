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
    const usage = await fetch("http://localhost/usage", { unix: socket })
    expect(usage.status).toBe(200)
    expect((await usage.json()).data).toEqual([])
    const filtered = await fetch("http://localhost/usage?provider=zcode", { unix: socket })
    expect(filtered.status).toBe(200)
    expect((await filtered.json()).data).toEqual([])
    const invalidFilter = await fetch("http://localhost/usage?provider=unknown", { unix: socket })
    expect(invalidFilter.status).toBe(400)
    const checkins = await fetch("http://localhost/checkins", { unix: socket })
    expect(checkins.status).toBe(200)
    const state = await checkins.json()
    expect(state.auto_checkin).toBe(false)
    expect(state.data.find(x => x.provider === "qwenwork").supported).toBe(false)
    expect(state.data.find(x => x.provider === "qoder").supported).toBe(true)
    const claim = await fetch("http://localhost/checkin", {
      unix: socket, method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ provider: "qwenwork" }),
    })
    expect(claim.status).toBe(200)
    expect((await claim.json()).data).toEqual([])

    const accountsBefore = await fetch("http://localhost/admin/accounts", { unix: socket })
    expect((await accountsBefore.json()).data).toEqual([])
    const methods = await fetch("http://localhost/admin/methods?provider=qoder", { unix: socket })
    expect(methods.status).toBe(200)
    expect((await methods.json()).methods.length).toBeGreaterThan(0)
    const codearts = await fetch("http://localhost/admin/methods?provider=codearts", { unix: socket })
    expect((await codearts.json()).supported).toBe(false)
    const invalidMethod = await fetch("http://localhost/admin/methods?provider=garbage", { unix: socket })
    expect(invalidMethod.status).toBe(400)

    const secret = "never-return-this-access-token"
    const perform = (path, body) => fetch("http://localhost/admin/" + path, {
      unix: socket, method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    })
    const imported = await perform("import", {
      provider: "qoder", credential: { type: "oauth", access: secret, uid: "test-user", accountId: "sample@example.test" },
    })
    expect(imported.status).toBe(200)
    const { account } = await imported.json()
    const listed = await fetch("http://localhost/admin/accounts", { unix: socket })
    const raw = await listed.text()
    expect(raw.includes(secret)).toBe(false)
    expect(JSON.parse(raw).data).toHaveLength(1)
    expect((await perform("account/action", {provider:"qoder",account,action:"disable"})).status).toBe(200)
    const listing = await fetch("http://localhost/admin/accounts", { unix: socket })
    expect((await listing.json()).data[0].disabled).toBe(true)
    expect((await perform("account/action", {provider:"qoder",account,action:"enable"})).status).toBe(200)
    expect((await perform("account/action", {provider:"qoder",account,action:"remove"})).status).toBe(200)
    const removed = await fetch("http://localhost/admin/accounts", { unix: socket })
    expect((await removed.json()).data).toHaveLength(0)
  } finally {
    server?.stop(true)
    await rm(dir, { recursive: true, force: true })
  }
}, 20_000)
