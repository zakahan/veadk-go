// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package agentkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

const AIOSandboxStateKey = "_veadk_aio_sandbox"

const aioSessionTTL = 1800

// AIOBinding contains only references; endpoints and credentials are resolved
// afresh from the control plane for each invocation.
type AIOBinding struct {
	ToolID        string `json:"tool_id"`
	SessionID     string `json:"session_id"`
	Identity      string `json:"identity"`
	SkillSpaceIDs string `json:"skill_space_ids,omitempty"`
}

type aioSession struct {
	ID, UserID, Status, Endpoint, InternalEndpoint, ExpireAt, CreatedAt string
}

var aioSessionLocks [64]sync.Mutex

func aioIdentity(appName, userID, sessionID string) string {
	data, _ := json.Marshal([]string{appName, userID, sessionID})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func aioPhysicalPrefix(identity, skillSpaceIDs string) string {
	prefix := "veadk-aio-" + identity
	if skillSpaceIDs != "" {
		sum := sha256.Sum256([]byte(skillSpaceIDs))
		prefix += "_s_" + hex.EncodeToString(sum[:6])
	}
	return prefix
}

func bindingFromState(state session.ReadonlyState) (*AIOBinding, error) {
	v, err := state.Get(AIOSandboxStateKey)
	if errors.Is(err, session.ErrStateKeyNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("invalid AIO sandbox binding")
	}
	var binding AIOBinding
	if err := json.Unmarshal(data, &binding); err != nil || binding.ToolID == "" || binding.SessionID == "" || binding.Identity == "" {
		return nil, fmt.Errorf("invalid AIO sandbox binding")
	}
	return &binding, nil
}

func aioToolID() string { return strings.TrimSpace(os.Getenv("AGENTKIT_TOOL_ID")) }

// EnsureAIOSandbox binds an ADK session before any model or tool execution.
// Concurrent binding attempts for the same session are serialized locally.
func EnsureAIOSandbox(ctx agent.Context) (*AIOBinding, error) {
	return EnsureAIOSandboxWithSkills(ctx, nil)
}

// EnsureAIOSandboxWithSkills passes the Agent's pinned Skill Space selection
// to a newly created sandbox and rejects reuse with a different selection.
func EnsureAIOSandboxWithSkills(ctx agent.Context, skillSpaceIDs []string) (*AIOBinding, error) {
	if ctx == nil {
		return nil, errors.New("AIO sandbox requires app, user and session identity")
	}
	if aioToolID() == "" {
		return nil, errors.New("AGENTKIT_TOOL_ID is required for aio_sandbox")
	}
	client, err := NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve AgentKit client: %w", err)
	}
	return EnsureAIOBindingWithSkills(ctx, client, ctx.State(), ctx.AppName(), ctx.UserID(), ctx.SessionID(), skillSpaceIDs)
}

// EnsureAIOBinding is shared by Runner callbacks and session-creation handlers.
// The caller owns persisting state in its configured ADK SessionService.
func EnsureAIOBinding(ctx context.Context, client *Client, state session.State, appName, userID, sessionID string) (*AIOBinding, error) {
	return EnsureAIOBindingWithSkills(ctx, client, state, appName, userID, sessionID, nil)
}

// EnsureAIOBindingWithSkills is also available to session-creation handlers.
func EnsureAIOBindingWithSkills(ctx context.Context, client *Client, state session.State, appName, userID, sessionID string, skillSpaceIDs []string) (*AIOBinding, error) {
	if ctx == nil || client == nil || state == nil || appName == "" || userID == "" || sessionID == "" {
		return nil, errors.New("AIO sandbox requires client, state, app, user and session identity")
	}
	toolID := aioToolID()
	if toolID == "" {
		return nil, errors.New("AGENTKIT_TOOL_ID is required for aio_sandbox")
	}
	identity := aioIdentity(appName, userID, sessionID)
	skillSpaceValue := strings.Join(skillSpaceIDs, ",")
	sum := sha256.Sum256([]byte(toolID + "\x00" + identity))
	lock := &aioSessionLocks[int(sum[0])%len(aioSessionLocks)]
	lock.Lock()
	defer lock.Unlock()
	if existing, err := bindingFromState(state); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Identity != identity || existing.ToolID != toolID || existing.SkillSpaceIDs != skillSpaceValue {
			return nil, errors.New("AIO sandbox binding does not match this session")
		}
		if _, err := readyAIOSession(ctx, client, *existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	userSessionID := aioPhysicalPrefix(identity, skillSpaceValue)
	candidates, err := listAIOSessions(ctx, client, toolID, userSessionID)
	if err != nil {
		return nil, err
	}
	var chosen *aioSession
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.UserID != userSessionID && !strings.HasPrefix(candidate.UserID, userSessionID+"_r_") {
			continue
		}
		status := strings.ToLower(candidate.Status)
		if status != "ready" && status != "starting" {
			continue
		}
		if expired(candidate.ExpireAt) {
			continue
		}
		if chosen == nil || candidate.CreatedAt > chosen.CreatedAt {
			chosen = candidate
		}
	}
	if chosen == nil {
		physicalID := userSessionID
		if len(candidates) != 0 {
			var ids []string
			for _, candidate := range candidates {
				ids = append(ids, candidate.ID)
			}
			data, _ := json.Marshal(ids)
			hash := sha256.Sum256(data)
			physicalID += "_r_" + hex.EncodeToString(hash[:6])
		}
		body := map[string]any{"ToolId": toolID, "UserSessionId": physicalID, "Ttl": aioSessionTTL}
		if skillSpaceValue != "" {
			body["Envs"] = []map[string]string{{"Key": "SKILL_SPACE_ID", "Value": skillSpaceValue}}
		}
		created, createErr := client.DoContext(ctx, "CreateSession", body)
		if createErr == nil {
			value := parseAIOSession(created)
			chosen = &value
		} else {
			// The response may have been lost after creation. Recover by the exact
			// physical ID; never retry the mutation blindly.
			recovered, recoveryErr := listAIOSessions(ctx, client, toolID, userSessionID)
			if recoveryErr != nil {
				return nil, fmt.Errorf("CreateSession result unknown: %w", createErr)
			}
			for i := range recovered {
				if recovered[i].UserID == physicalID && (strings.EqualFold(recovered[i].Status, "ready") || strings.EqualFold(recovered[i].Status, "starting")) {
					chosen = &recovered[i]
					break
				}
			}
			if chosen == nil {
				return nil, fmt.Errorf("CreateSession failed or result unknown: %w", createErr)
			}
		}
	}
	if chosen.ID == "" {
		return nil, errors.New("AgentKit session response has no SessionId")
	}
	binding := &AIOBinding{ToolID: toolID, SessionID: chosen.ID, Identity: identity, SkillSpaceIDs: skillSpaceValue}
	if _, err := readyAIOSession(ctx, client, *binding); err != nil {
		return nil, err
	}
	if err := state.Set(AIOSandboxStateKey, map[string]string{
		"tool_id": toolID, "session_id": chosen.ID, "identity": identity, "skill_space_ids": skillSpaceValue,
	}); err != nil {
		return nil, fmt.Errorf("persist AIO sandbox binding: %w", err)
	}
	return binding, nil
}

// ResolveAIOEndpoint validates an existing binding. Missing or expired
// bindings never create a new empty sandbox.
func ResolveAIOEndpoint(ctx agent.Context) (string, error) {
	if ctx == nil {
		return "", errors.New("AIO sandbox requires invocation context")
	}
	client, err := NewClient(ctx)
	if err != nil {
		return "", err
	}
	return ResolveAIOEndpointFor(ctx, client, ctx.ReadonlyState(), ctx.AppName(), ctx.UserID(), ctx.SessionID())
}

// ResolveAIOEndpointFor reads and validates a stored binding without creating
// a replacement sandbox. It can be used by non-Agent request handlers.
func ResolveAIOEndpointFor(ctx context.Context, client *Client, state session.ReadonlyState, appName, userID, sessionID string) (string, error) {
	if ctx == nil || client == nil || state == nil || appName == "" || userID == "" || sessionID == "" {
		return "", errors.New("AIO sandbox requires client, state, app, user and session identity")
	}
	binding, err := bindingFromState(state)
	if err != nil {
		return "", err
	}
	if binding == nil || binding.Identity != aioIdentity(appName, userID, sessionID) || binding.ToolID != aioToolID() {
		return "", errors.New("AIO sandbox is not bound to this session")
	}
	info, err := readyAIOSession(ctx, client, *binding)
	if err != nil {
		return "", err
	}
	endpoint := info.Endpoint
	if endpoint == "" {
		endpoint = info.InternalEndpoint
	}
	if endpoint == "" {
		return "", errors.New("bound AIO sandbox has no endpoint")
	}
	return endpoint, nil
}

func readyAIOSession(ctx context.Context, client *Client, binding AIOBinding) (*aioSession, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	for {
		result, err := client.DoContext(waitCtx, "GetSession", map[string]any{"ToolId": binding.ToolID, "SessionId": binding.SessionID})
		if err != nil {
			return nil, fmt.Errorf("resolve bound AIO sandbox: %w", err)
		}
		info := parseAIOSession(result)
		if info.ID != "" && info.ID != binding.SessionID {
			return nil, errors.New("AgentKit returned another sandbox session")
		}
		expectedUserSessionID := aioPhysicalPrefix(binding.Identity, binding.SkillSpaceIDs)
		if info.UserID != expectedUserSessionID && !strings.HasPrefix(info.UserID, expectedUserSessionID+"_r_") {
			return nil, errors.New("bound AIO sandbox belongs to another ADK session")
		}
		if expired(info.ExpireAt) {
			return nil, errors.New("bound AIO sandbox has expired")
		}
		switch strings.ToLower(info.Status) {
		case "ready":
			if info.Endpoint == "" && info.InternalEndpoint == "" {
				return nil, errors.New("bound AIO sandbox is Ready but has no endpoint")
			}
			return &info, nil
		case "starting":
		default:
			return nil, fmt.Errorf("bound AIO sandbox is %s", info.Status)
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-time.After(time.Second):
		}
	}
}

func listAIOSessions(ctx context.Context, client *Client, toolID, userID string) ([]aioSession, error) {
	var out []aioSession
	seen := map[string]bool{}
	var next string
	for {
		body := map[string]any{"ToolId": toolID, "Filters": []map[string]any{{"NameContains": "UserSessionId", "Values": []string{userID}}}, "MaxResults": 100}
		if next != "" {
			body["NextToken"] = next
		}
		response, err := client.DoContext(ctx, "ListSessions", body)
		if err != nil {
			return nil, fmt.Errorf("list AIO sandbox sessions: %w", err)
		}
		container := resultMap(response)
		items, ok := firstAny(container, "SessionInfos", "session_infos", "Items", "items").([]any)
		if !ok {
			return nil, errors.New("AgentKit ListSessions returned invalid session list")
		}
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok {
				out = append(out, sessionFromMap(item))
			}
		}
		next = firstString(container, "NextToken", "next_token")
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, errors.New("AgentKit ListSessions repeated NextToken")
		}
		seen[next] = true
	}
}

func resultMap(response map[string]any) map[string]any {
	if v, ok := response["Result"].(map[string]any); ok {
		return v
	}
	if v, ok := response["result"].(map[string]any); ok {
		return v
	}
	return response
}

func firstAny(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func parseAIOSession(response map[string]any) aioSession { return sessionFromMap(resultMap(response)) }

func sessionFromMap(m map[string]any) aioSession {
	return aioSession{
		ID: firstString(m, "SessionId", "session_id"), UserID: firstString(m, "UserSessionId", "user_session_id"),
		Status: firstString(m, "Status", "status"), Endpoint: firstString(m, "Endpoint", "endpoint"),
		InternalEndpoint: firstString(m, "InternalEndpoint", "internal_endpoint"),
		ExpireAt:         firstString(m, "ExpireAt", "expire_at"), CreatedAt: firstString(m, "CreatedAt", "created_at"),
	}
}

func expired(value string) bool {
	if value == "" {
		return false
	}
	when, err := time.Parse(time.RFC3339Nano, value)
	return err != nil || !when.After(time.Now())
}
