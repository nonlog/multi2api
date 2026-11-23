import { expect, test } from 'bun:test'
import { _internal as ca } from '../magpie-plugins/codearts/index.mjs'

const credential = {
  access_key_id: 'test-ak', secret_access_key: 'test-sk', security_token: 'test-token',
  session_id: 'test-session',
}
const unavailable = {
  error_code: 'InferHub.APIG.0101.404',
  error_msg: 'The API does not exist or has not been published in the environment',
}
const models = ['deepseek-v4.1-flash', 'glm-5.3-flash']

for (const [index, model] of models.entries()) {
  test(`CodeArts ${model} retries without the unavailable benefit channel`, async () => {
    const state = { benefit: new Set(models), noBenefit: new Set() }
    const sent = []
    const context = {
      ...(await ca.accountSessionIds(credential)),
      gate: ca.createConcurrencyGate(1), state,
      fetcher: async (_url, init) => {
        const benefit = new Headers(init.headers).get('maas_type')
        sent.push(benefit)
        expect(JSON.parse(new TextDecoder().decode(init.body)).model).toBe(model)
        if (benefit) {
          if (index === 0) return Response.json(unavailable, { status: 400 })
          return new Response(`data: ${JSON.stringify(unavailable)}\n\n`, { headers: { 'content-type': 'text/event-stream' } })
        }
        return new Response('data: {"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n', {
          headers: { 'content-type': 'text/event-stream' },
        })
      },
    }
    const chat = () => ca.signedFetch(credential, `${ca.CHAT_API_BASE}/chat/completions`, {
      method: 'POST', body: JSON.stringify({ model, messages: [{ role: 'user', content: 'hello' }] }),
    }, context)
    for (let i = 0; i < 2; i++) {
      const res = await chat()
      expect(res.status).toBe(200)
      expect((await res.json()).choices[0].message.content).toBe('OK')
    }
    expect(sent).toEqual(['benefit', null, null])
    expect(ca.isBenefitModel(models[1 - index], state)).toBe(true)
  })
}
