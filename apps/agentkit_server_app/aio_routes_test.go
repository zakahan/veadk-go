// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package agentkit_server_app

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/volcengine/veadk-go/v2/apps"
	"github.com/volcengine/veadk-go/v2/integrations/agentkit"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

func TestAIOSessionRoutesBindAndHideInternalState(t *testing.T) {
	var userSessionID string
	var status = "Ready"
	var createCalls, deleteCalls int
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch r.URL.Query().Get("Action") {
		case "ListSessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"SessionInfos": []any{}}})
		case "CreateSession":
			createCalls++
			userSessionID, _ = request["UserSessionId"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"SessionId": "physical", "Status": "Ready"}})
		case "GetSession":
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"SessionId": "physical", "UserSessionId": userSessionID, "Status": status, "Endpoint": "https://sandbox.example"}})
		case "DeleteSession":
			deleteCalls++
			status = "Deleted"
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{}})
		default:
			t.Errorf("unexpected AgentKit action %s", r.URL.Query().Get("Action"))
		}
	}))
	defer control.Close()
	u, _ := url.Parse(control.URL)
	t.Setenv("AGENTKIT_TOOL_ID", "tool-test")
	t.Setenv("VOLCENGINE_ACCESS_KEY", "test-ak")
	t.Setenv("VOLCENGINE_SECRET_KEY", "test-sk")
	t.Setenv("AGENTKIT_TOOL_SCHEME", "http")
	t.Setenv("AGENTKIT_TOOL_HOST", u.Host)
	t.Setenv("AGENTKIT_TOOL_SERVICE_CODE", "agentkit")
	t.Setenv("AGENTKIT_TOOL_REGION", "cn-beijing")
	store := session.InMemoryService()
	manager, err := agentkit.NewAIOLifecycle(agentkit.AIOLifecycleConfig{SessionService: store})
	if err != nil {
		t.Fatal(err)
	}
	root, err := agent.New(agent.Config{Name: "aio", Run: func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
		return func(func(*session.Event, error) bool) {}
	}})
	if err != nil {
		t.Fatal(err)
	}
	app := NewAgentkitServerApp(apps.DefaultApiConfig().SetApiPathPrefix("/api").SetSimpleAPIEnabled(false).SetWebUIEnabled(false), WithAIOLifecycle(manager))
	router := mux.NewRouter()
	if err := app.SetupRouters(router, &apps.RunConfig{AgentLoader: agent.NewSingleLoader(root), SessionService: store, DisableObservability: true}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(context.Background())
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	created := call(http.MethodPost, "/api/apps/aio/users/user/sessions", []byte(`{"sessionId":"logical","state":{"x":1}}`))
	if created.Code != http.StatusOK || strings.Contains(created.Body.String(), agentkit.AIOSandboxStateKey) || createCalls != 1 {
		t.Fatalf("create = %d %s; calls=%d", created.Code, created.Body.String(), createCalls)
	}
	oldName := call(http.MethodPost, "/api/apps/aio/users/user/sessions", []byte(`{"session_id":"another"}`))
	if oldName.Code != http.StatusBadRequest || createCalls != 1 {
		t.Fatalf("snake_case session_id was accepted: %d; creates=%d", oldName.Code, createCalls)
	}
	got := call(http.MethodGet, "/api/apps/aio/users/user/sessions/logical", nil)
	if got.Code != http.StatusOK || strings.Contains(got.Body.String(), agentkit.AIOSandboxStateKey) {
		t.Fatalf("get = %d %s", got.Code, got.Body.String())
	}
	bad := call(http.MethodPost, "/api/run_sse", []byte(`{"appName":"aio","userId":"user","sessionId":"logical","newMessage":{"role":"user","parts":[{"text":"hello"}]},"stateDelta":{"_veadk_aio_sandbox":{}}}`))
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reserved state delta status = %d", bad.Code)
	}
	badName := call(http.MethodPost, "/api/run_sse", []byte(`{"app_name":"aio","userId":"user","sessionId":"logical"}`))
	if badName.Code != http.StatusBadRequest {
		t.Fatalf("snake_case app_name was accepted: %d", badName.Code)
	}
	api := httptest.NewServer(router)
	defer api.Close()
	run, err := api.Client().Post(api.URL+"/api/run_sse", "application/json", strings.NewReader(`{"appName":"aio","userId":"user","sessionId":"logical","newMessage":{"role":"user","parts":[{"text":"hello"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Body.Close()
	if run.StatusCode != http.StatusOK || createCalls != 1 {
		t.Fatalf("run status = %d; creates=%d", run.StatusCode, createCalls)
	}
	deleted := call(http.MethodDelete, "/api/apps/aio/users/user/sessions/logical", nil)
	if deleted.Code != http.StatusOK || deleteCalls != 1 {
		t.Fatalf("delete = %d %s; calls=%d", deleted.Code, deleted.Body.String(), deleteCalls)
	}
}
