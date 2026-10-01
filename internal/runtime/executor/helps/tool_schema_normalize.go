package helps

// Tool-schema normalization for OpenAI-shaped upstreams.
//
// Several providers accept the OpenAI `tools` envelope but reject the more
// expressive JSON Schema that modern SDKs emit: union types, nullable
// annotations, and locally-referenced definitions. The rewrite below reduces a
// schema to a conservative subset that those providers accept, without losing
// the structure the caller actually meant.
//
// It is provider-agnostic on purpose. Nothing here is specific to any one
// upstream, and the caller keeps full control: the input is never mutated.

import (
	"encoding/json"
	"strconv"
	"strings"
)

// MaxSchemaNormalizeDepth bounds how deep the normalizer will descend. A
// self-referential `$ref` would otherwise inline forever, so the walk stops at
// this depth and leaves the remainder as the caller wrote it.
const MaxSchemaNormalizeDepth = 12

// refPrefixes are the only `$ref` forms that can be resolved without a
// document. A remote or relative reference is left alone rather than being
// dropped, because removing it would silently change the schema's meaning.
var refPrefixes = []string{"#/definitions/", "#/$defs/"}

// unionKeys are the JSON Schema keywords that carry a list of alternatives.
var unionKeys = []string{"anyOf", "oneOf"}

// definitionKeys hold the locally-declared sub-schemas a `$ref` can point at.
var definitionKeys = []string{"definitions", "$defs"}

// schemaChildKeys are the keywords whose values are themselves schemas. They
// are walked so nested rules apply, which is the whole point of the helper.
var schemaChildKeys = []string{
	"items", "additionalProperties", "not", "contains", "if", "then", "else",
	"propertyNames", "additionalItems",
}

// schemaMapKeys are the keywords whose values are maps of name to schema.
var schemaMapKeys = []string{"properties", "patternProperties", "dependentSchemas"}

// schemaMapKeysWithRefs is schemaMapKeys plus the definition blocks. A local
// `$ref` has to be walked the same way as any other child schema, otherwise a
// ref is only ever resolved one level deep. These keys are dropped once the
// document root has been processed.
var schemaMapKeysWithRefs = append(append([]string{}, schemaMapKeys...), definitionKeys...)

// schemaListKeys are the keywords whose values are lists of schemas.
var schemaListKeys = []string{"allOf", "prefixItems"}

// NormalizeToolSchemas returns a copy of tools with every function's parameter
// schema reduced to a conservative subset. The input slice and the maps inside
// it are left untouched, so a caller can normalize without giving up ownership
// of its request body.
//
// Entries that are not a recognizable OpenAI function tool are passed through
// unchanged; a malformed tool is the caller's business, not this function's.
func NormalizeToolSchemas(tools []any) []any {
	if tools == nil {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, entry := range tools {
		out = append(out, normalizeTool(entry))
	}
	return out
}

// normalizeTool normalizes a single tool envelope, returning the original value
// when it is not a function tool with an object parameter schema.
func normalizeTool(entry any) any {
	tool, ok := entry.(map[string]any)
	if !ok {
		return entry
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		return entry
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		return entry
	}

	// Copy the two levels we touch so the caller's maps are never written to.
	copiedFn := copyMap(fn)
	copiedFn["parameters"] = normalizeSchema(params, rootScope(params), 0)
	copiedTool := copyMap(tool)
	copiedTool["function"] = copiedFn
	return copiedTool
}

// rootScope returns the definitions visible to a schema. Only the document root
// carries them, so a nested schema resolves against the same scope the caller
// wrote it into.
func rootScope(params map[string]any) map[string]any {
	scope := make(map[string]any)
	for _, key := range definitionKeys {
		if defs, ok := params[key].(map[string]any); ok {
			scope[key] = defs
		}
	}
	return scope
}

// normalizeSchema rewrites one schema node. scope holds the document's
// definitions; depth is the current recursion depth.
func normalizeSchema(schema map[string]any, scope map[string]any, depth int) map[string]any {
	if depth > MaxSchemaNormalizeDepth {
		// Too deep to keep inlining. Returning the node as-is is what breaks a
		// $ref cycle without losing the rest of the document.
		return copyMap(schema)
	}

	out := copyMap(schema)
	out = resolveRef(out, scope, depth)
	out = dropNullable(out)
	out = collapseUnion(out, scope, depth)
	out = collapseNullableType(out)
	out = dedupeEnum(out)
	out = dropNullConst(out)
	out = normalizeChildren(out, scope, depth)
	return out
}

// resolveRef inlines a local `$ref` target into the node and removes the ref.
// A ref it cannot resolve is preserved.
func resolveRef(schema map[string]any, scope map[string]any, depth int) map[string]any {
	ref, ok := schema["$ref"].(string)
	if !ok || ref == "" {
		return schema
	}
	target, ok := lookupRef(scope, ref)
	if !ok {
		return schema
	}

	// Sibling keys on a $ref node are annotations, so the resolved target wins
	// and only keys the target does not define are carried over.
	resolved := normalizeSchema(target, scope, depth+1)
	for key, value := range schema {
		if key == "$ref" {
			continue
		}
		if _, taken := resolved[key]; !taken {
			resolved[key] = value
		}
	}
	return resolved
}

// lookupRef resolves a local reference against the document definitions.
func lookupRef(scope map[string]any, ref string) (map[string]any, bool) {
	for _, prefix := range refPrefixes {
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		name := strings.TrimPrefix(ref, prefix)
		// A name may contain slashes, so walk the remaining path segments.
		segments := strings.Split(name, "/")
		for _, key := range definitionKeys {
			defs, ok := scope[key].(map[string]any)
			if !ok {
				continue
			}
			if found, ok := walkRefPath(defs, segments); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// walkRefPath descends through nested maps following the pointer segments.
func walkRefPath(current map[string]any, segments []string) (map[string]any, bool) {
	for i, segment := range segments {
		// A JSON Pointer escapes "~" as "~0" and "/" as "~1".
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")

		next, ok := current[segment]
		if !ok {
			return nil, false
		}
		if i == len(segments)-1 {
			target, ok := next.(map[string]any)
			return target, ok
		}
		current, ok = next.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

// dropNullable removes the OpenAPI `nullable` annotation, which is not part of
// JSON Schema proper and which strict validators reject.
func dropNullable(schema map[string]any) map[string]any {
	delete(schema, "nullable")
	return schema
}

// collapseUnion flattens `anyOf`/`oneOf` when they carry no real alternative.
//
// A union of one schema adds nothing, and the common nullable union of
// [T, null] is better expressed as T. A union of two genuinely different
// schemas is left intact: flattening it would change what the schema means.
func collapseUnion(schema map[string]any, scope map[string]any, depth int) map[string]any {
	for _, key := range unionKeys {
		raw, ok := schema[key]
		if !ok {
			continue
		}
		members, ok := raw.([]any)
		if !ok {
			continue
		}

		// Keep only members that are not the null type. A schema with no members
		// left is genuinely unsatisfiable, so leave it alone.
		kept := make([]any, 0, len(members))
		for _, member := range members {
			if isNullSchema(member) {
				continue
			}
			kept = append(kept, member)
		}
		if len(kept) == 0 {
			continue
		}
		if len(kept) == 1 {
			member, ok := kept[0].(map[string]any)
			if !ok {
				continue
			}
			// Merge the single member up into the parent, but never let a
			// conflicting type silently replace the parent's own type.
			delete(schema, key)
			for memberKey, value := range normalizeSchema(member, scope, depth+1) {
				if memberKey == "type" {
					if _, exists := schema["type"]; exists {
						continue
					}
				}
				schema[memberKey] = value
			}
			return collapseUnion(schema, scope, depth)
		}
		// A real union survives, but its members are still normalized.
		normalized := make([]any, 0, len(kept))
		for _, member := range kept {
			if memberSchema, ok := member.(map[string]any); ok {
				normalized = append(normalized, normalizeSchema(memberSchema, scope, depth+1))
				continue
			}
			normalized = append(normalized, member)
		}
		schema[key] = normalized
	}
	return schema
}

// isNullSchema reports whether a union member is the JSON null type.
func isNullSchema(member any) bool {
	schema, ok := member.(map[string]any)
	if !ok {
		return false
	}
	if typeName, ok := schema["type"].(string); ok {
		return typeName == "null"
	}
	// A bare `{}` matches anything, including null, so it is not a null-only
	// member and must not be dropped.
	return false
}

// collapseNullableType reduces `type: [T, "null"]` to `type: T`.
func collapseNullableType(schema map[string]any) map[string]any {
	types, ok := schema["type"].([]any)
	if !ok {
		return schema
	}
	kept := make([]any, 0, len(types))
	for _, entry := range types {
		if name, ok := entry.(string); ok && name == "null" {
			continue
		}
		kept = append(kept, entry)
	}
	switch len(kept) {
	case 0:
		// Nothing but null: leave the original rather than inventing a type.
		return schema
	case 1:
		schema["type"] = kept[0]
	case len(types):
		// Nothing was removed, so a genuine multi-type union is preserved.
	default:
		schema["type"] = kept
	}
	return schema
}

// dedupeEnum removes duplicate and null entries from an enum. A null in an enum
// is the nullable union again, spelled differently.
func dedupeEnum(schema map[string]any) map[string]any {
	values, ok := schema["enum"].([]any)
	if !ok {
		return schema
	}
	seen := make(map[string]struct{}, len(values))
	kept := make([]any, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		// Compare on the JSON form so 1 and "1" do not collide.
		key := string(mustJSON(value))
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		kept = append(kept, value)
	}
	schema["enum"] = kept
	return schema
}

// dropNullConst removes `const: null`, which is a strict validators reject and
// which a nullable annotation expresses better.
func dropNullConst(schema map[string]any) map[string]any {
	if value, present := schema["const"]; present && value == nil {
		delete(schema, "const")
	}
	return schema
}

// normalizeChildren walks nested schemas so the same rules apply at depth.
func normalizeChildren(schema map[string]any, scope map[string]any, depth int) map[string]any {
	for _, key := range schemaChildKeys {
		if child, ok := schema[key].(map[string]any); ok {
			schema[key] = normalizeSchema(child, scope, depth+1)
		}
	}
	for _, key := range schemaMapKeysWithRefs {
		children, ok := schema[key].(map[string]any)
		if !ok {
			continue
		}
		// Definition blocks are walked like any other child schema so a ref
		// nested inside one still resolves, then dropped: once every ref has
		// been inlined nothing reads them, and a strict validator rejects
		// keywords it does not recognize.
		copied := make(map[string]any, len(children))
		for name, child := range children {
			if childSchema, ok := child.(map[string]any); ok {
				copied[name] = normalizeSchema(childSchema, scope, depth+1)
				continue
			}
			copied[name] = child
		}
		if isDefinitionKey(key) {
			delete(schema, key)
			continue
		}
		schema[key] = copied
	}
	for _, key := range schemaListKeys {
		children, ok := schema[key].([]any)
		if !ok {
			continue
		}
		copied := make([]any, 0, len(children))
		for _, child := range children {
			if childSchema, ok := child.(map[string]any); ok {
				copied = append(copied, normalizeSchema(childSchema, scope, depth+1))
				continue
			}
			copied = append(copied, child)
		}
		schema[key] = copied
	}
	if items, ok := schema["items"].([]any); ok {
		// Draft-2020 tuple form: items is a list rather than a single schema.
		copied := make([]any, 0, len(items))
		for _, item := range items {
			if itemSchema, ok := item.(map[string]any); ok {
				copied = append(copied, normalizeSchema(itemSchema, scope, depth+1))
				continue
			}
			copied = append(copied, item)
		}
		schema["items"] = copied
	}
	return schema
}

// isDefinitionKey reports whether a keyword holds local definition blocks.
func isDefinitionKey(key string) bool {
	for _, candidate := range definitionKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

// copyMap shallow-copies a map so a rewrite never writes to the caller's data.
func copyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// mustJSON renders a value for use as a comparison key.
func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		// A value that cannot be marshalled cannot be meaningfully deduped, so
		// fall back to its Go rendering rather than dropping it.
		return []byte(strconv.Quote("unmarshalable"))
	}
	return raw
}
