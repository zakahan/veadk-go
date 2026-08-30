package requestcontext

import (
	"context"
	"testing"
)

func TestTIPToken(t *testing.T) {
	ctx := WithTIPToken(context.Background(), " token-value ")
	if got, want := TIPToken(ctx), "token-value"; got != want {
		t.Fatalf("TIPToken() = %q, want %q", got, want)
	}
	if got := TIPToken(context.Background()); got != "" {
		t.Fatalf("TIPToken(empty context) = %q, want empty", got)
	}
}
