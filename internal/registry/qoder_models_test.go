package registry

import "testing"

func TestQoderModelsRegistered(t *testing.T) {
	models := GetQoderModels()
	if len(models) == 0 {
		t.Fatal("expected Qoder models")
	}
	required := []string{"qoder-cn", "auto", "qwen3.7-max", "glm-5.1", "glm-5.2", "kimi-k2.6", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro", "deepseek-v4-flash"}
	for _, name := range required {
		found := false
		for _, m := range models {
			if m.ID == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("model %s not found", name)
		}
	}
}
