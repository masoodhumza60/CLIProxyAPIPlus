package helps

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// decodeSchema parses a JSON object literal into the map form the normalizer works on.
func decodeSchema(t *testing.T, literal string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(literal), &out); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	return out
}

// encode renders a value back to compact JSON for comparisons.
func encode(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(raw)
}

// tool wraps a parameters schema in the OpenAI tool envelope the executor sees.
func tool(t *testing.T, parameters string) map[string]any {
	t.Helper()
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "example",
			"description": "an example tool",
			"parameters":  decodeSchema(t, parameters),
		},
	}
}

// parametersOf digs the parameters schema back out of a normalized tool.
func parametersOf(t *testing.T, normalized map[string]any) map[string]any {
	t.Helper()
	fn, ok := normalized["function"].(map[string]any)
	if !ok {
		t.Fatalf("normalized tool has no function object: %s", encode(t, normalized))
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("normalized tool has no parameters schema: %s", encode(t, normalized))
	}
	return params
}

func TestFreebuffNormalizeToolSchemasResolvesLocalDefinitionRefs(t *testing.T) {
	tools := []any{tool(t, `{
		"type": "object",
		"properties": {"thing": {"$ref": "#/definitions/Thing"}},
		"definitions": {"Thing": {"type": "string", "description": "a thing"}}
	}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))

	thing, ok := got["properties"].(map[string]any)["thing"].(map[string]any)
	if !ok {
		t.Fatalf("properties.thing is not an object: %s", encode(t, got))
	}
	if _, still := thing["$ref"]; still {
		t.Error("$ref was not resolved: " + encode(t, thing))
	}
	if thing["type"] != "string" {
		t.Errorf("resolved schema type = %v, want string", thing["type"])
	}
	if thing["description"] != "a thing" {
		t.Errorf("resolved schema lost its description: %s", encode(t, thing))
	}
	if _, still := got["definitions"]; still {
		t.Error("definitions should be removed once every ref is inlined")
	}
}

func TestFreebuffNormalizeToolSchemasResolvesLocalDefsRefs(t *testing.T) {
	tools := []any{tool(t, `{
		"type": "object",
		"properties": {"thing": {"$ref": "#/$defs/Thing"}},
		"$defs": {"Thing": {"type": "number"}}
	}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))

	thing := got["properties"].(map[string]any)["thing"].(map[string]any)
	if thing["type"] != "number" {
		t.Errorf("resolved $defs ref type = %v, want number", thing["type"])
	}
	if _, still := got["$defs"]; still {
		t.Error("$defs should be removed once every ref is inlined")
	}
}

func TestFreebuffNormalizeToolSchemasLeavesUnknownRefsAlone(t *testing.T) {
	// A remote or non-local ref cannot be resolved offline; rewriting it would
	// corrupt the schema, so it must survive untouched.
	tools := []any{tool(t, `{
		"type": "object",
		"properties": {"thing": {"$ref": "https://example.com/schema.json#/Thing"}}
	}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	thing := got["properties"].(map[string]any)["thing"].(map[string]any)
	if thing["$ref"] != "https://example.com/schema.json#/Thing" {
		t.Errorf("remote $ref was rewritten: %s", encode(t, thing))
	}
}

func TestFreebuffNormalizeToolSchemasDeletesNullable(t *testing.T) {
	tools := []any{tool(t, `{
		"type": "object",
		"properties": {"name": {"type": "string", "nullable": true}},
		"nullable": true
	}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	if _, still := got["nullable"]; still {
		t.Error("root nullable was not deleted")
	}
	name := got["properties"].(map[string]any)["name"].(map[string]any)
	if _, still := name["nullable"]; still {
		t.Error("nested nullable was not deleted")
	}
}

func TestFreebuffNormalizeToolSchemasCollapsesNullableUnion(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"anyOf", `{"anyOf": [{"type": "string"}, {"type": "null"}]}`},
		{"oneOf", `{"oneOf": [{"type": "number"}, {"type": "null"}]}`},
		{"reversed order", `{"anyOf": [{"type": "null"}, {"type": "boolean"}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := []any{tool(t, `{"type": "object", "properties": {"v": `+tc.in+`}}`)}

			got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
			v := got["properties"].(map[string]any)["v"].(map[string]any)

			if _, still := v["anyOf"]; still {
				t.Errorf("anyOf survived a nullable-only union: %s", encode(t, v))
			}
			if _, still := v["oneOf"]; still {
				t.Errorf("oneOf survived a nullable-only union: %s", encode(t, v))
			}
			if v["type"] == nil {
				t.Errorf("union did not collapse to a concrete type: %s", encode(t, v))
			}
		})
	}
}

func TestFreebuffNormalizeToolSchemasKeepsRealUnions(t *testing.T) {
	// A union of two real types is meaningful and must never be flattened.
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"anyOf": [{"type": "string"}, {"type": "number"}]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	union, ok := v["anyOf"].([]any)
	if !ok || len(union) != 2 {
		t.Fatalf("a real union was flattened: %s", encode(t, v))
	}
}

func TestFreebuffNormalizeToolSchemasCollapsesSingleRemainingUnion(t *testing.T) {
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"anyOf": [{"type": "string", "description": "keep me"}]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	if _, still := v["anyOf"]; still {
		t.Errorf("a single-entry anyOf should collapse into its member: %s", encode(t, v))
	}
	if v["type"] != "string" {
		t.Errorf("collapsed schema type = %v, want string", v["type"])
	}
	if v["description"] != "keep me" {
		t.Errorf("collapsed schema lost sibling keys: %s", encode(t, v))
	}
}

func TestFreebuffNormalizeToolSchemasCollapsesNullableTypeArray(t *testing.T) {
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"type": ["string", "null"]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	if v["type"] != "string" {
		t.Errorf("nullable type array was not collapsed: %s", encode(t, v))
	}
}

func TestFreebuffNormalizeToolSchemasKeepsGenuineTypeUnions(t *testing.T) {
	// Removing the null entry leaves two real types, so the array must survive.
	// Collapsing it to one would silently drop a type the caller declared.
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"type": ["null", "integer", "string"]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	types, ok := v["type"].([]any)
	if !ok {
		t.Fatalf("a genuine type union was collapsed to a single type: %s", encode(t, v))
	}
	want := []any{"integer", "string"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("type = %s, want %s", encode(t, types), encode(t, want))
	}
}

func TestFreebuffNormalizeToolSchemasKeepsOnlyNonNullType(t *testing.T) {
	// A single real type alongside null is the common nullable annotation, and
	// it collapses to the bare type so strict validators accept it.
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"type": ["null", "integer"]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	if v["type"] != "integer" {
		t.Errorf("nullable single type = %v, want the bare non-null type (integer)", v["type"])
	}
}

func TestFreebuffNormalizeToolSchemasDeduplicatesEnumAndDropsNil(t *testing.T) {
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"enum": ["a", "a", "b", null]}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	v := got["properties"].(map[string]any)["v"].(map[string]any)

	want := []any{"a", "b"}
	if !reflect.DeepEqual(v["enum"], want) {
		t.Errorf("enum = %s, want %s", encode(t, v["enum"]), encode(t, want))
	}
}

func TestFreebuffNormalizeToolSchemasDeletesNullConst(t *testing.T) {
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"const": null}, "w": {"const": "keep"}}}`)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	props := got["properties"].(map[string]any)

	if _, still := props["v"].(map[string]any)["const"]; still {
		t.Error("const: null was not deleted")
	}
	if props["w"].(map[string]any)["const"] != "keep" {
		t.Error("a non-null const must be preserved")
	}
}

func TestFreebuffNormalizeToolSchemasLeavesMinimalSchemaUnchanged(t *testing.T) {
	const minimal = `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`
	tools := []any{tool(t, minimal)}

	got := parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	if encode(t, got) != encode(t, decodeSchema(t, minimal)) {
		t.Errorf("an already-minimal schema was rewritten:\n got %s\nwant %s", encode(t, got), minimal)
	}
}

func TestFreebuffNormalizeToolSchemasTerminatesOnRefCycle(t *testing.T) {
	// A self-referential definition must not send the normalizer into an
	// infinite inline; the depth limit has to break the loop.
	tools := []any{tool(t, `{
		"type": "object",
		"properties": {"node": {"$ref": "#/definitions/Node"}},
		"definitions": {"Node": {"type": "object", "properties": {"child": {"$ref": "#/definitions/Node"}}}}
	}`)}

	done := make(chan map[string]any, 1)
	go func() {
		done <- parametersOf(t, NormalizeToolSchemas(tools)[0].(map[string]any))
	}()

	select {
	case got := <-done:
		// Whatever the shape, it must terminate and still be an object.
		if got["type"] != "object" {
			t.Errorf("cyclic schema root lost its type: %s", encode(t, got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NormalizeToolSchemas did not terminate on a $ref cycle")
	}
}

func TestFreebuffNormalizeToolSchemasDoesNotMutateInput(t *testing.T) {
	tools := []any{tool(t, `{"type": "object", "properties": {"v": {"type": ["string", "null"], "nullable": true}}}`)}
	before := encode(t, tools[0])

	NormalizeToolSchemas(tools)

	if after := encode(t, tools[0]); after != before {
		t.Errorf("input was mutated in place:\n got %s\nwant %s", after, before)
	}
}

func TestFreebuffNormalizeToolSchemasSkipsUnusableEntries(t *testing.T) {
	tools := []any{
		"not a tool",
		map[string]any{"type": "function"},
		map[string]any{"type": "function", "function": "not an object"},
		tool(t, `{"type": "object"}`),
	}

	got := NormalizeToolSchemas(tools)
	if len(got) != len(tools) {
		t.Fatalf("tool count changed: got %d, want %d", len(got), len(tools))
	}
}
