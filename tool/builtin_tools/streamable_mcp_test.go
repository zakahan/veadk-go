package builtin_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/volcengine/veadk-go/requestcontext"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

func TestMCPHeaderRoundTripperKeepsRequestHeadersIsolated(t *testing.T) {
	const requestCount = 32
	seen := make(chan string, requestCount)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Tip-Token-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	roundTripper := &mcpHeaderRoundTripper{
		base:          http.DefaultTransport,
		staticHeaders: http.Header{"Authorization": []string{"Bearer static-key"}},
		headerProvider: func(ctx context.Context) http.Header {
			return http.Header{"X-Tip-Token-Key": []string{requestcontext.TIPToken(ctx)}}
		},
	}
	client := &http.Client{Transport: roundTripper}

	var waitGroup sync.WaitGroup
	for index := 0; index < requestCount; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			token := fmt.Sprintf("tip-%02d", index)
			request, err := http.NewRequestWithContext(requestcontext.WithTIPToken(context.Background(), token), http.MethodPost, server.URL, nil)
			if err != nil {
				t.Errorf("NewRequestWithContext() error = %v", err)
				return
			}
			response, err := client.Do(request)
			if err != nil {
				t.Errorf("client.Do() error = %v", err)
				return
			}
			response.Body.Close()
		}(index)
	}
	waitGroup.Wait()
	close(seen)

	counts := make(map[string]int, requestCount)
	for token := range seen {
		counts[token]++
	}
	for index := 0; index < requestCount; index++ {
		token := fmt.Sprintf("tip-%02d", index)
		if got, want := counts[token], 1; got != want {
			t.Fatalf("header count for %q = %d, want %d; all headers: %#v", token, got, want, counts)
		}
	}
}

func TestNewStreamableMCPToolsetRejectsInvalidEndpoint(t *testing.T) {
	if _, err := NewStreamableMCPToolset(StreamableMCPConfig{}); err == nil {
		t.Fatal("NewStreamableMCPToolset() error = nil, want endpoint error")
	}
}

func TestStreamableMCPToolsetInitializeListAndCallWithRequestHeaders(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var tips []string
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read MCP body: %v", err)
			return
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode MCP request: %v; body=%s", err, body)
			return
		}
		mu.Lock()
		methods = append(methods, request.Method)
		tips = append(tips, r.Header.Get("X-Tip-Token-Key"))
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()

		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0.0"},
			}
		case "tools/list":
			response["result"] = map[string]any{
				"tools": []map[string]any{{
					"name": "browser_ping", "description": "returns the request TIP token",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			}
		case "tools/call":
			response["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "tip=" + r.Header.Get("X-Tip-Token-Key")}},
				"isError": false,
			}
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	toolset, err := NewStreamableMCPToolset(StreamableMCPConfig{
		Endpoint:      server.URL,
		StaticHeaders: http.Header{"Authorization": []string{"Bearer static-mcp-key"}},
		HeaderProvider: func(ctx context.Context) http.Header {
			return http.Header{"X-Tip-Token-Key": []string{requestcontext.TIPToken(ctx)}}
		},
		MaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("NewStreamableMCPToolset() error = %v", err)
	}
	readonlyCtx := &mcpTestContext{
		Context:   requestcontext.WithTIPToken(context.Background(), "tip-list"),
		sessionID: "session-1",
	}
	availableTools, err := toolset.Tools(readonlyCtx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(availableTools) != 1 || availableTools[0].Name() != "browser_ping" {
		t.Fatalf("Tools() = %#v, want browser_ping", availableTools)
	}

	runnable, ok := availableTools[0].(interface {
		Run(tool.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %T does not implement Run", availableTools[0])
	}
	callCtx := &mcpTestContext{
		Context:   requestcontext.WithTIPToken(context.Background(), "tip-call"),
		sessionID: "session-1",
		actions:   &session.EventActions{},
	}
	result, err := runnable.Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := result["output"], "tip=tip-call"; got != want {
		t.Fatalf("Run() result = %#v, want %q", result, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) < 4 {
		t.Fatalf("MCP methods = %v, want initialize/initialized/list/call", methods)
	}
	if tips[len(tips)-1] != "tip-call" {
		t.Fatalf("tools/call TIP = %q, want tip-call; all=%v", tips[len(tips)-1], tips)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer static-mcp-key" {
			t.Fatalf("Authorization = %q, want static bearer; all=%v", authorization, authorizations)
		}
	}
}

type mcpTestContext struct {
	context.Context
	sessionID string
	actions   *session.EventActions
}

func (c *mcpTestContext) UserContent() *genai.Content          { return nil }
func (c *mcpTestContext) InvocationID() string                 { return "invocation-1" }
func (c *mcpTestContext) AgentName() string                    { return "test-agent" }
func (c *mcpTestContext) ReadonlyState() session.ReadonlyState { return nil }
func (c *mcpTestContext) UserID() string                       { return "test-user" }
func (c *mcpTestContext) AppName() string                      { return "test-app" }
func (c *mcpTestContext) SessionID() string                    { return c.sessionID }
func (c *mcpTestContext) Branch() string                       { return "" }
func (c *mcpTestContext) Artifacts() agent.Artifacts           { return nil }
func (c *mcpTestContext) State() session.State                 { return nil }
func (c *mcpTestContext) FunctionCallID() string               { return "call-1" }
func (c *mcpTestContext) Actions() *session.EventActions       { return c.actions }
func (c *mcpTestContext) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}
func (c *mcpTestContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (c *mcpTestContext) RequestConfirmation(string, any) error                { return nil }
