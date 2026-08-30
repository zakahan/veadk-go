package requestcontext

import (
	"context"
	"strings"
)

type tipTokenKey struct{}

func WithTIPToken(ctx context.Context, token string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, tipTokenKey{}, strings.TrimSpace(token))
}

func TIPToken(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	token, _ := ctx.Value(tipTokenKey{}).(string)
	return token
}
