import { test, expect } from "bun:test"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { createServer } from "node:http"
import { Buffer } from "node:buffer"
import { tmpdir } from "node:os"
import { join } from "node:path"

test("all real bundled plugins initialize behind the same private Unix socket", async () => {
  const dir = await mkdtemp(join(tmpdir(), "wb2api-provider-test-"))
  const socket = join(dir, "plugin.sock")
  const authFile = join(dir, "empty-provider-auth.json")
  const proxyPoolFile = join(dir, "proxy-pool.json")
  await writeFile(proxyPoolFile, JSON.stringify([
    {id:"direct",name:"DIRECT",url:""},
    {id:"us-warp",name:"US WARP",url:"socks5h://127.0.0.1:56356"},
  ]))
  let proxyAuthorization = ""
  let proxyConnectAuthorization = ""
  const proxyServer = createServer((req, res) => {
    proxyAuthorization = String(req.headers["proxy-authorization"] || "")
    res.writeHead(200, {"content-type":"text/plain"}).end("through-account-proxy")
  })
  proxyServer.on("connect", (req, socket) => {
    proxyConnectAuthorization = String(req.headers["proxy-authorization"] || "")
    socket.write("HTTP/1.1 200 Connection Established\r\n\r\n", () => socket.destroy())
  })
  await new Promise(resolve => proxyServer.listen(0, "127.0.0.1", resolve))
  const proxyAddress = proxyServer.address()
  const directServer = createServer((_req, res) => res.writeHead(200, {"content-type":"text/plain"}).end("direct"))
  await new Promise(resolve => directServer.listen(0, "127.0.0.1", resolve))
  const directAddress = directServer.address()
  process.env.WB2API_PROVIDER_SOCKET = socket
  process.env.WB2API_PROVIDER_AUTH_FILE = authFile
  process.env.WB2API_PROXY_POOL_FILE = proxyPoolFile
  process.env.WB2API_PROXY_ENDPOINT = `http://127.0.0.1:${proxyAddress.port}`
  process.env.WB2API_PROXY_TOKEN = "provider-proxy-test-token"
  let server
  try {
    const mod = await import("./worker.mjs")
    server = await mod.start()
    const secretInError = "do-not-leak-authorization-secret"
    const qoderExchange = mod.loginFailure("Qoder 国际版", new Error("Qoder job token: status 401: " + secretInError))
    expect(qoderExchange.code).toBe("token_exchange")
    expect(qoderExchange.message.includes(secretInError)).toBe(false)
    const qoderTimeout = mod.loginFailure("Qoder 国际版", new Error("Qoder: the sign-in timed out; start again"))
    expect(qoderTimeout.code).toBe("device_timeout")
    const qoderStorage = mod.loginFailure("Qoder 国际版", new Error("EACCES: write /private/secret-file"))
    expect(qoderStorage.code).toBe("save_failed")
    // A plugin's sign-in answers { type: "success", access, … }; the record
    // stored is an oauth credential, and its access is still required.
    const signedIn = mod.loginRecord({ type: "success", access: "dv-1", refresh: "rt-1", uid: "user-1" })
    expect(signedIn.type).toBe("oauth")
    expect(signedIn.access).toBe("dv-1")
    expect(signedIn.refresh).toBe("rt-1")
    expect(signedIn.uid).toBe("user-1")
    expect(() => mod.loginRecord({ type: "success" })).toThrow(/usable access token/)
    expect(() => mod.loginRecord({ type: "success", access: "   " })).toThrow(/usable access token/)
    expect(() => mod.loginRecord({ type: "error", error: "denied by the owner" })).toThrow(/denied by the owner/)
    expect(() => mod.loginRecord(undefined)).toThrow(/vendor rejected sign-in/)
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
    const qoderMethods = await fetch("http://localhost/admin/methods?provider=qoder", { unix: socket })
    const methodData = await qoderMethods.json()
    expect(methodData.methods.some(m => /PAT/i.test(m.label))).toBe(true)
    const qoderDevice = await fetch("http://localhost/admin/login/start", {
      unix: socket, method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ provider: "qoder", method: methodData.methods[0].id }),
    })
    expect(qoderDevice.status).toBe(200)
    const qoderFlow = await qoderDevice.json()
    expect(new URL(qoderFlow.url).searchParams.get("client_id")).toBe("e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb")
    await fetch("http://localhost/admin/login/cancel", {
      unix: socket, method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ state: qoderFlow.state }),
    })
    const qoderCnMethods = await (await fetch("http://localhost/admin/methods?provider=qoder-cn", { unix: socket })).json()
    expect(qoderCnMethods.methods.some(m => /dedicated domain/i.test(m.label))).toBe(false)
    const cancel = await fetch("http://localhost/admin/login/cancel", {
      unix:socket, method:"POST", headers:{"content-type":"application/json"},
      body:JSON.stringify({state:"not-a-valid-existing-login"}),
    })
    expect(cancel.status).toBe(200)
    const cancelled = await fetch("http://localhost/admin/login/poll", {
      unix:socket, method:"POST", headers:{"content-type":"application/json"},
      body:JSON.stringify({state:"not-a-valid-existing-login"}),
    })
    expect(cancelled.status).toBe(404)
    const codearts = await fetch("http://localhost/admin/methods?provider=codearts", { unix: socket })
    const codeartsMethods = await codearts.json()
    expect(codeartsMethods.supported).toBe(true)
    expect(codeartsMethods.methods[0].label).toContain('Huawei Cloud')
    const start = await fetch('http://localhost/admin/login/start', {unix:socket,method:'POST',
      headers:{'content-type':'application/json'},
      body:JSON.stringify({provider:'codearts',method:codeartsMethods.methods[0].id})})
    expect(start.status).toBe(200)
    const flow = await start.json()
    expect(flow.manual_callback).toBe(true)
    const badCallback = await fetch('http://localhost/admin/login/callback',{unix:socket,method:'POST',
      headers:{'content-type':'application/json'},body:JSON.stringify({state:flow.state,
      url:'https://example.org/oauth/callback?code=invalid'})})
    expect(badCallback.status).toBe(400)
    const canceled = await fetch('http://localhost/admin/login/cancel',{unix:socket,method:'POST',
      headers:{'content-type':'application/json'},body:JSON.stringify({state:flow.state})})
    expect(canceled.status).toBe(200)
    const afterCancel = await fetch('http://localhost/admin/login/callback',{unix:socket,method:'POST',
      headers:{'content-type':'application/json'},body:JSON.stringify({state:flow.state,url:'test'})})
    expect(afterCancel.status).toBe(404)
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
    const assigned = await perform("account/proxy", {provider:"qoder",account,proxy_id:"us-warp"})
    expect(assigned.status).toBe(200)
    const accountProxyState = await fetch("http://localhost/admin/accounts", {unix:socket})
    expect((await accountProxyState.json()).data[0].proxy_id).toBe("us-warp")
    const badAssignment = await perform("account/proxy", {provider:"qoder",account,proxy_id:"not-in-pool"})
    expect(badAssignment.status).toBe(400)
    const routed = await mod._internal.withAccountProxy(account, () => mod._internal.providerFetch("http://example.invalid/proxy-check"))
    expect(routed.status).toBe(200)
    expect(await routed.text()).toBe("through-account-proxy")
    const proxyBasic = "Basic " + Buffer.from("us-warp:provider-proxy-test-token").toString("base64")
    expect(proxyAuthorization).toBe(proxyBasic)
    let tlsTunnelClosed = false
    try {
      await mod._internal.withAccountProxy(account, () => mod._internal.providerFetch("https://example.invalid/proxy-check"))
    } catch { tlsTunnelClosed = true }
    expect(tlsTunnelClosed).toBe(true)
    expect(proxyConnectAuthorization).toBe(proxyBasic)
    const direct = await mod._internal.withAccountProxy("unassigned-account", () =>
      mod._internal.providerFetch(`http://127.0.0.1:${directAddress.port}/direct-check`))
    expect(direct.status).toBe(200)
    expect(await direct.text()).toBe("direct")
    expect(proxyAuthorization).toBe(proxyBasic)
    const persisted = JSON.parse(await (await import("node:fs/promises")).readFile(authFile, "utf8"))
    expect(persisted[account].proxy_id).toBe("us-warp")
    expect((await perform("account/action", {provider:"qoder",account,action:"remove"})).status).toBe(200)
    const removed = await fetch("http://localhost/admin/accounts", { unix: socket })
    expect((await removed.json()).data).toHaveLength(0)
  } finally {
    server?.stop(true)
    await new Promise(resolve => proxyServer.close(resolve))
    await new Promise(resolve => directServer.close(resolve))
    await rm(dir, { recursive: true, force: true })
  }
}, 20_000)
