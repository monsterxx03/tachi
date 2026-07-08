// Package acpctx provides context-based access to the ACP agent connection
// for tools that need to route file I/O through the ACP client protocol.
package acpctx

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
)

type connKey struct{}

// WithConn returns a context with the ACP agent connection attached.
func WithConn(ctx context.Context, conn *acp.AgentSideConnection) context.Context {
	return context.WithValue(ctx, connKey{}, conn)
}

// Conn returns the ACP agent connection stored in ctx, or nil if none is set.
func Conn(ctx context.Context) *acp.AgentSideConnection {
	conn, _ := ctx.Value(connKey{}).(*acp.AgentSideConnection)
	return conn
}
