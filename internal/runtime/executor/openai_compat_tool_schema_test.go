package executor

import (
	"encoding/json"
	"testing"
)

// The normalization runs on every `openai-compatibility` request, so these
// tests pin both that it does its job and that it stays out of the way of a
// request it has no reason to touch.

// TestFreebuffCompatToolSchemasInlineLocalRefs covers the rejection this exists
// for: a server that cannot resolve a local reference refuses the whole request.
func TestFreebuffCompatToolSchemasInlineLocalRefs(t *testing.T) {
	// `$defs` sits beside the tool list at the request root, which is where a
	// real payload puts it. The definitions therefore travel with the tools and
	// are inlined from there.
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"$ref":"#/$defs/Body","$defs":{"Body":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}}}]}`)

	updated := normalizeCompatToolSchemas(payload)

	var request struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatalf("decoding the normalized request: %v", err)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("expected one tool, got %d", len(request.Tools))
	}
	function, _ := request.Tools[0]["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	if _, stillRef := parameters["$ref"]; stillRef {
		t.Fatal("the reference should have been inlined before the request is sent")
	}
	properties, _ := parameters["properties"].(map[string]any)
	if _, hasPath := properties["path"]; !hasPath {
		t.Fatalf("the inlined definition should carry its properties, got %v", parameters)
	}
}

// TestFreebuffCompatToolSchemasDropNullability pins the OpenAPI-only annotation
// being removed, which several OpenAI-compatible servers reject.
func TestFreebuffCompatToolSchemasDropNullability(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","nullable":true}}}}}]}`)

	updated := normalizeCompatToolSchemas(payload)

	var request struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatalf("decoding the normalized request: %v", err)
	}
	function, _ := request.Tools[0]["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	a, _ := properties["a"].(map[string]any)
	if _, present := a["nullable"]; present {
		t.Fatal("the OpenAPI-only nullable annotation should have been removed")
	}
	if a["type"] != "string" {
		t.Fatalf("the real type should survive, got %v", a["type"])
	}
}

// TestFreebuffCompatToolSchemasCollapseNullableUnion pins the case that is safe
// to collapse: a union that is only "the type, or null".
func TestFreebuffCompatToolSchemasCollapseNullableUnion(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":["string","null"]}}}}}]}`)

	updated := normalizeCompatToolSchemas(payload)

	var request struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatalf("decoding the normalized request: %v", err)
	}
	function, _ := request.Tools[0]["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	a, _ := properties["a"].(map[string]any)
	if got := a["type"]; got != "string" {
		t.Fatalf("a null-only union should collapse to the real type, got %v", got)
	}
}

// TestFreebuffCompatToolSchemasKeepRealUnions is the guard on the behaviour that
// would be a regression if it were wrong. Flattening a genuine two-type union
// would silently discard a constraint the caller declared.
func TestFreebuffCompatToolSchemasKeepRealUnions(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":["string","integer"]}}}}}]}`)

	updated := normalizeCompatToolSchemas(payload)

	var request struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatalf("decoding the normalized request: %v", err)
	}
	function, _ := request.Tools[0]["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	a, _ := properties["a"].(map[string]any)
	types, isArray := a["type"].([]any)
	if !isArray || len(types) != 2 {
		t.Fatalf("a genuine two-type union must survive, got %v", a["type"])
	}
}

// TestFreebuffCompatToolSchemasLeaveRequestsAlone covers everything the step has
// no business changing, including a malformed body.
func TestFreebuffCompatToolSchemasLeaveRequestsAlone(t *testing.T) {
	cases := map[string]string{
		"no tools":     `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"empty tools":  `{"tools":[]}`,
		"not json":     `not json at all`,
		"null tools":   `{"tools":null}`,
		"tools string": `{"tools":"not-an-array"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			updated := normalizeCompatToolSchemas([]byte(payload))
			if name == "not json" || name == "tools string" {
				// A body that cannot be understood must come back untouched so
				// the upstream can answer for itself.
				if string(updated) != payload {
					t.Fatalf("an unreadable body should be returned untouched, got %s", updated)
				}
				return
			}
			var request struct {
				Tools []any `json:"tools"`
			}
			if err := json.Unmarshal(updated, &request); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if len(request.Tools) != 0 {
				t.Fatalf("tools should be absent, got %v", request.Tools)
			}
		})
	}
}

// TestFreebuffCompatToolSchemasDoNotMutateTheCaller pins that rewriting the
// payload does not reach back into a structure the caller still holds.
func TestFreebuffCompatToolSchemasDoNotMutateTheCaller(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":["string","null"]}}}}}]}`)

	normalizeCompatToolSchemas(payload)

	if got := string(payload); got != `{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":["string","null"]}}}}}]}` {
		t.Fatalf("the caller's payload was modified: %s", got)
	}
}
