// WorkBuddy2API PRIVATE provider runtime. This is started and supervised by
// the existing Go process on an owner-only Unix socket. No TCP listener, no
// second public API, no separate deployment. Its plugins come from the
// pinned magpie-community MIT reference in ../magpie-plugins/.
import { readFile, rename, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import { randomUUID } from "node:crypto"
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
const browserLogins = new Map()

function allProviderKeys(id) {
  return Object.keys(credentials).filter(k => k === id || k.startsWith(id + "#"))
    .filter(k => {
      const a = credentials[k]
      return a && typeof a === "object" && !Array.isArray(a) &&
        (typeof a.access === "string" && !!a.access.trim() ||
         typeof a.refresh === "string" && !!a.refresh.trim())
    }).sort()
}

function providerKeys(id) {
  return allProviderKeys(id).filter(k => credentials[k].disabled !== true)
}

function pickAccount(id) {
  const accounts = providerKeys(id)
  if (!accounts.length) return null
  const pos = roundRobin.get(id) || 0
  roundRobin.set(id, pos + 1)
  return accounts[pos % accounts.length]
}

async function changeAuth(mutator) {
  // Serialize refresh-token rotations: single-use tokens must not be
  // accidentally reverted by a concurrent write from a different Provider.
  writing = writing.catch(() => {}).then(async () => {
    mutator(credentials)
    const tmp = AUTH_FILE + ".tmp-" + process.pid
    await writeFile(tmp, JSON.stringify(credentials, null, 2) + "\n", { mode: 0o600 })
    await rename(tmp, AUTH_FILE)
    modelCache.clear()
    usageCache.clear()
    pluginInstances.clear()
  })
  await writing
}

async function saveAuth(key, next, { enable = false } = {}) {
  // Background token refresh retains an explicitly disabled account. A
  // conscious relogin or import restores the account to the usable pool.
  return changeAuth(store => {
    // A late upstream refresh must not resurrect an account removed by the
    // operator while that request was in-flight.
    if (!enable && !Object.hasOwn(store, key)) return
    store[key] = { ...next, disabled: !enable && store[key]?.disabled === true }
  })
}

function saveAccountKey(id, record) {
  // A renewed sign-in replaces the same user, never silently leaves a stale
  // duplicate in rotation. Never synthesize or claim a device identity.
  const uid = String(record.uid || "")
  const accountId = String(record.accountId || "")
  const access = String(record.access || "")
  const existing = allProviderKeys(id).find(key => {
    const a = credentials[key]
    return (uid && String(a.uid || "") === uid) ||
      (accountId && String(a.accountId || "") === accountId) ||
      (access && String(a.access || "") === access)
  })
  if (existing) return existing
  return !Object.hasOwn(credentials, id) ? id : id + "#" + randomUUID().slice(0, 10)
}

function validCredential(value) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    (typeof value.access === "string" && value.access.trim().length > 0) &&
    (value.type === "oauth" || value.type === "api" || value.type === undefined)
}

async function forgetCheckin(key) {
  maintenanceWriting = maintenanceWriting.catch(() => {}).then(async () => {
    delete maintenance.checkins[key]
    const tmp = MAINTENANCE_FILE + ".tmp-" + process.pid
    await writeFile(tmp, JSON.stringify(maintenance, null, 2) + "\n", { mode: 0o600 })
    await rename(tmp, MAINTENANCE_FILE)
  })
  await maintenanceWriting
}

function publicAccount(id, key) {
  const a = credentials[key]
  const note = maintenance.checkins[key]
  const expired = note?.outcome === "failed" && /sign-in has expired|status 401/i.test(note.message || "")
  return {
    provider: id, account: key,
    label: String(a.cardName || a.name || a.email || a.accountId || a.uid || key).slice(0, 120),
    disabled: a.disabled === true, type: a.type || "oauth",
    state: expired ? "login_expired" : "unverified",
    checkin: note?.outcome || "not_run", checkin_supported: maintenanceEligible(id),
  }
}

async function adminAccounts() {
  return responses(200, { providers: Object.keys(PROVIDERS),
    data: Object.keys(PROVIDERS).flatMap(id => allProviderKeys(id).map(key => publicAccount(id, key))) })
}

// The plugin owns the OAuth/PKCE flow. Expose its methods, never reimplement
// protocol headers, token exchanges or synthetic device IDs in the panel.
async function loginMethods(id) {
  if (!PROVIDERS[id]) return []
  const plugin = await PROVIDERS[id]({ client: runtimeClient("__login_" + id) })
  return (plugin.auth?.methods || []).map((m, index) => ({ ...m, index }))
    .filter(m => m.type === "oauth" && typeof m.authorize === "function" &&
      !/app's sign-in/i.test(m.label || ""))
}

async function adminMethods(req) {
  const id = new URL(req.url).searchParams.get("provider")
  if (!PROVIDERS[id]) return responses(400, { error: "未知 Provider" })
  const methods = await loginMethods(id)
  return responses(200, { provider: id, supported: methods.length > 0,
    methods: methods.map(m => ({ id: m.index, label: String(m.label).slice(0, 100),
      prompts: (m.prompts || []).map(p => ({ key: p.key, type: p.type,
        message: p.message, options: p.options, placeholder: p.placeholder })) })) })
}

// Login failures can occur *after* the vendor's web page says "signed in".
// Never return raw upstream response bodies: those may contain tokens/URLs.
const PV_LOGIN_NAMES = {qoder:"Qoder 国际版", "qoder-cn":"Qoder 国内版", zcode:"ZCode", qwenwork:"千问办公", "qwenwork-enterprise":"千问企业", codearts:"CodeArts"}
export function loginFailure(provider, error) {
  const message = String(error?.message || "")
  if (provider === "华为 CodeArts") {
    if (/timed out|timeout/i.test(message)) return { code:"callback_timeout",
      message:"华为授权回调超时；请重新授权并将浏览器最后显示的本地回调地址粘贴到面板。" }
    if (/STS5\.1806|invalid_grant|invalid authorization code/i.test(message)) return { code:"authorization_code_expired",
      message:"华为拒绝了授权码或该授权码已经使用。请重新发起本次授权并粘贴新的回调地址。" }
    return { code:"oauth_exchange_failed", message:"华为 OAuth 令牌交换失败，请重新授权或检查官方平台登录状态。" }
  }
  const job = message.match(/job token[^\n]*?status\s*(\d{3})/i)
  const httpStatus = job ? Number(job[1]) : Number(error?.status)
  if (/timed out|timeout/i.test(message)) return {
    code: "device_timeout", message: `${provider} 设备授权超时。仅登录网页还不够，请在本次授权页面确认设备；也可选择 PAT 登录。`,
  }
  if (job || httpStatus === 401 || httpStatus === 403) return {
    code: "token_exchange", message: `${provider} 授权后未能取得 API 会话令牌${httpStatus ? `（HTTP ${httpStatus}）` : ""}。请尝试 PAT 登录，或退出官网账号后重新授权设备。`,
  }
  if (/\bPAT\b|personal access token/i.test(message)) return {
    code: "pat_rejected", message: `${provider} 拒绝了 PAT，请在官方账号设置中创建新的 PAT 并核对权限。`,
  }
  if (/EACCES|EPERM|ENOSPC|EROFS|write|rename/i.test(message)) return {
    code: "save_failed", message: "授权已返回，但服务器保存私有凭据失败；请检查数据目录权限及空间。",
  }
  if (/request failed|fetch failed|ETIMEDOUT|ECONN|network/i.test(message)) return {
    code: "network", message: `${provider} 授权服务连接失败。请检查 VPS 到官方认证端点的网络或代理配置。`,
  }
  return { code: "vendor_rejected", message: `${provider} 未完成设备令牌交换。请确认网页中已同意授权本次设备；若已同意，请改用 PAT 登录。` }
}

async function adminLoginStart(req) {
  const { provider: id, method: methodIndex = 0, inputs = {} } = await req.json()
  if (!PROVIDERS[id] || !Number.isInteger(methodIndex)) return responses(400, { error: "无效的登录方式" })
  for (const [key, entry] of browserLogins) {
    if (Date.now() - entry.at > 16 * 60_000) {
      entry.cancel?.()
      browserLogins.delete(key)
    }
  }
  if (browserLogins.size >= 12) return responses(429, { error: "登录任务过多，请稍后重试" })
  const methods = await loginMethods(id)
  const method = methods.find(m => m.index === methodIndex)
  if (!method) return responses(400, { error: "该 Provider 不支持此浏览器登录方式，可导入官方凭据" })
  let flow
  try { flow = await method.authorize(inputs) }
  catch { return responses(502, { error: "无法向官方平台发起授权，请使用官方客户端登录后导入凭据" }) }
  if (!flow?.url || !/^https:\/\//i.test(flow.url) || typeof flow.callback !== "function")
    return responses(502, { error: "官方登录未返回安全的授权链接" })
  const state = randomUUID()
  const entry = { provider: id, at: Date.now(), done: false, error: "", code: "", phase: "waiting", account: "",
    submitManual: typeof flow.submitManual === "function" ? flow.submitManual : null,
    cancel: typeof flow.cancel === "function" ? flow.cancel : null }
  browserLogins.set(state, entry)
  // Start the vendor's real PKCE/device callback once, without tying it to
  // the HTTP poll timeout; poll requests only observe the result.
  void (async () => {
    try {
      const result = await flow.callback()
      if (browserLogins.get(state) !== entry) return
      if (result?.type !== "success") throw new Error(String(result?.error || "vendor rejected sign-in").slice(0, 200))
      if (!validCredential(result)) throw new Error("vendor did not return a usable access token")
      entry.phase = "saving"
      const record = { ...result, type: "oauth" }
      const key = saveAccountKey(id, record)
      await saveAuth(key, record, { enable: true })
      await forgetCheckin(key)
      entry.account = key
      entry.done = true
      entry.phase = "done"
    } catch (error) {
      if (browserLogins.get(state) !== entry) return
      const failure = loginFailure(PV_LOGIN_NAMES[id] || id, error)
      entry.code = failure.code
      entry.error = failure.message
      entry.phase = "failed"
    }
  })()
  return responses(200, { state, url: flow.url, provider: id,
    manual_callback: !!entry.submitManual,
    mode: id === "codearts" ? "manual_oauth" : /PAT/i.test(method.label || "") ? "pat" : "device",
    instructions: id === "qoder" && !/PAT/i.test(method.label || "")
      ? "登录 Qoder 网页后，请确认‘授权此设备’。登录官网并不等于设备已授权。"
      : String(flow.instructions || "").slice(0, 180),
  })
}

// OOB handoff for CodeArts' fixed 127.0.0.1 OAuth redirect. The callback
// remains bound to the stateful PKCE/DPoP flow created by this worker; it is
// never used as a generic fetch, redirector, or URL proxy.
async function adminLoginCallback(req) {
  let input
  try { input = await req.json() } catch { return responses(400, {error:"invalid JSON"}) }
  const state = String(input?.state || "")
  const value = input?.url
  if (state.length > 80 || typeof value !== "string" || value.length > 8192)
    return responses(400, {error:"invalid callback input"})
  const entry = browserLogins.get(state)
  if (!entry || entry.provider !== "codearts" || !entry.submitManual)
    return responses(404, {error:"CodeArts authorization session was not found"})
  if (Date.now() - entry.at > 16 * 60_000) {
    entry.cancel?.()
    browserLogins.delete(state)
    return responses(410, {error:"华为授权已过期，请重新开始"})
  }
  if (entry.done || entry.error || entry.phase === "saving")
    return responses(409, {error:"this authorization has already finished"})
  try {
    const outcome = entry.submitManual(value)
    if (outcome.submitted) entry.phase = "saving"
    return responses(200, {ok:true, submitted:outcome.submitted===true,
      ...(outcome.next_url ? {next_url:outcome.next_url} : {})})
  } catch (error) {
    // Only the plugin's fixed validation messages are returned; never include
    // raw callback URL, authorization code or vendor token response bodies.
    return responses(400, {error:String(error?.message || "invalid callback").slice(0,180)})
  }
}

async function adminLoginPoll(req) {
  const { state } = await req.json()
  const entry = browserLogins.get(state)
  if (!entry) return responses(404, { error: "登录会话不存在或已经过期" })
  if (Date.now() - entry.at > 16 * 60_000) {
    entry.cancel?.()
    browserLogins.delete(state)
    return responses(410, { error: "授权已过期，请重新开始" })
  }
  const result = { done: entry.done, error: entry.error, code: entry.code, phase: entry.phase,
    ...(entry.done ? { account: entry.account, provider: entry.provider } : {}) }
  if (entry.done || entry.error) browserLogins.delete(state)
  return responses(200, result)
}

async function adminLoginCancel(req) {
  const { state } = await req.json()
  if (typeof state !== "string" || state.length > 80) return responses(400, {error:"invalid login state"})
  browserLogins.get(state)?.cancel?.()
  browserLogins.delete(state)
  return responses(200, { cancelled: true })
}

async function adminImport(req) {
  const { provider: id, credential } = await req.json()
  if (!PROVIDERS[id] || !validCredential(credential)) return responses(400, { error: "无效的 Provider 或凭据结构" })
  if (JSON.stringify(credential).length > 60_000) return responses(413, { error: "凭据文件过大" })
  const record = { ...credential, type: credential.type || "oauth" }
  const key = saveAccountKey(id, record)
  await saveAuth(key, record, { enable: true })
  await forgetCheckin(key)
  return responses(200, { imported: 1, provider: id, account: key })
}

async function adminAccountAction(req) {
  const { provider: id, account, action } = await req.json()
  if (!PROVIDERS[id] || !allProviderKeys(id).includes(account))
    return responses(404, { error: "账号不存在" })
  if (!["disable", "enable", "remove"].includes(action))
    return responses(400, { error: "无效操作" })
  await changeAuth(store => {
    if (action === "remove") delete store[account]
    else store[account] = { ...store[account], disabled: action === "disable" }
  })
  if (action === "remove") await forgetCheckin(account)
  return responses(200, { ok: true, action, provider: id, account })
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
  for (const k of ["plan", "until", "user", "signIn", "balance", "error"]) {
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
  // Force bypass is only used by the explicit operator refresh button;
  // automatic page refresh respects the 3-minute per-account cache.
  const force = url.searchParams.get("refresh") === "1"
  const ids = requested ? [requested] : Object.keys(PROVIDERS)
  if (ids.some(id => !PROVIDERS[id])) return responses(400, { error: "unknown provider" })
  const accounts = ids.flatMap(id => providerKeys(id).map(key => ({ id, key })))
  const result = new Array(accounts.length)
  // A serial usage scan across ten accounts can exceed the Go gateway's
  // upstream deadline (especially during OAuth refresh). Limit concurrency
  // instead of firing a large burst at every vendor simultaneously.
  let cursor = 0
  const readOne = async () => {
    while (cursor < accounts.length) {
      const index = cursor++
      const { id, key } = accounts[index]
      const cacheKey = id + "\0" + key
      const cached = usageCache.get(cacheKey)
      if (!force && cached && Date.now() - cached.at < 180_000) {
        result[index] = { provider: id, account: key, ...cached.value, cached: true }
        continue
      }
      let value
      try {
        const { plugin, provider } = await configuredPlugin(id, key)
        if (typeof plugin.auth?.usage !== "function") value = { error: "usage query unsupported", windows: [] }
        else {
          const read = await Promise.race([
            plugin.auth.usage(async () => credentials[key], provider),
            new Promise((_, reject) => setTimeout(() => reject(new Error("quota request timeout")), id === "codearts" ? 75_000 : 30_000)),
          ])
          value = publicUsage(read)
        }
      } catch { value = { error: "provider usage request failed", windows: [] } }
      usageCache.set(cacheKey, { value, at: Date.now() })
      result[index] = { provider: id, account: key, ...value }
    }
  }
  await Promise.all(Array.from({ length: Math.min(4, accounts.length) }, readOne))
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
  if (req.method === "GET" && path === "/admin/accounts") return adminAccounts()
  if (req.method === "GET" && path === "/admin/methods") return adminMethods(req)
  if (req.method === "POST" && path === "/admin/login/start") return adminLoginStart(req)
  if (req.method === "POST" && path === "/admin/login/poll") return adminLoginPoll(req)
  if (req.method === "POST" && path === "/admin/login/callback") return adminLoginCallback(req)
  if (req.method === "POST" && path === "/admin/login/cancel") return adminLoginCancel(req)
  if (req.method === "POST" && path === "/admin/import") return adminImport(req)
  if (req.method === "POST" && path === "/admin/account/action") return adminAccountAction(req)
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
