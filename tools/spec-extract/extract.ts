// Reads the RPC contracts of an upstream T3 Code checkout and writes a
// language-neutral spec: every WebSocket RPC method with JSON Schemas
// (draft 2020-12) for its payload, success value, and errors, sharing one
// `$defs` table.
//
//   bun extract.ts <upstream-checkout> <out-dir>
//
// The schemas describe the JSON wire form (the encoded side of
// Schema.toCodecJson), which is exactly what a server in another language
// has to produce and accept.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const [upstreamArg, outArg] = process.argv.slice(2);
if (!upstreamArg || !outArg) {
  console.error("usage: bun extract.ts <upstream-checkout> <out-dir>");
  process.exit(2);
}
const upstream = resolve(upstreamArg);
const outDir = resolve(outArg);
const here = dirname(fileURLToPath(import.meta.url));

// The contracts package only depends on `effect`. Point it at our pinned copy
// so the extractor does not need a full pnpm install of the monorepo.
const contracts = join(upstream, "packages/contracts");
const linkedEffect = join(contracts, "node_modules/effect");
if (!existsSync(linkedEffect)) {
  mkdirSync(dirname(linkedEffect), { recursive: true });
  symlinkSync(join(here, "node_modules/effect"), linkedEffect, "dir");
}

const Schema = await import("effect/Schema");
const SchemaRepresentation = await import("effect/SchemaRepresentation");
const RpcSchema = await import("effect/rpc/RpcSchema");
const { WsRpcGroup } = await import(join(contracts, "src/rpc.ts"));

type Root = { method: string; role: string; schema: any };
const roots: Root[] = [];
const methods: Record<string, any> = {};

for (const [tag, rpc] of WsRpcGroup.requests as Map<string, any>) {
  const stream = RpcSchema.isStreamSchema(rpc.successSchema);
  const success = stream ? rpc.successSchema.success : rpc.successSchema;
  const error = stream
    ? Schema.Union([rpc.errorSchema, rpc.successSchema.error])
    : rpc.errorSchema;
  const entry: any = {
    kind: stream ? "stream" : "unary",
    middlewares: [...(rpc.middlewares ?? [])].map((m: any) => m.key ?? String(m)).sort(),
  };
  for (const [role, schema] of [
    ["payload", rpc.payloadSchema],
    ["success", success],
    ["error", error],
  ] as const) {
    entry[role] = { $ref: `#/$roots/${roots.length}` };
    roots.push({ method: tag, role, schema });
  }
  methods[tag] = entry;
}

const representation = SchemaRepresentation.toRepresentations(
  roots.map((r) => Schema.toCodecJson(r.schema).ast) as any,
);
const multi: any = SchemaRepresentation.toJsonSchemaMultiDocument(representation, {
  generateDescriptions: true,
});

// Inline each root into its method entry so the file reads method by method.
for (const [tag, entry] of Object.entries(methods)) {
  for (const role of ["payload", "success", "error"]) {
    const index = Number(entry[role].$ref.split("/").pop());
    entry[role] = multi.schemas[index];
  }
  methods[tag] = entry;
}

// The compiler inlines every anonymous struct, which makes the document huge
// (about 10 MB) and noisy to diff. Move every large subtree that occurs more
// than once into `$defs` under a name derived from its content, so the same
// shape always gets the same name and unrelated upstream changes do not
// renumber anything.
const canonical = (v: any): string =>
  Array.isArray(v)
    ? `[${v.map(canonical).join(",")}]`
    : v && typeof v === "object"
      ? `{${Object.keys(v)
          .sort()
          .map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`)
          .join(",")}}`
      : JSON.stringify(v);

// Only subschemas are candidates: keyword maps such as `properties` and data
// such as `enum` values must stay where they are.
const MAP_KEYWORDS = new Set(["properties", "patternProperties", "$defs", "dependentSchemas"]);
const ONE_KEYWORDS = new Set([
  "items", "additionalProperties", "not", "contains", "propertyNames", "if", "then", "else",
  "unevaluatedItems", "unevaluatedProperties", "contentSchema",
]);
const LIST_KEYWORDS = new Set(["anyOf", "oneOf", "allOf", "prefixItems"]);
const mapSubschemas = (node: any, f: (schema: any) => any): any => {
  if (!node || typeof node !== "object" || Array.isArray(node)) return node;
  const out: any = {};
  for (const [k, v] of Object.entries(node)) {
    if (MAP_KEYWORDS.has(k) && v && typeof v === "object")
      out[k] = Object.fromEntries(Object.entries(v).map(([n, sub]) => [n, f(sub)]));
    else if (LIST_KEYWORDS.has(k) && Array.isArray(v)) out[k] = v.map(f);
    else if (ONE_KEYWORDS.has(k)) out[k] = Array.isArray(v) ? v.map(f) : f(v);
    else out[k] = v;
  }
  return out;
};

const counts = new Map<string, number>();
const countAll = (schema: any): void => {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) return;
  const c = canonical(schema);
  counts.set(c, (counts.get(c) ?? 0) + 1);
  mapSubschemas(schema, (sub) => (countAll(sub), sub));
};
const MIN_SHARED_LENGTH = 160;
const shared: Record<string, any> = {};
const share = (schema: any): any => {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) return schema;
  const c = canonical(schema);
  const out = mapSubschemas(schema, share);
  if ((counts.get(c) ?? 0) > 1 && c.length >= MIN_SHARED_LENGTH) {
    const name = `s_${createHash("sha256").update(c).digest("hex").slice(0, 12)}`;
    shared[name] = out;
    return { $ref: `#/$defs/${name}` };
  }
  return out;
};
const namedDefs = multi.definitions ?? {};
for (const m of Object.values(methods)) for (const r of ["payload", "success", "error"]) countAll(m[r]);
for (const d of Object.values(namedDefs)) countAll(d);
for (const m of Object.values(methods))
  for (const r of ["payload", "success", "error"]) m[r] = share(m[r]);
for (const k of Object.keys(namedDefs)) namedDefs[k] = share(namedDefs[k]);

const sorted = (o: Record<string, any>) =>
  Object.fromEntries(Object.keys(o).sort().map((k) => [k, o[k]]));

const git = (...args: string[]) =>
  execFileSync("git", ["-C", upstream, ...args], { encoding: "utf8" }).trim();
const exactTag = () => {
  try {
    return git("describe", "--tags", "--exact-match");
  } catch {
    return null;
  }
};
// apps/server/package.json lags behind: releases set the real version at
// build time, so the tag is the version that matters.
const pkg = await import(join(upstream, "apps/server/package.json"), { with: { type: "json" } });

const spec = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  upstream: {
    repository: "https://github.com/pingdotgg/t3code",
    tag: exactTag(),
    commit: git("rev-parse", "HEAD"),
    committedAt: git("log", "-1", "--format=%cI"),
    packageVersion: pkg.default.version,
  },
  methods: sorted(methods),
  $defs: sorted({ ...namedDefs, ...shared }),
};

mkdirSync(outDir, { recursive: true });
writeFileSync(join(outDir, "rpc.json"), JSON.stringify(spec, null, 2) + "\n");

// A compact index that is easy to diff and to review by eye.
const index = Object.entries(spec.methods)
  .map(([tag, m]: [string, any]) => `${tag}\t${m.kind}\t${m.middlewares.join(",")}`)
  .join("\n");
writeFileSync(join(outDir, "rpc-methods.tsv"), "method\tkind\tmiddlewares\n" + index + "\n");

console.error(
  `wrote ${Object.keys(spec.methods).length} methods and ${Object.keys(spec.$defs).length} definitions`,
);
