package builtin_tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
)

type MCPHeaderProvider func(context.Context) http.Header

type StreamableMCPConfig struct {
	Endpoint            string
	HTTPClient          *http.Client
	StaticHeaders       http.Header
	HeaderProvider      MCPHeaderProvider
	EnableStandaloneSSE bool
	MaxRetries          int
}

func NewStreamableMCPToolset(config StreamableMCPConfig) (tool.Toolset, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || endpoint == "" || parsedEndpoint.Host == "" || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") {
		return nil, fmt.Errorf("valid HTTP(S) MCP endpoint is required")
	}

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	clientCopy.Transport = &mcpHeaderRoundTripper{
		base:           baseTransport,
		staticHeaders:  config.StaticHeaders.Clone(),
		headerProvider: config.HeaderProvider,
	}

	return mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.StreamableClientTransport{
			Endpoint:             endpoint,
			HTTPClient:           &clientCopy,
			MaxRetries:           config.MaxRetries,
			DisableStandaloneSSE: !config.EnableStandaloneSSE,
		},
	})
}

type mcpHeaderRoundTripper struct {
	base           http.RoundTripper
	staticHeaders  http.Header
	headerProvider MCPHeaderProvider
}

func (r *mcpHeaderRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	applyHeaders(cloned.Header, r.staticHeaders)
	if r.headerProvider != nil {
		applyHeaders(cloned.Header, r.headerProvider(request.Context()))
	}
	return r.base.RoundTrip(cloned)
}

func applyHeaders(target, source http.Header) {
	for name, values := range source {
		target.Del(name)
		for _, value := range values {
			target.Add(name, value)
		}
	}
}
