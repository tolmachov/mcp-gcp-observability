package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func applyAuthHint(t *testing.T, method string, res mcp.Result) mcp.Result {
	t.Helper()
	handler := authErrorHintMiddleware()(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return res, nil
	})
	out, err := handler(context.Background(), method, nil)
	require.NoError(t, err)
	return out
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

func TestAuthErrorHintMiddleware(t *testing.T) {
	t.Run("appends unauthenticated hint", func(t *testing.T) {
		res := applyAuthHint(t, "tools/call",
			toolError("Failed to query logs: rpc error: code = Unauthenticated desc = token expired."))
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "re-connect this MCP server")
	})
	t.Run("appends permission hint", func(t *testing.T) {
		res := applyAuthHint(t, "tools/call",
			toolError("Failed to list errors: rpc error: code = PermissionDenied desc = missing permission."))
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "IAM permission")
	})
	t.Run("does not append twice", func(t *testing.T) {
		msg := "code = Unauthenticated." + unauthenticatedHint
		res := applyAuthHint(t, "tools/call", toolError(msg))
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.Equal(t, msg, text)
	})
	t.Run("leaves other errors alone", func(t *testing.T) {
		res := applyAuthHint(t, "tools/call", toolError("invalid filter syntax"))
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.Equal(t, "invalid filter syntax", text)
	})
	t.Run("leaves successful results alone", func(t *testing.T) {
		ok := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "code = Unauthenticated in payload"}}}
		res := applyAuthHint(t, "tools/call", ok)
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.NotContains(t, text, "Hint:")
	})
	t.Run("ignores other methods", func(t *testing.T) {
		res := applyAuthHint(t, "resources/read", toolError("code = Unauthenticated"))
		text := res.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text
		assert.NotContains(t, text, "Hint:")
	})
}
