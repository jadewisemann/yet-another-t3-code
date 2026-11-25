// A deterministic stand-in for the `claude` CLI (Claude Code), used by the
// conformance scenarios so that recordings from upstream T3 Code do not
// depend on a real model. It speaks the stream-json protocol that the Claude
// Agent SDK drives: user messages and control requests arrive on stdin as
// JSONL, and system, stream_event, assistant, user and result messages (and
// the CLI's own control requests) leave on stdout.
//
// A user message gets the answer `fake claude reply to: <text>`, streamed as
// one text block. Text starting with:
//   tools   - thinking, a Bash call and a Write call (which really writes
//             hello.txt in the workspace), then the answer;
//   approve - a Bash call that asks permission (can_use_tool) first;
//   ask     - an AskUserQuestion call that asks permission first;
//   wait    - starts the answer and finishes only on an interrupt;
//   slow    - answers after a second (for queueing scenarios).
// Every message in and out is appended to $FAKE_CLAUDE_LOG when it is set.
import { appendFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";

const log = (dir, message) => {
  if (process.env.FAKE_CLAUDE_LOG)
    appendFileSync(process.env.FAKE_CLAUDE_LOG, `${JSON.stringify({ dir, pid: process.pid, message })}\n`);
};
const args = process.argv.slice(2);
log("argv", args);
if (args[0] === "--version" || args[0] === "-v") {
  process.stdout.write("2.1.200 (Claude Code)\n");
  process.exit(0);
}
const flag = (name) => {
  for (let i = 0; i < args.length; i++) {
    if (args[i] === name) return args[i + 1];
    if (args[i].startsWith(`${name}=`)) return args[i].slice(name.length + 1);
  }
  return undefined;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const send = (m) => {
  log("out", m);
  process.stdout.write(`${JSON.stringify(m)}\n`);
};

const model = flag("--model") ?? "claude-opus-5-5";
const permissionMode = flag("--permission-mode") ?? "default";
const sessionId = flag("--session-id") ?? flag("--resume") ?? "00000000-0000-4000-8000-000000000000";
const cwd = process.cwd();
let counter = 0;
const nextId = (prefix) => `${prefix}${String(++counter).padStart(4, "0")}fake`;
const uuid = () => {
  const n = String(++counter).padStart(12, "0");
  return `00000000-0000-4000-8000-${n}`;
};
const base = () => ({ session_id: sessionId, uuid: uuid() });

const usage = { input_tokens: 10, output_tokens: 5, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 };
let initSent = false;
function init() {
  if (initSent) return;
  initSent = true;
  send({
    type: "system", subtype: "init", cwd, session_id: sessionId, tools: ["Bash", "Read", "Write", "Edit", "AskUserQuestion"],
    mcp_servers: [], model, permissionMode, slash_commands: [], apiKeySource: "none",
    claude_code_version: "2.1.200", output_style: "default", agents: [], skills: [], plugins: [], uuid: uuid(),
  });
}

// One assistant API message, streamed block by block: each block is
// { type: "text", text } | { type: "thinking", thinking } | { type: "tool_use", name, input }.
async function assistantMessage(blocks, stopReason) {
  const id = nextId("msg_");
  const message = { id, type: "message", role: "assistant", model, content: [], stop_reason: null, stop_sequence: null, usage };
  const event = (e) => send({ type: "stream_event", event: e, parent_tool_use_id: null, ...base() });
  event({ type: "message_start", message });
  const content = [];
  for (const [index, b] of blocks.entries()) {
    let block;
    if (b.type === "text") {
      block = { type: "text", text: b.text };
      event({ type: "content_block_start", index, content_block: { type: "text", text: "" } });
      event({ type: "content_block_delta", index, delta: { type: "text_delta", text: b.text } });
    } else if (b.type === "thinking") {
      block = { type: "thinking", thinking: b.thinking, signature: "fake-signature" };
      event({ type: "content_block_start", index, content_block: { type: "thinking", thinking: "", signature: "" } });
      event({ type: "content_block_delta", index, delta: { type: "thinking_delta", thinking: b.thinking } });
      event({ type: "content_block_delta", index, delta: { type: "signature_delta", signature: "fake-signature" } });
    } else {
      block = { type: "tool_use", id: nextId("toolu_"), name: b.name, input: b.input };
      event({ type: "content_block_start", index, content_block: { type: "tool_use", id: block.id, name: b.name, input: {} } });
      event({ type: "content_block_delta", index, delta: { type: "input_json_delta", partial_json: JSON.stringify(b.input) } });
    }
    event({ type: "content_block_stop", index });
    content.push(block);
    // The CLI sends each finished block as its own assistant message.
    send({ type: "assistant", message: { ...message, content: [block] }, parent_tool_use_id: null, ...base() });
    await sleep(150);
  }
  event({ type: "message_delta", delta: { stop_reason: stopReason, stop_sequence: null }, usage });
  event({ type: "message_stop" });
  return content;
}

function toolResult(toolUseId, text, isError = false) {
  send({
    type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: toolUseId, content: text, is_error: isError }] },
    parent_tool_use_id: null, ...base(), tool_use_result: { stdout: text, stderr: "", interrupted: false, isImage: false },
  });
}

function result(text, subtype = "success") {
  send({
    type: "result", subtype, is_error: subtype !== "success", duration_ms: 100, duration_api_ms: 80, num_turns: 1,
    ...(subtype === "success" ? { result: text } : {}), stop_reason: "end_turn", session_id: sessionId,
    total_cost_usd: 0, usage, modelUsage: {}, permission_denials: [], uuid: uuid(),
  });
}

// Requests from the CLI to the SDK (can_use_tool) wait for its response.
const waiting = new Map();
function controlRequest(request) {
  const id = nextId("req_");
  send({ type: "control_request", request_id: id, request });
  return new Promise((resolve) => waiting.set(id, resolve));
}

let interrupted = null;
let turnActive = false;
let interruptPending = false; // an interrupt that came before the turn waited
async function turn(text) {
  turnActive = true;
  try {
    await runTurn(text);
  } finally {
    turnActive = false;
    interruptPending = false;
  }
}
async function runTurn(text) {
  init();
  await sleep(10);
  if (text.startsWith("wait")) {
    if (!interruptPending) await new Promise((resolve) => (interrupted = resolve));
    result("", "error_during_execution");
    return;
  }
  if (text.startsWith("tools")) {
    const [, bash] = await assistantMessage([
      { type: "thinking", thinking: "Planning the fake tools turn." },
      { type: "tool_use", name: "Bash", input: { command: "echo hello", description: "Print hello" } },
    ], "tool_use");
    toolResult(bash.id, "hello");
    await sleep(150);
    const file = join(cwd, "hello.txt");
    const [write] = await assistantMessage([{ type: "tool_use", name: "Write", input: { file_path: file, content: "hello\n" } }], "tool_use");
    writeFileSync(file, "hello\n");
    toolResult(write.id, `File created successfully at: ${file}`);
    await sleep(150);
  }
  if (text.startsWith("approve") || text.startsWith("ask")) {
    const ask = text.startsWith("ask");
    const input = ask
      ? { questions: [{ question: "Which color?", header: "Color", multiSelect: false, options: [{ label: "Red", description: "Warm" }, { label: "Blue", description: "Cool" }] }] }
      : { command: "touch approved.txt", description: "Create a file" };
    const name = ask ? "AskUserQuestion" : "Bash";
    const [call] = await assistantMessage([{ type: "tool_use", name, input }], "tool_use");
    const res = await controlRequest({ subtype: "can_use_tool", tool_name: name, input, tool_use_id: call.id, permission_suggestions: [] });
    const allowed = res?.response?.behavior === "allow";
    toolResult(call.id, allowed ? (ask ? `User answered: ${JSON.stringify(res.response.updatedInput?.answers ?? {})}` : "") : (res?.response?.message ?? "denied"), !allowed);
    await sleep(150);
  }
  if (text.startsWith("slow")) await sleep(1000);
  const reply = `fake claude reply to: ${text}`;
  await assistantMessage([{ type: "text", text: reply }], "end_turn");
  result(reply);
}

const controlResponses = {
  initialize: () => ({
    commands: [], output_style: "default", available_output_styles: ["default"],
    models: [{ value: "default", displayName: "Default", description: "Fake default model" }],
    account: { email: "fake@example.com", subscriptionType: "max", tokenSource: "claude.ai" },
  }),
  mcp_set_servers: (r) => ({ added: Object.keys(r.servers ?? {}), removed: [], errors: {} }),
  mcp_status: () => ({ mcpServers: [] }),
  get_settings: () => ({}),
  get_usage: () => ({ rate_limits_available: false, rate_limits: [] }),
};

let queue = Promise.resolve();
const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
  if (!line.trim()) return;
  let m;
  try { m = JSON.parse(line); } catch { return; }
  log("in", m);
  if (m.type === "control_request") {
    const r = m.request ?? {};
    if (r.subtype === "interrupt" && interrupted) {
      const done = interrupted;
      interrupted = null;
      done();
    } else if (r.subtype === "interrupt" && turnActive) {
      interruptPending = true;
    }
    const response = controlResponses[r.subtype]?.(r) ?? {};
    send({ type: "control_response", response: { subtype: "success", request_id: m.request_id, response } });
  } else if (m.type === "control_response") {
    const id = m.response?.request_id;
    const resolve = waiting.get(id);
    waiting.delete(id);
    resolve?.(m.response);
  } else if (m.type === "user") {
    const content = m.message?.content;
    const text = typeof content === "string" ? content : (content ?? []).filter((c) => c.type === "text").map((c) => c.text).join("\n");
    queue = queue.then(() => turn(text));
  }
});
rl.on("close", () => queue.then(() => process.exit(0)));
