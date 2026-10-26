// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package agentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

func TestAIOLifecyclePauseResumeRecreateAndDelete(t *testing.T) {
	type physical struct{ id, user, status string }
	var mu sync.Mutex
	var instances []physical
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		action := r.URL.Query().Get("Action")
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, action)
		result := map[string]any{}
		find := func(id string) *physical {
			for i := range instances {
				if instances[i].id == id {
					return &instances[i]
				}
			}
			return nil
		}
		id, _ := request["SessionId"].(string)
		switch action {
		case "ListSessions":
			items := make([]any, 0, len(instances))
			for _, item := range instances {
				items = append(items, map[string]any{"SessionId": item.id, "UserSessionId": item.user, "Status": item.status})
			}
			result["SessionInfos"] = items
		case "CreateSession":
			id = fmt.Sprintf("physical-%d", len(instances)+1)
			user, _ := request["UserSessionId"].(string)
			instances = append(instances, physical{id: id, user: user, status: "Ready"})
			result["SessionId"] = id
		case "GetSession":
			item := find(id)
			if item == nil || item.status == "Deleted" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ResponseMetadata": map[string]any{"Error": map[string]any{"Code": "InvalidResource.NotFound"}}})
				return
			}
			result = map[string]any{"SessionId": item.id, "UserSessionId": item.user, "Status": item.status, "Endpoint": "https://sandbox.example"}
		case "PauseSession":
			find(id).status = "Paused"
		case "ResumeSession":
			find(id).status = "Ready"
		case "DeleteSession":
			find(id).status = "Deleted"
		default:
			t.Errorf("unexpected action %s", action)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Result": result})
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	t.Setenv("AGENTKIT_TOOL_ID", "tool-test")
	t.Setenv("VOLCENGINE_ACCESS_KEY", "test-ak")
	t.Setenv("VOLCENGINE_SECRET_KEY", "test-sk")
	t.Setenv("AGENTKIT_TOOL_SCHEME", "http")
	t.Setenv("AGENTKIT_TOOL_HOST", u.Host)
	t.Setenv("AGENTKIT_TOOL_SERVICE_CODE", "agentkit")
	t.Setenv("AGENTKIT_TOOL_REGION", "cn-beijing")
	store := session.InMemoryService()
	m, err := NewAIOLifecycle(AIOLifecycleConfig{SessionService: store, SkillSpaceIDs: []string{"ss-demo"}, IdleTimeout: time.Second, PausedRetention: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := m.Create(ctx, "app", "user", "logical", map[string]any{"business": "value"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID() != "logical" {
		t.Fatalf("ADK session ID = %q", created.ID())
	}
	entry := m.track("app", "user", "logical")
	entry.lastActivity = time.Now().Add(-2 * time.Second)
	m.Sweep(ctx)
	if entry.pausedAt.IsZero() || instances[0].status != "Paused" {
		t.Fatalf("sandbox did not pause: %+v", instances)
	}
	if _, err := m.EnsureReady(ctx, "app", "user", "logical"); err != nil || instances[0].status != "Ready" {
		t.Fatalf("resume failed: %v, %+v", err, instances)
	}
	entry.lastActivity = time.Now().Add(-2 * time.Second)
	m.Sweep(ctx)
	entry.pausedAt = time.Now().Add(-2 * time.Second)
	m.Sweep(ctx)
	if instances[0].status != "Deleted" || !entry.deleted {
		t.Fatalf("expired sandbox not deleted: %+v", instances)
	}
	if _, err := m.EnsureReady(ctx, "app", "user", "logical"); err != nil || len(instances) != 2 || instances[1].status != "Ready" {
		t.Fatalf("recreate failed: %v, %+v", err, instances)
	}
	if err := m.Delete(ctx, "app", "user", "logical"); err != nil || instances[1].status != "Deleted" {
		t.Fatalf("explicit delete failed: %v, %+v", err, instances)
	}
	if _, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "logical"}); err == nil {
		t.Fatal("ADK session survived explicit deletion")
	}
	if len(actions) == 0 {
		t.Fatal("control plane was not called")
	}
}

func TestAIOLifecycleDurationConfiguration(t *testing.T) {
	t.Setenv("VEADK_AIO_IDLE_TIMEOUT", "7m")
	t.Setenv("VEADK_AIO_PAUSED_RETENTION", "3h")
	t.Setenv("VEADK_AIO_SCAN_INTERVAL", "12s")
	m, err := NewAIOLifecycle(AIOLifecycleConfig{SessionService: session.InMemoryService()})
	if err != nil {
		t.Fatal(err)
	}
	if m.cfg.IdleTimeout != 7*time.Minute || m.cfg.PausedRetention != 3*time.Hour || m.cfg.ScanInterval != 12*time.Second {
		t.Fatalf("environment durations were not applied: %+v", m.cfg)
	}
	if m.sessionTTLSeconds() < int((m.cfg.IdleTimeout+5*time.Minute)/time.Second) {
		t.Fatal("AgentKit TTL expires before configured idle timeout")
	}
	m, err = NewAIOLifecycle(AIOLifecycleConfig{SessionService: session.InMemoryService(), IdleTimeout: 2 * time.Minute})
	if err != nil || m.cfg.IdleTimeout != 2*time.Minute {
		t.Fatalf("explicit timeout did not take priority: %+v, %v", m, err)
	}
	t.Setenv("VEADK_AIO_PAUSED_RETENTION", "invalid")
	if _, err := NewAIOLifecycle(AIOLifecycleConfig{SessionService: session.InMemoryService()}); err == nil {
		t.Fatal("invalid retention was accepted")
	}
}
