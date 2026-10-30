// WorkBuddy2API PRIVATE provider runtime. This is started and supervised by
// the existing Go process on an owner-only Unix socket. No TCP listener, no
// second public API, no separate deployment. Its plugins come from the
// pinned magpie-community MIT reference in ../magpie-plugins/.
import { readFile, rename, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import { ZCodeAuthPlugin } from "../magpie-plugins/zcode/index.mjs"
import {
  QoderAuthPlugin, QoderCNAuthPlugin, QwenWorkAuthPlugin,
  QwenWorkEnterpriseAuthPlugin,
} from "../magpie-plugins/qoder/index.mjs"
import { CodeArtsAuthPlugin } from "../magpie-plugins/codearts/index.mjs"
import { chatToAnthropic, anthroResultToChat, anthroStreamToChat } from "./bridge.mjs"

const SOCKET = process.env.WB2API_PROVIDER_SOCKET
const AUTH_FILE = process.env.WB2API_PROVIDER_AUTH_FILE
const MAINTENANCE_FILE = process.env.WB2API_PROVIDER_MAINTENANCE_FILE || join(dirname(AUTH_FILE || "."), "provider-maintenance.json")
const AUTO_CHECKIN = process.env.WB2API_PROVIDER_AUTO_CHECKIN === "true"
const PROVIDERS = Object.freeze({
  zcode: ZCodeAuthPlugin,
  qoder: QoderAuthPlugin,
  "qoder-cn": QoderCNAuthPlugin,
  qwenwork: QwenWorkAuthPlugin,
  "qwenwork-enterprise": QwenWorkEnterpriseAuthPlugin,
  codearts: CodeArtsAuthPlugin,
})

const responses = (status, value) =>
  new Response(JSON.stringify(value), {
    status, headers: { "content-type": "application/json; charset=utf-8" },
  })

let credentials = {}
let writing = Promise.resolve()
let maintenance = { checkins: {} }
let maintenanceWriting = Promise.resolve()
const maintenanceBusy = new Set()
let maintenanceTimer
let autoCheckinRunning = false
const usageCache = new Map()
const pluginInstances = new Map()
const modelCache = new Map()
const roundRobin = new Map()

function providerKeys(id) {
  return Object.keys(credentials)
    .filter(k => k === id || k.startsWith(id + "#"))
    .filter(k => {
      const record = credentials[k]
      // Provider placeholders are not usable login sessions. Do not list
      // them as active accounts or randomly route requests to them.
      return record && typeof record === "object" &&
        ((typeof record.access === "string" && !!record.access.trim()) ||
         (typeof record.refresh === "string" && !!record.refresh.trim()))
    })
    .sort()
}

function pickAccount(id) {
  const accounts = providerKeys(id)
  if (!accounts.length) return null
  const pos = roundRobin.get(id) || 0
  roundRobin.set(id, pos + 1)
  return accounts[pos % accounts.length]
}

async function saveAuth(key, next) {
  // Serialize refresh-token rotations: single-use tokens must not be
  // accidentally reverted by a concurrent write from a different Provider.
  writing = writing.catch(() => {}).then(async () => {
    credentials[key] = next
    const tmp = AUTH_FILE + ".tmp-" + process.pid
    await writeFile(tmp, JSON.stringify(credentials, null, 2) + "\n", { mode: 0o600 })
    await rename(tmp, AUTH_FILE)
    modelCache.clear()
  })
  await writing
}

// Vendor check-ins are day-scoped. Use the vendor-facing Beijing calendar
// day, never a server-local time zone, and retain attempts across restarts.
function calendarDay(now = new Date()) {
  return new Intl.DateTimeFormat("en-CA", {
    timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
  }).format(now)
}

function publicUsage(data) {
  const out = {}
  for (const k of ["plan", "until", "user", "signIn", "error"]) {
    if (typeof data?.[k] === "string") out[k] = data[k].slice(0, 240)
  }
  out.windows = Array.isArray(data?.windows)
    ? data.windows.slice(0, 24).map(w => ({
      name: String(w?.name || "Allowance").slice(0, 120),
      ...(Number.isFinite(w?.used) ? { used: Math.max(0, Math.min(100, w.used)) } : {}),
      ...(typeof w?.display === "string" ? { display: w.display.slice(0, 200) } : {}),
      ...(typeof w?.until === "string" ? { until: w.until.slice(0, 48) } : {}),
      ...(w?.aside ? { aside: true } : {}),
    }))
    : []
  return out
}

async function recordCheckin(key, result) {
  const entry = {
    day: calendarDay(),
    at: new Date().toISOString(),
    outcome: String(result?.outcome || "failed").slice(0, 36),
    message: String(result?.message || "").slice(0, 200),
    ...(Number.isFinite(result?.credit) ? { credit: result.credit } : {}),
    ...(typeof result?.until === "string" ? { until: result.until.slice(0, 48) } : {}),
  }
  maintenanceWriting = maintenanceWriting.catch(() => {}).then(async () => {
    maintenance.checkins[key] = entry
    const tmp = MAINTENANCE_FILE + ".tmp-" + process.pid
    await writeFile(tmp, JSON.stringify(maintenance, null, 2) + "\n", { mode: 0o600 })
    await rename(tmp, MAINTENANCE_FILE)
  })
  await maintenanceWriting
  return entry
}

function maintenanceEligible(id) {
  // QwenWork's upstream plugin deliberately implements NO check-in; do not
  // invent a claim endpoint or report success.
  return id === "zcode" || id === "qoder" || id === "qoder-cn" || id === "codearts"
}

async function checkinAccount(id, key, { force = false } = {}) {
  const today = calendarDay()
  if (!maintenanceEligible(id)) return { outcome: "unsupported", message: "upstream plugin does not support check-in" }
  const existing = maintenance.checkins[key]
  if (!force && existing?.day === today) return { ...existing, cached: true }
  if (maintenanceBusy.has(key)) return { outcome: "busy", message: "check-in already in progress" }
  maintenanceBusy.add(key)
  try {
    const { plugin } = await configuredPlugin(id, key)
    if (typeof plugin.auth?.checkin !== "function") return { outcome: "unsupported" }
    let result
    try { result = await plugin.auth.checkin(async () => credentials[key]) }
    catch { result = { outcome: "failed", message: "provider check-in request failed" } }
    return await recordCheckin(key, result)
  } finally {
    maintenanceBusy.delete(key)
  }
}

async function autoCheckin() {
  if (!AUTO_CHECKIN || autoCheckinRunning) return
  autoCheckinRunning = true
  try {
    const entries = Object.keys(PROVIDERS)
      .filter(maintenanceEligible)
      .flatMap(id => providerKeys(id).map(key => ({ id, key })))
    for (const { id, key } of entries) {
      if (maintenance.checkins[key]?.day === calendarDay()) continue
      try { await checkinAccount(id, key) } catch {}
    }
  } finally {
    autoCheckinRunning = false
  }
}

async function handleUsage(url) {
  const requested = url.searchParams.get("provider")
  const ids = requested ? [requested] : Object.keys(PROVIDERS)
  if (ids.some(id => !PROVIDERS[id])) return responses(400, { error: "unknown provider" })
  const result = []
  for (const id of ids) {
    for (const key of providerKeys(id)) {
      const cacheKey = id + "\0" + key
      const cached = usageCache.get(cacheKey)
      if (cached && Date.now() - cached.at < 180_000) {
        result.push({ provider: id, account: key, ...cached.value, cached: true })
        continue
      }
      let value
      try {
        const { plugin, provider } = await configuredPlugin(id, key)
        if (typeof plugin.auth?.usage !== "function") value = { error: "usage query unsupported", windows: [] }
        else {
          const read = await Promise.race([
            plugin.auth.usage(async () => credentials[key], provider),
            new Promise((_, reject) => setTimeout(() => reject(new Error("quota request timeout")), 30_000)),
          ])
          value = publicUsage(read)
        }
      } catch { value = { error: "provider usage request failed", windows: [] } }
      usageCache.set(cacheKey, { value, at: Date.now() })
      result.push({ provider: id, account: key, ...value })
    }
  }
  return responses(200, { data: result })
}

async function handleCheckin(req) {
  let input
  try { input = await req.json() }
  catch { return responses(400, { error: "invalid JSON" }) }
  const provider = String(input?.provider || "")
  if (!PROVIDERS[provider]) return responses(400, { error: "unknown provider" })
  const requestedAccount = String(input?.account || "")
  const keys = providerKeys(provider)
  if (requestedAccount && !keys.includes(requestedAccount))
    return responses(404, { error: "account not found" })
  const results = []
  for (const key of requestedAccount ? [requestedAccount] : keys) {
    const value = await checkinAccount(provider, key, { force: input?.force === true })
    results.push({ provider, account: key, ...value })
  }
  return responses(200, { data: results })
}

function runtimeClient(accountKey) {
  return {
    auth: {
      // Magpie plugins call auth.set after successful refresh. Always write
      // back to the *selected* account, not the provider-wide alias.
      async set({ body }) {
        if (body && typeof body === "object") await saveAuth(accountKey, body)
      },
    },
    app: {
      // Never echo vendor responses: they can include bearer credentials.
      log: () => {},
    },
  }
}

async function configuredPlugin(id, key) {
  const cacheKey = id + "\0" + key
  if (!pluginInstances.has(cacheKey)) {
    pluginInstances.set(cacheKey, (async () => {
      const plugin = await PROVIDERS[id]({ client: runtimeClient(key) })
      const config = { provider: {} }
      await plugin.config?.(config)
      return { plugin, provider: config.provider?.[id] || { id, models: {} } }
    })())
  }
  return pluginInstances.get(cacheKey)
}

function providerModel(id, name, model) {
  const context = Number(model?.limit?.context || model?.context_length || 0)
  const maxOutput = Number(model?.limit?.output || model?.max_output_tokens || 0)
  return {
    id: id + ":" + name, object: "model",
    created: 1753600000, owned_by: id,
    ...(model?.name ? { name: model.name } : {}),
    ...(context > 0 ? { context_length: context } : {}),
    ...(maxOutput > 0 ? { max_output_tokens: maxOutput } : {}),
    provider: id,
  }
}

async function listModels(id, key) {
  const cached = modelCache.get(id + "\0" + key)
  if (cached && Date.now() - cached.at < 120_000) return cached.models
  const { plugin, provider } = await configuredPlugin(id, key)
  const currentAuth = credentials[key]
  const defaults = provider.models || {}
  let models = defaults
  try {
    if (typeof plugin.provider?.models === "function") {
      // On a dead/offline account, retain the plugin's static catalogue.
      // Never propagate credentials or raw errors to the public models API.
      const runtime = await Promise.race([
        plugin.provider.models(provider, { auth: currentAuth }),
        new Promise((_, reject) => setTimeout(() => reject(new Error("model listing timed out")), 6000)),
      ])
      if (runtime && typeof runtime === "object") models = runtime
    }
  } catch {}
  const listed = Object.entries(models)
    .filter(([name]) => typeof name === "string" && name.length)
    .map(([name, model]) => providerModel(id, name, model))
  modelCache.set(id + "\0" + key, { at: Date.now(), models: listed })
  return listed
}

async function handleModels() {
  const ids = Object.keys(PROVIDERS).filter(id => providerKeys(id).length > 0)
  const items = await Promise.allSettled(ids.map(async id => listModels(id, providerKeys(id)[0])))
  const data = items.flatMap(x => x.status === "fulfilled" ? x.value : [])
  return responses(200, { object: "list", data })
}

async function handleChat(req, id) {
  if (!PROVIDERS[id]) return responses(404, { error: { message: "unknown provider" } })
  const key = pickAccount(id)
  if (!key) return responses(503, { error: { message: "no imported provider credential", type: "provider_unavailable" } })
  let chat
  try { chat = await req.json() }
  catch { return responses(400, { error: { message: "invalid Chat Completions JSON" } }) }
  const requestedModel = String(chat.model || "")
  if (!requestedModel) return responses(400, { error: { message: "missing model" } })
  const source = { ...chat }
  const isZCode = id === "zcode"
  const { plugin, provider } = await configuredPlugin(id, key)
  const getAuth = async () => credentials[key]
  const loader = await plugin.auth.loader(getAuth, provider)
  if (!loader?.fetch || !loader?.baseURL) {
    return responses(503, { error: { message: "provider credential is not ready" } })
  }
  const url = String(loader.baseURL).replace(/\/$/, "") + (isZCode ? "/messages" : "/chat/completions")
  const payload = isZCode ? chatToAnthropic(source) : source
  const headers = {
    "content-type": "application/json",
    ...(req.headers.get("x-wb2api-session") ? { "x-deepseek-harness-session-id": req.headers.get("x-wb2api-session") } : {}),
  }
  // No user-controlled base URL: only plugin-controlled vendor origins.
  const output = await loader.fetch(url, {
    method: "POST", headers, body: JSON.stringify(payload), signal: req.signal,
  })
  if (!(output instanceof Response)) throw new Error("provider returned no response")
  if (!output.ok || !isZCode) {
    // The Qoder and CodeArts plugins already return Chat Completions and
    // streaming Chat SSE. Do not reparse them, especially not tool_calls.
    return new Response(output.body, {
      status: output.status, headers: { "content-type": output.headers.get("content-type") || "application/json" },
    })
  }
  if (source.stream) {
    return new Response(anthroStreamToChat(output.body, requestedModel), {
      status: output.status, headers: { "content-type": "text/event-stream", "cache-control": "no-cache" },
    })
  }
  return responses(200, anthroResultToChat(await output.json(), requestedModel))
}

async function dispatch(req) {
  const url = new URL(req.url)
  const path = url.pathname
  if (req.method === "GET" && path === "/health") {
    return responses(200, { ready: true, providers: Object.keys(PROVIDERS), credential_count: Object.keys(credentials).length })
  }
  if (req.method === "GET" && path === "/models") return handleModels()
  if (req.method === "GET" && path === "/usage") return handleUsage(url)
  if (req.method === "GET" && path === "/checkins") {
    return responses(200, {
      auto_checkin: AUTO_CHECKIN,
      data: Object.keys(PROVIDERS).map(id => ({
        provider: id, supported: maintenanceEligible(id),
        accounts: providerKeys(id).map(key => ({
          account: key, ...(maintenance.checkins[key] || { outcome: "not_run" }),
        })),
      })),
    })
  }
  if (req.method === "POST" && path === "/checkin") return handleCheckin(req)
  if (req.method === "GET" && path === "/accounts") {
    return responses(200, Object.fromEntries(Object.keys(PROVIDERS)
      .map(id => [id, { count: providerKeys(id).length, ready: providerKeys(id).length > 0 }])))
  }
  if (req.method === "POST" && path.startsWith("/chat/")) {
    const id = decodeURIComponent(path.slice("/chat/".length))
    return handleChat(req, id)
  }
  return responses(404, { error: { message: "not found" } })
}

export async function start() {
  if (!SOCKET || !AUTH_FILE) throw new Error("private Unix socket and auth file are required")
  try {
    credentials = JSON.parse(await readFile(AUTH_FILE, "utf8"))
    if (!credentials || Array.isArray(credentials) || typeof credentials !== "object")
      throw new Error("invalid provider credential file structure")
  } catch (err) {
    if (err.code !== "ENOENT") throw err
    credentials = {}
  }
  try {
    maintenance = JSON.parse(await readFile(MAINTENANCE_FILE, "utf8"))
    if (!maintenance || !maintenance.checkins || typeof maintenance.checkins !== "object")
      throw new Error("invalid check-in state")
  } catch (err) {
    if (err.code !== "ENOENT") throw err
    maintenance = { checkins: {} }
  }
  const server = Bun.serve({
    unix: SOCKET,
    async fetch(req) {
      try { return await dispatch(req) }
      catch (error) {
        // Intentionally avoid logging remote exception details: vendor
        // exceptions can carry access tokens or signing materials.
        return responses(502, { error: { type: "provider_error", message: "provider request failed", retryable: true } })
      }
    },
  })
  process.on("SIGTERM", () => server.stop(true))
  if (AUTO_CHECKIN) {
    // One attempt per vendor-facing day and account, persisted to disk.
    // No background usage polling: balances are only queried on demand.
    maintenanceTimer = setInterval(() => { void autoCheckin() }, 15 * 60 * 1000)
    maintenanceTimer.unref?.()
    // A delayed first tick avoids colliding with startup and simultaneous
    // Magpie credential migration; the first 15m tick does the check-in.
  }
  return server
}

if (import.meta.main) await start()
