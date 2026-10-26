// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package agentkit_server_app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/volcengine/veadk-go/v2/apps"
	"github.com/volcengine/veadk-go/v2/integrations/agentkit"
	"google.golang.org/adk/v2/session"
)

func (a *AgentkitServerApp) setupAIORoutes(router *mux.Router, cfg *apps.RunConfig) error {
	if cfg.SessionService == nil {
		return errors.New("AIO session routes require SessionService")
	}
	root := cfg.AgentLoader.RootAgent().Name()
	prefix := strings.TrimRight(a.ApiPathPrefix, "/")
	base := prefix + "/apps/{app_name}/users/{user_id}/sessions"
	create := func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if vars["app_name"] != root || vars["user_id"] == "" {
			http.Error(w, "unknown app or user", http.StatusNotFound)
			return
		}
		var body struct {
			SessionID string            `json:"session_id"`
			State     map[string]any    `json:"state"`
			Events    []json.RawMessage `json:"events"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 20<<20)).Decode(&body); err != nil {
				http.Error(w, "invalid session request", http.StatusBadRequest)
				return
			}
		}
		id := vars["session_id"]
		if id != "" && body.SessionID != "" && id != body.SessionID {
			http.Error(w, "conflicting session IDs", http.StatusUnprocessableEntity)
			return
		}
		if id == "" {
			id = body.SessionID
		}
		created, err := a.aioLifecycle.Create(r.Context(), vars["app_name"], vars["user_id"], id, body.State)
		if err != nil {
			if errors.Is(err, agentkit.ErrAIOSessionExists) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, "AIO session creation failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		for _, raw := range body.Events {
			var event session.Event
			var fields map[string]json.RawMessage
			decodeErr := json.Unmarshal(raw, &fields)
			if decodeErr == nil && fields == nil {
				decodeErr = errors.New("event must be an object")
			}
			if decodeErr == nil {
				if stamp, ok := fields["timestamp"]; ok {
					var seconds float64
					decodeErr = json.Unmarshal(stamp, &seconds)
					if decodeErr == nil {
						whole := int64(seconds)
						event.Timestamp = time.Unix(whole, int64((seconds-float64(whole))*1e9))
					}
					delete(fields, "timestamp")
				}
			}
			if decodeErr == nil {
				var remainder []byte
				remainder, decodeErr = json.Marshal(fields)
				if decodeErr == nil {
					decodeErr = json.Unmarshal(remainder, &event)
				}
			}
			if event.Timestamp.IsZero() {
				event.Timestamp = time.Now()
			}
			_, forbidden := event.Actions.StateDelta[agentkit.AIOSandboxStateKey]
			if decodeErr != nil || forbidden {
				_ = a.aioLifecycle.Delete(r.Context(), created.AppName(), created.UserID(), created.ID())
				http.Error(w, "invalid initial event", http.StatusUnprocessableEntity)
				return
			}
			if err := cfg.SessionService.AppendEvent(r.Context(), created, &event); err != nil {
				_ = a.aioLifecycle.Delete(r.Context(), created.AppName(), created.UserID(), created.ID())
				http.Error(w, "save initial event failed", http.StatusServiceUnavailable)
				return
			}
		}
		writeADKSession(w, created)
	}
	router.HandleFunc(base, create).Methods(http.MethodPost)
	router.HandleFunc(base+"/{session_id}", create).Methods(http.MethodPost)
	router.HandleFunc(base+"/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if vars["app_name"] != root {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		got, err := cfg.SessionService.Get(r.Context(), &session.GetRequest{
			AppName: vars["app_name"], UserID: vars["user_id"], SessionID: vars["session_id"],
		})
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		writeADKSession(w, got.Session)
	}).Methods(http.MethodGet)
	router.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if vars["app_name"] != root {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		listed, err := cfg.SessionService.List(r.Context(), &session.ListRequest{AppName: vars["app_name"], UserID: vars["user_id"]})
		if err != nil {
			http.Error(w, "list sessions failed", http.StatusServiceUnavailable)
			return
		}
		items := make([]any, 0, len(listed.Sessions))
		for _, item := range listed.Sessions {
			items = append(items, adkSessionValue(item))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	}).Methods(http.MethodGet)
	router.HandleFunc(base+"/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		if vars["app_name"] != root {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		err := a.aioLifecycle.Delete(r.Context(), vars["app_name"], vars["user_id"], vars["session_id"])
		if err != nil {
			writeAIOError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "null\n")
	}).Methods(http.MethodDelete)
	return nil
}

// aioRunHandler validates identity and holds the lifecycle lock until the ADK
// handler has finished, including the final SSE frame.
func (a *AgentkitServerApp) aioRunHandler(next http.Handler, root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.aioLifecycle == nil {
			next.ServeHTTP(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, strings.TrimRight(a.ApiPathPrefix, "/"))
		if path != "/run" && path != "/run_sse" && path != "/run_live" {
			next.ServeHTTP(w, r)
			return
		}
		var app, user, id string
		if path == "/run_live" {
			app, user, id = r.URL.Query().Get("appName"), r.URL.Query().Get("userId"), r.URL.Query().Get("sessionId")
			if app == "" {
				app = r.URL.Query().Get("app_name")
			}
			if user == "" {
				user = r.URL.Query().Get("user_id")
			}
			if id == "" {
				id = r.URL.Query().Get("session_id")
			}
		} else {
			data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, "invalid run request", http.StatusBadRequest)
				return
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(data, &body); err != nil {
				http.Error(w, "invalid run request", http.StatusBadRequest)
				return
			}
			for alias, canonical := range map[string]string{"app_name": "appName", "user_id": "userId", "session_id": "sessionId", "new_message": "newMessage", "state_delta": "stateDelta"} {
				if value, ok := body[alias]; ok {
					if _, duplicate := body[canonical]; duplicate {
						http.Error(w, "duplicate run field", http.StatusUnprocessableEntity)
						return
					}
					body[canonical] = value
					delete(body, alias)
				}
			}
			if raw := body["stateDelta"]; len(raw) != 0 {
				var delta map[string]any
				if json.Unmarshal(raw, &delta) != nil {
					http.Error(w, "invalid stateDelta", http.StatusUnprocessableEntity)
					return
				}
				if _, forbidden := delta[agentkit.AIOSandboxStateKey]; forbidden {
					http.Error(w, "sandbox binding is server-owned", http.StatusUnprocessableEntity)
					return
				}
			}
			_ = json.Unmarshal(body["appName"], &app)
			_ = json.Unmarshal(body["userId"], &user)
			_ = json.Unmarshal(body["sessionId"], &id)
			data, _ = json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
		}
		if app != root || user == "" || id == "" {
			http.Error(w, "unknown app or missing session identity", http.StatusNotFound)
			return
		}
		if err := a.aioLifecycle.WithSession(r.Context(), app, user, id, func() error {
			next.ServeHTTP(w, r)
			return nil
		}); err != nil {
			writeAIOError(w, err)
		}
	})
}

func writeAIOError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, agentkit.ErrAIOSessionBusy) {
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}

func writeADKSession(w http.ResponseWriter, value session.Session) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adkSessionValue(value))
}

func adkSessionValue(value session.Session) map[string]any {
	state := map[string]any{}
	for key, item := range value.State().All() {
		if key != agentkit.AIOSandboxStateKey {
			state[key] = item
		}
	}
	events := make([]any, 0, value.Events().Len())
	for event := range value.Events().All() {
		if event.Author != "veadk_lifecycle" {
			actions := map[string]any{
				"artifactDelta":              event.Actions.ArtifactDelta,
				"requestedToolConfirmations": event.Actions.RequestedToolConfirmations,
				"skipSummarization":          event.Actions.SkipSummarization,
				"transferToAgent":            event.Actions.TransferToAgent,
				"escalate":                   event.Actions.Escalate,
			}
			if len(event.Actions.StateDelta) > 0 {
				delta := map[string]any{}
				for key, item := range event.Actions.StateDelta {
					if key != agentkit.AIOSandboxStateKey {
						delta[key] = item
					}
				}
				actions["stateDelta"] = delta
			}
			events = append(events, map[string]any{
				"id": event.ID, "invocationId": event.InvocationID,
				"author": event.Author, "branch": event.Branch,
				"timestamp": float64(event.Timestamp.UnixNano()) / 1e9,
				"content":   event.Content, "partial": event.Partial,
				"actions": actions, "errorCode": event.ErrorCode,
				"errorMessage": event.ErrorMessage, "usageMetadata": event.UsageMetadata,
				"turnComplete": event.TurnComplete, "interrupted": event.Interrupted,
				"groundingMetadata":  event.GroundingMetadata,
				"longRunningToolIds": event.LongRunningToolIDs,
			})
		}
	}
	return map[string]any{
		"id": value.ID(), "appName": value.AppName(), "userId": value.UserID(),
		"state": state, "events": events, "lastUpdateTime": value.LastUpdateTime().Unix(),
	}
}
