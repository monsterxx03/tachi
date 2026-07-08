package acpctx

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
)

type connKey struct{}

func WithConn(ctx context.Context, conn *acp.AgentSideConnection) context.Context {
	return context.WithValue(ctx, connKey{}, conn)
}

func Conn(ctx context.Context) *acp.AgentSideConnection {
	conn, _ := ctx.Value(connKey{}).(*acp.AgentSideConnection)
	return conn
}
