package frame

import (
	"testing"
)

func TestGenerateFrame(t *testing.T) {
	b, err := GenerateFrame()
	if err != nil {
		t.Fatalf("GenerateFrame error: %v", err)
	}
	if len(b) < 100 {
		t.Fatalf("frame too small: %d", len(b))
	}
}
