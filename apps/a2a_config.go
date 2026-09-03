// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apps

import (
	"net/http"
	"net/url"
	"strings"
)

// AgentCardURLResolver resolves the externally reachable A2A endpoint for one
// Agent Card request. Resolvers are an explicit trust boundary: the default is
// nil and therefore ignores Host, Forwarded and X-Forwarded-* headers.
type AgentCardURLResolver func(request *http.Request, config *ApiConfig) string

// SetPublicURL sets the externally reachable base URL used by Agent Cards.
// It may include a path prefix, for example https://example.test/sandbox.
func (a *ApiConfig) SetPublicURL(publicURL string) *ApiConfig {
	a.PublicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	return a
}

// SetA2APath sets the internal HTTP route used by the A2A JSON-RPC handler.
func (a *ApiConfig) SetA2APath(path string) *ApiConfig {
	a.A2APath = normalizeRoutePath(path)
	return a
}

// SetA2APublicPath sets the externally visible path advertised by Agent Cards.
// An empty value makes it follow A2APath.
func (a *ApiConfig) SetA2APublicPath(path string) *ApiConfig {
	if strings.TrimSpace(path) == "" {
		a.A2APublicPath = ""
		return a
	}
	a.A2APublicPath = normalizeRoutePath(path)
	return a
}

// SetAgentCardURLResolver opts into request-aware Agent Card URLs. Use
// ForwardedAgentCardURL only behind a trusted reverse proxy which overwrites
// client-supplied forwarding headers.
func (a *ApiConfig) SetAgentCardURLResolver(resolver AgentCardURLResolver) *ApiConfig {
	a.AgentCardURLResolver = resolver
	return a
}

// GetA2APath returns the internal A2A JSON-RPC route.
func (a *ApiConfig) GetA2APath() string {
	return normalizeRoutePath(a.A2APath)
}

// GetA2APublicPath returns the path advertised to A2A clients.
func (a *ApiConfig) GetA2APublicPath() string {
	if strings.TrimSpace(a.A2APublicPath) != "" {
		return normalizeRoutePath(a.A2APublicPath)
	}
	return a.GetA2APath()
}

// GetA2APublicURL returns the configured, request-independent Agent Card URL.
func (a *ApiConfig) GetA2APublicURL() string {
	baseURL := a.GetWebUrl()
	if validPublicURL(a.PublicURL) {
		baseURL = a.PublicURL
	}
	publicURL, err := url.JoinPath(baseURL, a.GetA2APublicPath())
	if err != nil {
		return baseURL
	}
	return publicURL
}

// ResolveAgentCardURL resolves an Agent Card URL and falls back to static
// configuration if a custom resolver returns an invalid or empty URL.
func (a *ApiConfig) ResolveAgentCardURL(request *http.Request) string {
	if a.AgentCardURLResolver != nil {
		resolved := strings.TrimSpace(a.AgentCardURLResolver(request, a))
		if validPublicURL(resolved) {
			return resolved
		}
	}
	return a.GetA2APublicURL()
}

// ForwardedAgentCardURL resolves an Agent Card URL from standard Forwarded or
// X-Forwarded-Proto/X-Forwarded-Host headers. It must only be enabled behind a
// trusted proxy which removes client-supplied forwarding headers.
func ForwardedAgentCardURL(request *http.Request, config *ApiConfig) string {
	if request == nil || config == nil {
		return ""
	}

	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	} else if request.URL != nil && validPublicScheme(request.URL.Scheme) {
		scheme = strings.ToLower(request.URL.Scheme)
	}
	host := request.Host
	if host == "" && request.URL != nil {
		host = request.URL.Host
	}

	forwardedScheme, forwardedHost := parseForwarded(request.Header.Get("Forwarded"))
	if forwardedScheme != "" {
		scheme = forwardedScheme
	}
	if forwardedHost != "" {
		host = forwardedHost
	}
	if forwardedScheme == "" {
		if candidate := firstHeaderValue(request.Header.Get("X-Forwarded-Proto")); validPublicScheme(candidate) {
			scheme = strings.ToLower(candidate)
		}
	}
	if forwardedHost == "" {
		if candidate := firstHeaderValue(request.Header.Get("X-Forwarded-Host")); validPublicHost(candidate) {
			host = candidate
		}
	}
	if !validPublicScheme(scheme) || !validPublicHost(host) {
		return ""
	}

	result := &url.URL{Scheme: scheme, Host: host, Path: config.GetA2APublicPath()}
	return result.String()
}

func normalizeRoutePath(route string) string {
	route = strings.TrimSpace(route)
	if route == "" || route == "/" {
		return "/"
	}
	return "/" + strings.Trim(route, "/")
}

func validPublicURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed != nil && validPublicScheme(parsed.Scheme) &&
		validPublicHost(parsed.Host) && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validPublicScheme(value string) bool {
	return strings.EqualFold(value, "http") || strings.EqualFold(value, "https")
}

func validPublicHost(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\/?#@\r\n\t ") {
		return false
	}
	parsed, err := url.Parse("http://" + value)
	return err == nil && parsed.Host == value && parsed.Hostname() != "" && parsed.User == nil && parsed.Path == ""
}

func firstHeaderValue(value string) string {
	return strings.TrimSpace(strings.SplitN(value, ",", 2)[0])
}

func parseForwarded(value string) (scheme, host string) {
	first := firstHeaderValue(value)
	for _, parameter := range strings.Split(first, ";") {
		key, candidate, ok := strings.Cut(parameter, "=")
		if !ok {
			continue
		}
		candidate = strings.Trim(strings.TrimSpace(candidate), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "proto":
			if validPublicScheme(candidate) {
				scheme = strings.ToLower(candidate)
			}
		case "host":
			if validPublicHost(candidate) {
				host = candidate
			}
		}
	}
	return scheme, host
}
