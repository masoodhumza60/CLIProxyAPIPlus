package qoder

import "testing"

func TestFormatQoderConstant(t *testing.T) {
	if FormatQoder != "qoder" {
		t.Fatalf("expected FormatQoder to be 'qoder', got %q", FormatQoder)
	}
}
