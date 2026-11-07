package mcptools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolServer retains the definitions we register, including inferred schemas.
// The catalog becomes immutable before the server is returned to a transport.
type toolServer struct {
	*mcp.Server
	catalog []*mcp.Tool
}

func inferredSchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Errorf("infer tool schema: %w", err))
	}
	return schema
}

func addTool[In, Out any](s *toolServer, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if tool.InputSchema == nil {
		tool.InputSchema = inferredSchema[In]()
	}
	if tool.OutputSchema == nil {
		tool.OutputSchema = inferredSchema[Out]()
	}
	mcp.AddTool(s.Server, tool, handler)
	definition := *tool
	s.catalog = append(s.catalog, &definition)
}

// Start failures have their own schema branch: inventing an empty job_id or a
// new job state for a request that never started would break the job contract.
func addStartTool[In, Out any](s *toolServer, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	tool.InputSchema = runInputSchema[In]()
	tool.Description += reviewEvidenceGuide + runArgumentGuide
	if tool.Name == toolAgyRunSync {
		tool.Description += inlineWaitGuide
	}
	tool.OutputSchema = &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{inferredSchema[Out](), inferredSchema[startErrorOutput]()}}
	addTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		result, out, err := handler(ctx, req, in)
		if err == nil {
			return result, out, nil
		}
		return startErrorResult(err), nil, nil
	})
}

func startErrorResult(err error) *mcp.CallToolResult {
	data, _ := json.Marshal(describeStartError(err))
	return &mcp.CallToolResult{
		IsError: true, StructuredContent: json.RawMessage(data),
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// The SDK validates arguments before entering a typed handler. Keep these
// rejections just as machine-readable as failures returned by that handler.
func startValidationErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call == nil || call.Params == nil || (call.Params.Name != toolAgyRun && call.Params.Name != toolAgyRunSync) {
			return result, err
		}
		if response, ok := result.(*mcp.CallToolResult); ok && response != nil && response.IsError && response.StructuredContent == nil {
			message := "invalid run arguments"
			for _, content := range response.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					message = text.Text
					break
				}
			}
			data, _ := json.Marshal(startErrorOutput{ErrorCode: "invalid_argument", Message: message, JobStarted: new(false), NextAction: actionChangeRequest})
			response.StructuredContent = json.RawMessage(data)
		}
		return result, err
	}
}
