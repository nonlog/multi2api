import { test, expect } from "bun:test"
import { chatToAnthropic, anthroResultToChat, anthroStreamToChat } from "./bridge.mjs"

test("OpenAI tools and image tool results survive Chat -> ZCode Messages", () => {
  const source = {
    model: "glm-5.3", stream: true, max_tokens: 1024,
    tools: [{ type: "function", function: {
      name: "read", description: "read a file",
      parameters: { type: "object", properties: { path: { type: "string" } } },
    } }],
    messages: [
      { role: "system", content: "User system instruction. Never replace me." },
      { role: "assistant", content: null, tool_calls: [
        { id: "call_1", type: "function", function: { name: "read", arguments: '{"path":"file.png"}' } },
      ] },
      { role: "tool", tool_call_id: "call_1", content: [
        { type: "text", text: "PNG from read" },
        { type: "image_url", image_url: { url: "data:image/png;base64,iVBORw0KGgoAAAANS" } },
      ] },
    ],
  }
  const v = chatToAnthropic(source)
  expect(v.system).toBe("User system instruction. Never replace me.")
  expect(v.model).toBe("glm-5.3")
  expect(v.tools[0].input_schema.type).toBe("object")
  expect(v.messages[0].content[0]).toEqual({
    type: "tool_use", id: "call_1", name: "read", input: { path: "file.png" },
  })
  const result = v.messages[1].content[0]
  expect(result.tool_use_id).toBe("call_1")
  expect(result.content[1].type).toBe("image")
  expect(result.content[1].source.data).toBe("iVBORw0KGgoAAAANS")
  expect(JSON.stringify(result.content[0])).not.toContain("iVBORw")
})

test("invalid model-generated tool arguments are rejected, not silently executed", () => {
  expect(() => chatToAnthropic({
    model: "glm-5.3",
    messages: [{
      role: "assistant", tool_calls: [{
        id: "x", type: "function", function: { name: "exec", arguments: '{"command":' },
      }],
    }],
  })).toThrow("invalid Chat Completions function arguments")
})

test("Anthropic nonstream result preserves tool ID, name, arguments and usage", () => {
  const result = anthroResultToChat({
    id: "msg-1", stop_reason: "tool_use",
    content: [
      { type: "text", text: "I will inspect it." },
      { type: "tool_use", id: "toolu_1", name: "read", input: { path: "README.md" } },
    ],
    usage: { input_tokens: 51, output_tokens: 10 },
  }, "zcode:glm-5.3")
  expect(result.model).toBe("zcode:glm-5.3")
  expect(result.choices[0].finish_reason).toBe("tool_calls")
  const tc = result.choices[0].message.tool_calls[0]
  expect(tc.id).toBe("toolu_1")
  expect(tc.function.name).toBe("read")
  expect(JSON.parse(tc.function.arguments).path).toBe("README.md")
  expect(result.usage.total_tokens).toBe(61)
})

test("Anthropic streamed text and tool JSON convert into complete Chat SSE", async () => {
  const records = [
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "Preparing." } },
    { type: "content_block_start", index: 1, content_block: { type: "tool_use", name: "read", id: "toolu_9" } },
    { type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: '{"path":"' } },
    { type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: 'README.md"}' } },
    { type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { input_tokens: 9, output_tokens: 5 } },
    { type: "message_stop" },
  ]
  const source = records.map(e => "event: " + e.type + "\ndata: " + JSON.stringify(e) + "\n\n").join("")
  // Real providers split SSE frames at arbitrary network byte boundaries.
  const bytes = new TextEncoder().encode(source)
  const stream = new ReadableStream({
    start(controller) {
      for (let i = 0; i < bytes.length; i += 19) {
        controller.enqueue(bytes.slice(i, i + 19))
      }
      controller.close()
    },
  })
  const out = await new Response(anthroStreamToChat(stream, "zcode:glm-5.3")).text()
  expect(out).toContain("data: [DONE]")
  const events = out.split("\n").filter(x => x.startsWith("data: {")).map(x => JSON.parse(x.slice(6)))
  expect(events.some(e => e.choices[0].delta.content === "Preparing.")).toBe(true)
  expect(events.some(e => e.choices[0].delta.tool_calls?.[0].id === "toolu_9")).toBe(true)
  const args = events.filter(e => e.choices[0].delta.tool_calls?.[0]?.function?.arguments !== undefined)
    .map(e => e.choices[0].delta.tool_calls[0].function.arguments).join("")
  expect(JSON.parse(args).path).toBe("README.md")
  expect(events.some(e => e.choices[0].finish_reason === "tool_calls")).toBe(true)
})
