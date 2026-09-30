package qoder_claude

import (
	"encoding/json"
	"testing"
)

func TestConvertClaudeToQoder(t *testing.T) {
	input := []byte(`{"model":"qoder-cn","messages":[{"role":"user","content":"hello"}]}`)
	result, err := ConvertClaudeToQoder(input)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var qoderReq map[string]interface{}
	if err := json.Unmarshal(result, &qoderReq); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}
