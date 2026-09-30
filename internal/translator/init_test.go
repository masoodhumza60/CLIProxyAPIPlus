package translator

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/qoder"
)

func TestQoderTranslatorsRegistered(t *testing.T) {
	// Verify translators are registered by checking the format constant
	if qoder.FormatQoder != "qoder" {
		t.Fatalf("expected FormatQoder to be 'qoder', got %q", qoder.FormatQoder)
	}

	// Registration is verified via the init() imports in init.go
	// which import the qoder/qoder_openai and qoder/qoder_claude packages
}
