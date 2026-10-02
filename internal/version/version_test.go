package version

import (
	"testing"
)

func TestEncodeDecodeSecret(t *testing.T) {
	orig := "tskey-auth-k36JHZ6yvK11CNTRL"
	encoded := EncodeSecret(orig)

	if encoded == orig {
		t.Fatalf("expected encoded string to differ from original")
	}
	if got := DecodeSecret(encoded); got != orig {
		t.Fatalf("expected %q, got %q", orig, got)
	}

	// Plain text should pass through unchanged
	if got := DecodeSecret(orig); got != orig {
		t.Fatalf("expected plaintext to pass through, got %q", got)
	}

	// Empty string
	if got := DecodeSecret(""); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}
