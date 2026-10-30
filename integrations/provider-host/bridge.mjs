// The external provider plugins are maintained upstream by magpie-community.
// This file contains only the WorkBuddy2API request/response protocol bridge.
// It deliberately does not add or replace any system/developer instructions.

const encode = (value) => JSON.stringify(value)
const text = (part) => typeof part === "string" ? part : String(part ?? "")

function asAnthropicPart(part) {
  if (!part || typeof part !== "object") return { type: "text", text: text(part) }
  if (part.type === "text" || part.type === "input_text") return { type: "text", text: text(part.text) }
  if (part.type === "image_url" || part.type === "input_image") {
    const url = typeof part.image_url === "string" ? part.image_url : part.image_url?.url || part.image_url
    if (typeof url !== "string") throw new Error("image_url is missing")
    const m = /^data:(image\/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/=]+)$/i.exec(url)
    if (m) return { type: "image", source: { type: "base64", media_type: m[1], data: m[2] } }
    if (/^https:\/\//i.test(url)) return { type: "image", source: { type: "url", url } }
    throw new Error("unsupported image URL: expected HTTPS or data:image/*;base64")
  }
  throw new Error("unsupported OpenAI content part type: " + part.type)
}

function asAnthropicContent(content) {
  if (typeof content === "string") return [{ type: "text", text: content }]
  if (Array.isArray(content)) return content.map(asAnthropicPart)
  if (content == null) return []
  throw new Error("unsupported Chat Completions message content")
}

function toolArguments(raw) {
  if (raw && typeof raw === "object") return raw
  try {
    const parsed = JSON.parse(String(raw || "{}"))
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) return parsed
  } catch {}
  throw new Error("invalid Chat Completions function arguments")
}

// ZCode's plugin accepts Anthropic Messages rather than Chat Completions.
// Preserve tool-call IDs so Codex/Claude can continue the same conversation.
export function chatToAnthropic(chat) {
  const system = []
  const messages = []
  for (const message of chat.messages || []) {
    if (message.role === "system" || message.role === "developer") {
      system.push(asAnthropicContent(message.content).map(p => p.text ?? "").join("\n"))
      continue
    }
    if (message.role === "tool") {
      messages.push({
        role: "user",
        content: [{
          type: "tool_result", tool_use_id: String(message.tool_call_id || ""),
          content: asAnthropicContent(message.content),
          ...(message.is_error ? { is_error: true } : {}),
        }],
      })
      continue
    }
    if (message.role !== "user" && message.role !== "assistant") continue
    const content = asAnthropicContent(message.content)
    if (message.role === "assistant") {
      for (const call of message.tool_calls || []) {
        if (call.type !== "function") continue
        content.push({
          type: "tool_use", id: String(call.id || ""),
          name: String(call.function?.name || ""),
          input: toolArguments(call.function?.arguments),
        })
      }
    }
    messages.push({ role: message.role, content })
  }
  const tools = (chat.tools || [])
    .filter(t => t?.type === "function" && t.function?.name)
    .map(t => ({
      name: t.function.name, description: t.function.description || "",
      input_schema: t.function.parameters || { type: "object", properties: {} },
    }))
  const result = {
    model: chat.model,
    messages,
    stream: chat.stream === true,
    max_tokens: Number.isFinite(chat.max_tokens) && chat.max_tokens > 0
      ? Math.floor(chat.max_tokens) : 8192,
    ...(system.length ? { system: system.join("\n\n") } : {}),
    ...(tools.length ? { tools } : {}),
  }
  for (const field of ["temperature", "top_p"]) {
    if (chat[field] !== undefined) result[field] = chat[field]
  }
  if (chat.stop) result.stop_sequences = Array.isArray(chat.stop) ? chat.stop : [chat.stop]
  if (chat.tool_choice === "required") result.tool_choice = { type: "any" }
  else if (chat.tool_choice === "none") result.tool_choice = { type: "none" }
  else if (chat.tool_choice?.type === "function") {
    result.tool_choice = { type: "tool", name: chat.tool_choice.function?.name }
  }
  return result
}

export function anthroStopReason(reason) {
  return reason === "tool_use" ? "tool_calls"
    : reason === "max_tokens" ? "length" : "stop"
}

export function anthroResultToChat(message, model) {
  const toolCalls = []
  const pieces = []
  const thoughts = []
  for (const item of message.content || []) {
    if (item.type === "text") pieces.push(String(item.text || ""))
    if (item.type === "thinking") thoughts.push(String(item.thinking || ""))
    if (item.type === "tool_use") {
      toolCalls.push({
        type: "function", id: item.id,
        function: { name: item.name, arguments: encode(item.input || {}) },
      })
    }
  }
  const out = {
    id: message.id || "chatcmpl-bridge", object: "chat.completion",
    created: Math.floor(Date.now() / 1000), model,
    choices: [{
      index: 0, message: {
        role: "assistant", content: pieces.join(""),
        ...(thoughts.length ? { reasoning_content: thoughts.join("") } : {}),
        ...(toolCalls.length ? { tool_calls: toolCalls } : {}),
      },
      finish_reason: anthroStopReason(message.stop_reason),
    }],
  }
  if (message.usage) {
    const prompt = Number(message.usage.input_tokens || 0)
    const completion = Number(message.usage.output_tokens || 0)
    out.usage = {
      prompt_tokens: prompt, completion_tokens: completion,
      total_tokens: prompt + completion,
    }
  }
  return out
}

// Transform Anthropic SSE into OpenAI Chat SSE, including streamed tool
// arguments. Keep it truly streaming: do not buffer an entire coding turn.
export function anthroStreamToChat(source, model) {
  const reader = source.getReader()
  const decoder = new TextDecoder()
  const encoder = new TextEncoder()
  let remainder = ""
  let done = false
  let callIndex = 0
  let promptTokens = 0
  const blocks = new Map()
  const emit = (v) => encoder.encode("data: " + encode(v) + "\n\n")
  return new ReadableStream({
    async pull(controller) {
      if (done) {
        controller.close()
        return
      }
      try {
        while (true) {
          const i = remainder.indexOf("\n\n")
          if (i < 0) {
            const next = await reader.read()
            if (next.done) {
              done = true
              controller.enqueue(encoder.encode("data: [DONE]\n\n"))
              controller.close()
              return
            }
            remainder += decoder.decode(next.value, { stream: true })
            // Normalize after joining chunks: CRLF can straddle reads.
            remainder = remainder.replaceAll("\r\n", "\n")
            continue
          }
          const frame = remainder.slice(0, i)
          remainder = remainder.slice(i + 2)
          const line = frame.split("\n").find(v => v.startsWith("data: "))
          if (!line) continue
          let event
          try { event = JSON.parse(line.slice(6)) } catch { continue }
          const kind = event.type
          const delta = { role: "assistant" }
          if (kind === "message_start") {
            promptTokens = Number(event.message?.usage?.input_tokens || 0)
            continue
          } else if (kind === "content_block_start") {
            const block = event.content_block || {}
            if (block.type === "tool_use") {
              const index = callIndex++
              blocks.set(event.index, { type: "tool", index })
              delta.tool_calls = [{
                index, id: block.id, type: "function",
                function: { name: block.name, arguments: "" },
              }]
            } else blocks.set(event.index, { type: block.type || "text" })
          } else if (kind === "content_block_delta") {
            const data = event.delta || {}
            const block = blocks.get(event.index)
            if (data.type === "text_delta") delta.content = data.text || ""
            else if (data.type === "thinking_delta") delta.reasoning_content = data.thinking || ""
            else if (data.type === "input_json_delta" && block?.type === "tool") {
              delta.tool_calls = [{
                index: block.index, function: { arguments: data.partial_json || "" },
              }]
            } else continue
          } else if (kind === "message_delta") {
            controller.enqueue(emit({
              id: "chatcmpl-bridge", object: "chat.completion.chunk", model,
              choices: [{ index: 0, delta: {}, finish_reason: anthroStopReason(event.delta?.stop_reason) }],
              ...(event.usage ? {
                usage: {
                  prompt_tokens: promptTokens,
                  completion_tokens: Number(event.usage.output_tokens || 0),
                  total_tokens: promptTokens + Number(event.usage.output_tokens || 0),
                },
              } : {}),
            }))
            return
          } else if (kind === "message_stop") {
            done = true
            controller.enqueue(encoder.encode("data: [DONE]\n\n"))
            controller.close()
            return
          } else if (kind === "error") {
            controller.enqueue(emit({ error: event.error || { message: "upstream stream error" } }))
            return
          } else continue
          controller.enqueue(emit({
            id: "chatcmpl-bridge", object: "chat.completion.chunk", model,
            choices: [{ index: 0, delta, finish_reason: null }],
          }))
          return
        }
      } catch (err) {
        controller.error(err)
      }
    },
    cancel(reason) { return reader.cancel(reason) },
  })
}
