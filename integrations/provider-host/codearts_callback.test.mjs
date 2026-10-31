import { test, expect } from 'bun:test'
import { _internal as ca, CodeArtsAuthPlugin } from '../magpie-plugins/codearts/index.mjs'

const validToken = (refresh='refresh-new') => ({
  credentials: {
    access_key_id: 'temporary-ak', secret_access_key: 'temporary-sk',
    security_token: 'short-lived-st', expiration: new Date(Date.now()+3600_000).toISOString(),
  }, refresh_token: refresh,
})

// This exercises the REAL PKCE/DPoP + callback-to-STS code path but substitutes
// a fake STS response; no Huawei account or network dependency is needed.
test('remote CodeArts OOB callback exchanges code, rejects mismatched port and unsafe redirects', async () => {
  let exchanged = 0
  const fetcher = async (url) => {
    expect(url).toBe(ca.STS_TOKEN_ENDPOINT)
    exchanged++
    return new Response(JSON.stringify(validToken()), {status:200,headers:{'content-type':'application/json'}})
  }
  const flow = await ca.startOAuthFlow(fetcher)
  try {
    const port = Number(new URL(flow.loginUrl).searchParams.get('port'))
    expect(port).toBeGreaterThanOrEqual(10000)
    const callback = `http://127.0.0.1:${port}/oauth/callback`
    expect(() => flow.submitManual(`http://127.0.0.1:${port+1}/oauth/callback?code=wrong`)).toThrow(/port/)
    expect(() => flow.submitManual(`https://evil.example/oauth/callback?code=wrong`)).toThrow(/host/)
    expect(() => flow.submitManual(`http://127.0.0.1:${port}/evil?code=wrong`)).toThrow(/path/)
    expect(() => flow.submitManual(`${callback}?secret=opaque&redirect=https%3A%2F%2Fevil.example%2Fhijack`)).toThrow(/portal/)
    const first = flow.submitManual(`${callback}?secret=opaque&redirect=${encodeURIComponent('https://codearts.huaweicloud.com/portal/login?step=2')}`)
    expect(first.next_url.startsWith('https://codearts.huaweicloud.com/')).toBe(true)
    expect(exchanged).toBe(0)
    const final = flow.submitManual(`${callback}?code=valid-authorization-code`)
    expect(final.submitted).toBe(true)
    const result = await flow.result
    const credential = JSON.parse(result.access)
    expect(credential.access_key_id).toBe('temporary-ak')
    expect(exchanged).toBe(1)
    expect(() => flow.submitManual(`${callback}?code=duplicate`)).toThrow(/already/)
  } finally { await flow.close() }
})

test('CodeArts remote manual callback accepts a bare code without contacting arbitrary URLs', () => {
  const p = ca.normalizeManualCallback('simple-test-code', 16345)
  expect(p.get('code')).toBe('simple-test-code')
  expect(() => ca.normalizeManualCallback('http://127.0.0.1:9999/oauth/callback?code=foo', 16345)).toThrow()
  expect(() => ca.normalizeManualCallback('http://localhost:16345/oauth/callback?code=foo', 16345)).toThrow()
})

test('expired CodeArts quota credential renews once before signed usage read and persists refresh rotation', async () => {
  const keyPair = await ca.generateDpopKeyPair()
  const credential = {
    access_key_id:'old-ak',secret_access_key:'old-sk',security_token:'old-st',
    refresh_token:'old-refresh',code_verifier:'test-verifier',dpop_private_key_jwk:keyPair.privateKeyJwk,
    expires_at:new Date(Date.now()-5000).toISOString(),session_id:'stable-test-session',user_phone:'138****1234',
  }
  let account={type:'oauth',access:JSON.stringify(credential),refresh:'old-refresh',accountId:'test-account'}
  let rotations=0, stats=0, saved=0
  const plugin=await CodeArtsAuthPlugin({client:{auth:{async set({body}){saved++;account=body}}}}, {
    fetch:async (url, init) => {
      const u=String(url)
      if (u===ca.STS_TOKEN_ENDPOINT) {rotations++;return new Response(JSON.stringify(validToken()),{status:200})}
      if (u===ca.SNAP_STATISTICS_URL) {stats++;return new Response(JSON.stringify({code:0,data:{package:{package_name_cn:'单元测试套餐'},metrics:[]}}),{status:200})}
      return new Response(JSON.stringify({code:404,message:'not in test'}),{status:404})
    },
    ledgerPath:'/dev/null',
  })
  const usage=await plugin.auth.usage(async()=>account)
  expect(usage.error).toBeUndefined()
  expect(usage.plan).toBe('单元测试套餐')
  expect(rotations).toBe(1)
  expect(stats).toBe(1)
  expect(saved).toBe(1)
  expect(JSON.parse(account.access).access_key_id).toBe('temporary-ak')
})

test('quota statistics error preserves HTTP status instead of generic provider error', async()=>{
  const credential={access_key_id:'fresh-ak',secret_access_key:'fresh-sk',security_token:'fresh-st',
    expires_at:new Date(Date.now()+3600_000).toISOString(),session_id:'another-test-session',user_phone:'138****1234'}
  const plugin=await CodeArtsAuthPlugin({}, {fetch:async()=>new Response(JSON.stringify({error_code:'API.TEST',error_msg:'forbidden'}),{status:403})})
  const usage=await plugin.auth.usage(async()=>({type:'oauth',access:JSON.stringify(credential)}))
  expect(usage.error).toContain('HTTP 403')
  expect(usage.error).toContain('API.TEST')
})
