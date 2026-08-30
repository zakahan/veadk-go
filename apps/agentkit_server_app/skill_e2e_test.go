package agentkit_server_app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	veagent "github.com/volcengine/veadk-go/agent/llmagent"
	"github.com/volcengine/veadk-go/apps"
	"github.com/volcengine/veadk-go/apps/agentkit_server_app"
	"github.com/volcengine/veadk-go/tool/skilltool"
	"google.golang.org/adk/agent"
	adkllmagent "google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

func TestSkillAgentA2AAnswersAvailableLocalSkills(t *testing.T) {
	skillsRoot := t.TempDir()
	writeSkill(t, skillsRoot, "docx", "creates and edits Word documents")
	writeSkill(t, skillsRoot, "xlsx", "creates and edits spreadsheets")

	skillToolset, err := skilltool.NewLocalSkillToolset(skilltool.LocalRuntimeConfig{
		SkillsRoot:    skillsRoot,
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLocalSkillToolset() error = %v", err)
	}

	var modelMu sync.Mutex
	var modelRequest map[string]any
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		modelMu.Lock()
		modelRequest = request
		modelMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-skill-list", "object": "chat.completion", "model": "test-model",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "沙箱内可用的本地 Skill 有 docx 和 xlsx。",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer modelServer.Close()

	rootAgent, err := veagent.New(&veagent.Config{
		Config: adkllmagent.Config{
			Name:        "veadk_skills_agent",
			Description: "Answers questions and executes local skills.",
			Instruction: "Use the available local skills when relevant.",
			Toolsets:    []tool.Toolset{skillToolset},
			GenerateContentConfig: &genai.GenerateContentConfig{
				MaxOutputTokens: 18000,
			},
		},
		ModelName:       "test-model",
		ModelProvider:   "openai",
		ModelAPIBase:    modelServer.URL,
		ModelAPIKey:     "test-key",
		ModelHTTPClient: modelServer.Client(),
		DisableThought:  true,
	})
	if err != nil {
		t.Fatalf("llmagent.New() error = %v", err)
	}

	apiConfig := apps.DefaultApiConfig().
		SetA2APath("/").
		SetA2APublicPath("/a2a").
		SetSimpleAPIEnabled(false).
		SetWebUIEnabled(false)
	app := agentkit_server_app.NewAgentkitServerApp(apiConfig)
	runConfig := &apps.RunConfig{
		AgentLoader:    agent.NewSingleLoader(rootAgent),
		SessionService: session.InMemoryService(),
	}
	router := mux.NewRouter()
	if err := app.SetupRouters(router, runConfig); err != nil {
		t.Fatalf("SetupRouters() error = %v", err)
	}
	server := httptest.NewServer(router)
	defer server.Close()

	blocking := false
	requestPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      "request-1",
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": "message-1",
				"role":      "user",
				"parts": []map[string]any{{
					"kind": "text",
					"text": "这个沙箱里有哪些本地 skill？",
				}},
			},
			"configuration": map[string]any{"blocking": blocking, "historyLength": 20},
		},
	}
	body, err := json.Marshal(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(server.URL+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("A2A message/send request error = %v", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("A2A status = %d, body = %s", response.StatusCode, responseBody)
	}
	var sendResponse struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(responseBody, &sendResponse); err != nil {
		t.Fatalf("decode message/send response: %v; body=%s", err, responseBody)
	}
	if sendResponse.Result.ID == "" {
		t.Fatalf("message/send response has no task id: %s", responseBody)
	}

	var finalResponseBody []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pollPayload := map[string]any{
			"jsonrpc": "2.0",
			"id":      "poll-1",
			"method":  "tasks/get",
			"params":  map[string]any{"id": sendResponse.Result.ID, "historyLength": 20},
		}
		pollBody, marshalErr := json.Marshal(pollPayload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		pollResponse, postErr := http.Post(server.URL+"/", "application/json", bytes.NewReader(pollBody))
		if postErr != nil {
			t.Fatalf("tasks/get request error = %v", postErr)
		}
		finalResponseBody, err = io.ReadAll(pollResponse.Body)
		pollResponse.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if pollResponse.StatusCode != http.StatusOK {
			t.Fatalf("tasks/get status = %d, body = %s", pollResponse.StatusCode, finalResponseBody)
		}
		if strings.Contains(string(finalResponseBody), "docx") && strings.Contains(string(finalResponseBody), "xlsx") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(finalResponseBody), "docx") || !strings.Contains(string(finalResponseBody), "xlsx") {
		t.Fatalf("tasks/get response does not list local skills: %s", finalResponseBody)
	}

	modelMu.Lock()
	serializedModelRequest, err := json.Marshal(modelRequest)
	modelMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(serializedModelRequest, []byte("docx")) || !bytes.Contains(serializedModelRequest, []byte("xlsx")) {
		t.Fatalf("model request does not contain discovered skills: %s", serializedModelRequest)
	}
	if !bytes.Contains(serializedModelRequest, []byte(`"max_tokens":18000`)) {
		t.Fatalf("model request does not contain max_tokens=18000: %s", serializedModelRequest)
	}
	if !bytes.Contains(serializedModelRequest, []byte(`"thinking":{"type":"disabled"}`)) {
		t.Fatalf("model request does not disable thinking: %s", serializedModelRequest)
	}
}

func writeSkill(t *testing.T, root, name, description string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		"---",
		"name: " + name,
		"description: " + description,
		"---",
		"Follow this skill's instructions.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
