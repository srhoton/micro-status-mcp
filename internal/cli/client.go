package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// dialTimeout caps the time spent connecting to the MCP server.
	dialTimeout = 5 * time.Second
	// callTimeout caps the time spent on a single tool call.
	callTimeout = 10 * time.Second
)

// connect dials the local MCP server. The caller is responsible for closing
// the returned session.
func connect(ctx context.Context, endpoint string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "micro-status-mcp-cli",
		Version: Version,
	}, nil)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	sess, err := client.Connect(dialCtx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	return sess, nil
}

// callToolTyped invokes name with the JSON-encoded args struct, unmarshals
// the structured content into out (if non-nil), and returns the raw result.
// Use this over a map[string]any to keep field names checked at compile time.
func callToolTyped(ctx context.Context, sess *mcp.ClientSession, name string, args any, out any) (*mcp.CallToolResult, error) {
	asMap, err := toArgMap(args)
	if err != nil {
		return nil, fmt.Errorf("encode %s args: %w", name, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := sess.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: asMap})
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", name, err)
	}
	if res.IsError {
		return res, fmt.Errorf("tool %s returned error: %s", name, textOf(res))
	}
	if out != nil && res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return res, fmt.Errorf("marshal structured content: %w", err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return res, fmt.Errorf("unmarshal %s result: %w", name, err)
		}
	}
	return res, nil
}

func toArgMap(args any) (map[string]any, error) {
	if args == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func textOf(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
