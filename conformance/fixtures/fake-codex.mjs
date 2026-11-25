// A deterministic stand-in for the `codex` CLI, used by the conformance
// scenarios so that recordings from upstream T3 Code do not depend on a real
// model. It speaks the app-server JSON-RPC protocol (JSONL over stdio).
//
// Responses and notifications are the smallest values that satisfy
// spec/generated/codex.json, with the fields that matter overridden below.
// Every message in and out is appended to $FAKE_CODEX_LOG when it is set.
import { appendFileSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const spec = JSON.parse(readFileSync(join(here, "../../spec/generated/codex.json"), "utf8"));
const log = (dir, message) => {
  if (process.env.FAKE_CODEX_LOG)
    appendFileSync(process.env.FAKE_CODEX_LOG, `${JSON.stringify({ dir, pid: process.pid, message })}\n`);
};

const args = process.argv.slice(2);
if (args[0] === "--version") {
  process.stdout.write("codex-cli 0.200.0\n");
  process.exit(0);
}
if (args[0] === "app-server" && args.includes("--help")) {
  process.stdout.write("Usage: codex app-server [OPTIONS]\n\nOptions:\n  --listen <URL>\n  -h, --help\n");
  process.exit(0);
}
if (args[0] !== "app-server") {
  log("argv", args);
  process.stderr.write(`fake codex: unsupported arguments: ${args.join(" ")}\n`);
  process.exit(2);
}

// --- minimal instances of a JSON Schema -------------------------------------
const deref = (s) => {
  while (s && s.$ref) s = spec.$defs[s.$ref.split("/").pop()];
  return s;
};
// `hint` is the value the caller wants (it wins over the minimal one) and
// picks the anyOf branch whose constant fields it matches.
function minimal(schema, hint, depth = 0) {
  const s = deref(schema);
  if (hint !== undefined && (hint === null || typeof hint !== "object" || Array.isArray(hint))) return hint;
  if (!s || depth > 40) return hint ?? null;
  if ("const" in s) return s.const;
  if (s.enum) return s.enum[0];
  for (const k of ["anyOf", "oneOf"]) {
    if (s[k]) {
      const opts = s[k].map(deref);
      const fits = (o) =>
        hint && o && o.properties &&
        Object.entries(hint).every(([f, v]) => {
          const p = deref(o.properties[f]);
          return !p || !("const" in p || p.enum) || p.const === v || (p.enum ?? []).includes(v);
        }) &&
        Object.keys(hint).some((f) => { const p = deref(o.properties[f]); return p && ("const" in p || p.enum); });
      const nul = opts.find((o) => o && o.type === "null");
      const pick = (hint && opts.find(fits)) ?? (hint ? opts.find((o) => o && o.type === "object") : nul) ?? opts[0];
      return minimal(pick, hint, depth + 1);
    }
  }
  const type = Array.isArray(s.type) ? (s.type.includes("null") ? "null" : s.type[0]) : s.type;
  if (!type && s.allOf) return Object.assign({}, ...s.allOf.map((x) => minimal(x, hint, depth + 1)));
  switch (type) {
    case "null":
      return null;
    case "boolean":
      return false;
    case "integer":
    case "number":
      return 0;
    case "string":
      return "";
    case "array":
      return Array.from({ length: s.minItems ?? 0 }, () => minimal(s.items, undefined, depth + 1));
    default: {
      const out = {};
      for (const k of s.required ?? []) out[k] = minimal(s.properties?.[k] ?? s.additionalProperties, hint?.[k], depth + 1);
      for (const [k, v] of Object.entries(hint ?? {}))
        out[k] = minimal(s.properties?.[k] ?? (typeof s.additionalProperties === "object" ? s.additionalProperties : undefined), v, depth + 1);
      return out;
    }
  }
}
const resultOf = (method, over) => minimal(spec.clientRequests[method]?.results, over);
const notificationOf = (method, over) => minimal(spec.serverNotifications[method], over);

// --- wire --------------------------------------------------------------------
const write = (message) => {
  log("out", message);
  process.stdout.write(`${JSON.stringify(message)}\n`);
};
const respond = (id, result) => write({ id, result });
const notify = (method, params) => write({ method, params });

const notifyLater = (f, ms) => setTimeout(f, ms);
let threadCounter = 0;
let turnCounter = 0;
const threads = new Map();
const pending = new Map(); // turns waiting for turn/interrupt

const nowSec = () => Math.floor(Date.now() / 1000);
const thread = (id, cwd) => ({ id, cwd: cwd ?? process.cwd(), preview: "", createdAt: nowSec(), updatedAt: nowSec() });

function startTurn(id, params) {
  const threadId = params.threadId;
  const turnId = `fake-turn-${++turnCounter}`;
  const text = (params.input ?? []).map((i) => i.text ?? "").join("");
  const reply = `fake codex reply to: ${text}`;
  const turn = { id: turnId, items: [], status: "inProgress" };
  respond(id, resultOf("turn/start", { turn }));
  const itemId = `fake-item-${turnCounter}`;
  if (text.startsWith("wait")) {
    // Runs until interrupted (turn/interrupt), for stop scenarios.
    setTimeout(() => {
      notify("turn/started", notificationOf("turn/started", { threadId, turn }));
      pending.set(turnId, { threadId, turn });
    }, 10);
    return;
  }
  // A turn whose text starts with "slow" takes a second, for queueing scenarios.
  const delay = text.startsWith("slow") ? 1000 : 10;
  notifyLater(() => notify("turn/started", notificationOf("turn/started", { threadId, turn })), 10);
  setTimeout(() => {
    const item = { type: "agentMessage", id: itemId, text: "" };
    const startedAtMs = Date.now();
    notify("item/started", notificationOf("item/started", { threadId, turnId, item, startedAtMs }));
    notify("item/agentMessage/delta", notificationOf("item/agentMessage/delta", { threadId, turnId, itemId, delta: reply }));
    notify("item/completed", notificationOf("item/completed", { threadId, turnId, item: { ...item, text: reply }, startedAtMs, completedAtMs: Date.now() }));
    notify("turn/completed", notificationOf("turn/completed", { threadId, turn: { ...turn, status: "completed" } }));
  }, delay);
}

function handle(message) {
  const { id, method, params = {} } = message;
  switch (method) {
    case "initialize":
      return respond(id, {
        userAgent: "fake-codex/0.200.0",
        codexHome: process.env.CODEX_HOME ?? process.cwd(),
        platformFamily: "unix",
        platformOs: "linux",
      });
    case "initialized":
      return;
    case "account/read":
      return respond(id, resultOf("account/read", { account: { type: "apiKey" }, requiresOpenaiAuth: false }));
    case "thread/start":
    case "thread/resume":
    case "thread/fork": {
      const tid = params.threadId && method === "thread/resume" ? params.threadId : `fake-thread-${++threadCounter}`;
      threads.set(tid, params);
      return respond(
        id,
        resultOf(method, {
          thread: thread(tid, params.cwd),
          cwd: params.cwd ?? process.cwd(),
          model: params.model ?? "gpt-fake",
          modelProvider: "openai",
        }),
      );
    }
    case "turn/start":
      return startTurn(id, params);
    case "turn/interrupt": {
      respond(id, resultOf("turn/interrupt", {}));
      const p = pending.get(params.turnId);
      if (p) {
        pending.delete(params.turnId);
        notify("turn/completed", notificationOf("turn/completed", { threadId: p.threadId, turn: { ...p.turn, status: "interrupted" } }));
      }
      return;
    }
    default:
      if (id === undefined) return;
      if (spec.clientRequests[method]) return respond(id, resultOf(method, {}));
      return write({ id, error: { code: -32601, message: `fake codex: unhandled request ${method}` } });
  }
}

let rest = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  rest += chunk;
  const lines = rest.split("\n");
  rest = lines.pop() ?? "";
  for (const line of lines) {
    if (!line.trim()) continue;
    const message = JSON.parse(line);
    log("in", message);
    if ("method" in message) handle(message);
  }
});
process.stdin.on("end", () => process.exit(0));
