// Shared by the extractors: compiles Effect Schemas into one JSON Schema
// document with content-addressed shared definitions.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, symlinkSync } from "node:fs";
import { dirname, join } from "node:path";

/** Points an upstream package at our pinned `effect` so no pnpm install is needed. */
export function linkEffect(packageDir: string, toolDir: string) {
  const linked = join(packageDir, "node_modules/effect");
  if (!existsSync(linked)) {
    mkdirSync(dirname(linked), { recursive: true });
    symlinkSync(join(toolDir, "node_modules/effect"), linked, "dir");
  }
}

export const canonical = (v: any): string =>
  Array.isArray(v)
    ? `[${v.map(canonical).join(",")}]`
    : v && typeof v === "object"
      ? `{${Object.keys(v)
          .sort()
          .map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`)
          .join(",")}}`
      : JSON.stringify(v);

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

export const sorted = (o: Record<string, any>) =>
  Object.fromEntries(Object.keys(o).sort().map((k) => [k, o[k]]));

/**
 * Compiles schemas (in order) to JSON Schemas of their JSON wire form and
 * returns them with one shared `$defs` table. Large subtrees that occur more
 * than once move into `$defs` under a name derived from their content.
 */
export async function compile(schemas: any[]): Promise<{ schemas: any[]; defs: Record<string, any> }> {
  const Schema = await import("effect/Schema");
  const SchemaRepresentation = await import("effect/SchemaRepresentation");
  const representation = SchemaRepresentation.toRepresentations(
    schemas.map((s) => Schema.toCodecJson(s).ast) as any,
  );
  const multi: any = SchemaRepresentation.toJsonSchemaMultiDocument(representation, {
    generateDescriptions: true,
  });
  const counts = new Map<string, number>();
  const countAll = (schema: any): void => {
    if (!schema || typeof schema !== "object" || Array.isArray(schema)) return;
    const c = canonical(schema);
    counts.set(c, (counts.get(c) ?? 0) + 1);
    mapSubschemas(schema, (sub) => (countAll(sub), sub));
  };
  const shared: Record<string, any> = {};
  const share = (schema: any): any => {
    if (!schema || typeof schema !== "object" || Array.isArray(schema)) return schema;
    const c = canonical(schema);
    const out = mapSubschemas(schema, share);
    if ((counts.get(c) ?? 0) > 1 && c.length >= 160) {
      const name = `s_${createHash("sha256").update(c).digest("hex").slice(0, 12)}`;
      shared[name] = out;
      return { $ref: `#/$defs/${name}` };
    }
    return out;
  };
  const named = multi.definitions ?? {};
  for (const s of multi.schemas) countAll(s);
  for (const d of Object.values(named)) countAll(d);
  const out = multi.schemas.map(share);
  for (const k of Object.keys(named)) named[k] = share(named[k]);
  return { schemas: out, defs: sorted({ ...named, ...shared }) };
}
