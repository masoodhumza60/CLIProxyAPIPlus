package qoder_openai

import (
	"encoding/json"
	"testing"
)

func TestConvertOpenAIToQoder(t *testing.T) {
	input := []byte(`{"model":"qoder-cn","messages":[{"role":"user","content":"hello"}]}`)
	result, err := ConvertOpenAIToQoder(input)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var qoderReq map[string]interface{}
	if err := json.Unmarshal(result, &qoderReq); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if qoderReq["model"] != "qoder-cn" {
		t.Errorf("expected model qoder-cn, got %v", qoderReq["model"])
	}
}
