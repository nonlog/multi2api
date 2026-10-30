// WorkBuddy2API PRIVATE provider runtime. This is started and supervised by
// the existing Go process on an owner-only Unix socket. No TCP listener, no
// second public API, no separate deployment. Its plugins come from the
// pinned magpie-community MIT reference in ../magpie-plugins/.
import { readFile, rename, writeFile } from "node:fs/promises"
import { ZCodeAuthPlugin } from "../magpie-plugins/zcode/index.mjs"
import {
  QoderAuthPlugin, QoderCNAuthPlugin, QwenWorkAuthPlugin,
  QwenWorkEnterpriseAuthPlugin,
} from "../magpie-plugins/qoder/index.mjs"
import { CodeArtsAuthPlugin } from "../magpie-plugins/codearts/index.mjs"
import { chatToAnthropic, anthroResultToChat, anthroStreamToChat } from "./bridge.mjs"

const SOCKET = process.env.WB2API_PROVIDER_SOCKET
const AUTH_FILE = process.env.WB2API_PROVIDER_AUTH_FILE
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
const pluginInstances = new Map()
const modelCache = new Map()
const roundRobin = new Map()

function providerKeys(id) {
  return Object.keys(credentials)
    .filter(k => k === id || k.startsWith(id + "#"))
    .filter(k => credentials[k] && typeof credentials[k] === "object")
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
  const path = new URL(req.url).pathname
  if (req.method === "GET" && path === "/health") {
    return responses(200, { ready: true, providers: Object.keys(PROVIDERS), credential_count: Object.keys(credentials).length })
  }
  if (req.method === "GET" && path === "/models") return handleModels()
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
  return server
}

if (import.meta.main) await start()
