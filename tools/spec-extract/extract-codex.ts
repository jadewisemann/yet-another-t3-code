// Writes spec/generated/codex.json: the JSON-RPC protocol upstream T3 Code
// speaks with `codex app-server`, from the schemas upstream generates for it
// (packages/effect-codex-app-server/src/_generated/meta.gen.ts).
//
//   bun extract-codex.ts <upstream-checkout> <out-dir>
//
// Sections, each method → JSON Schema:
//   clientRequests.params / .results   requests the server (T3) sends to codex
//   clientNotifications                notifications T3 sends to codex
//   serverRequests.params / .results   requests codex sends to T3 (approvals)
//   serverNotifications                notifications codex sends to T3
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { compile, linkEffect, sorted } from "./lib.ts";

const [upstreamArg, outArg] = process.argv.slice(2);
if (!upstreamArg || !outArg) {
  console.error("usage: bun extract-codex.ts <upstream-checkout> <out-dir>");
  process.exit(2);
}
const upstream = resolve(upstreamArg);
const pkg = join(upstream, "packages/effect-codex-app-server");
linkEffect(pkg, dirname(fileURLToPath(import.meta.url)));
const meta = await import(join(pkg, "src/_generated/meta.gen.ts"));

const sections: Record<string, Record<string, any>> = {
  "clientRequests.params": meta.CLIENT_REQUEST_PARAMS,
  "clientRequests.results": meta.CLIENT_REQUEST_RESPONSES,
  clientNotifications: meta.CLIENT_NOTIFICATION_PARAMS,
  "serverRequests.params": meta.SERVER_REQUEST_PARAMS,
  "serverRequests.results": meta.SERVER_REQUEST_RESPONSES,
  serverNotifications: meta.SERVER_NOTIFICATION_PARAMS,
};
const keys: [string, string][] = [];
const schemas: any[] = [];
const noSchema: [string, string][] = [];
for (const [section, map] of Object.entries(sections)) {
  for (const [method, schema] of Object.entries(map ?? {})) {
    // Methods without params (or result) carry no schema upstream.
    if (schema === undefined || schema === null) {
      noSchema.push([section, method]);
      continue;
    }
    keys.push([section, method]);
    schemas.push(schema);
  }
}
const { schemas: compiled, defs } = await compile(schemas);
const out: any = {};
const place = (section: string, method: string, value: any) => {
  const [group, role] = section.split(".") as [string, string | undefined];
  out[group] ??= {};
  if (role) {
    out[group][method] ??= {};
    out[group][method][role] = value;
  } else out[group][method] = value;
};
keys.forEach(([section, method], i) => place(section, method, compiled[i]));
// `null` marks a method that takes no params / returns no result.
for (const [section, method] of noSchema) place(section, method, null);
for (const g of Object.keys(out)) out[g] = sorted(out[g]);

const git = (...args: string[]) =>
  execFileSync("git", ["-C", upstream, ...args], { encoding: "utf8" }).trim();
let tag: string | null = null;
try {
  tag = git("describe", "--tags", "--exact-match");
} catch {}
const doc = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  upstream: { repository: "https://github.com/pingdotgg/t3code", tag, commit: git("rev-parse", "HEAD") },
  ...out,
  $defs: defs,
};
mkdirSync(resolve(outArg), { recursive: true });
writeFileSync(join(resolve(outArg), "codex.json"), JSON.stringify(doc, null, 2) + "\n");
console.error(
  `wrote codex protocol: ${Object.entries(out).map(([g, m]) => `${g} ${Object.keys(m as any).length}`).join(", ")}; ${Object.keys(defs).length} definitions`,
);
