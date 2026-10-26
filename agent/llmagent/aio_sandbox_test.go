// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package llmagent

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/volcengine/veadk-go/v2/auth/veauth"
	"github.com/volcengine/veadk-go/v2/skills"
	"github.com/volcengine/veadk-go/v2/tool/skilltool"
	"google.golang.org/adk/v2/agent"
	adkllmagent "google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type aioCountingModel struct{ calls atomic.Int32 }

func (*aioCountingModel) Name() string { return "counting-model" }
func (m *aioCountingModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.calls.Add(1)
	return func(func(*adkmodel.LLMResponse, error) bool) {}
}

func TestAIOSkillsModeDiscoversAtConstruction(t *testing.T) {
	var calls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["SkillSpaceId"] != "ss-test" {
			t.Errorf("Skill Space request: %v, %v", body, err)
		}
		_, _ = w.Write([]byte(`{"Result":{"Items":[{"Name":"demo","Description":"Demo Skill","BucketName":"bucket","TosPath":"demo.zip","Version":"v1"}]}}`))
	}))
	defer service.Close()
	cfg := &Config{
		Config:     adkllmagent.Config{Name: "aio-agent", Model: testLLM{}},
		SkillsMode: "aio_sandbox",
		Skills:     []string{"ss-test"},
		SkillSpaceSourceConfig: skills.RemoteSourceConfig{
			Endpoint: service.URL,
			Credentials: veauth.NewRoleCredentialSource(veauth.RoleCredentialSourceConfig{
				AccessKeyID: "test-ak", SecretAccessKey: "test-sk",
			}),
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New() did not discover Skill Space: %v", err)
	}
	if mode, ids, known := SkillsModeFor(a); !known || mode != "aio_sandbox" || len(ids) != 1 || ids[0] != "ss-test" {
		t.Fatalf("constructed Agent mode = %q, ids=%v, known=%t", mode, ids, known)
	}
	if calls.Load() != 1 || !strings.Contains(cfg.Instruction, "Demo Skill") || !strings.Contains(cfg.Instruction, "<name>demo</name>") || !strings.Contains(cfg.Instruction, "<path>/workspace/skills/demo</path>") {
		t.Fatalf("Agent prompt missing startup metadata: calls=%d instruction=%q", calls.Load(), cfg.Instruction)
	}
	if len(cfg.Toolsets) != 1 || len(cfg.BeforeAgentCallbacks) != 1 {
		t.Fatalf("AIO wiring: toolsets=%d callbacks=%d", len(cfg.Toolsets), len(cfg.BeforeAgentCallbacks))
	}
	if _, ok := cfg.Toolsets[0].(*skilltool.AIOSandboxToolset); !ok {
		t.Fatalf("toolset is %T, want AIOSandboxToolset", cfg.Toolsets[0])
	}
	empty := &Config{Config: adkllmagent.Config{Name: "empty-aio-agent", Model: testLLM{}}, SkillsMode: "aio_sandbox"}
	if _, err := New(empty); err != nil || len(empty.Toolsets) != 1 {
		t.Fatalf("empty AIO Skills must still install tools: %v", err)
	}
}

func TestLocalAgentExposesLocalMode(t *testing.T) {
	a, err := New(&Config{Config: adkllmagent.Config{Name: "local-agent", Model: testLLM{}}})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, known := SkillsModeFor(a); !known || mode != "local" {
		t.Fatalf("constructed local Agent mode = %q, known=%t", mode, known)
	}
}

func TestSkillsModeRejectsUnknownAndSandboxPaths(t *testing.T) {
	for _, cfg := range []*Config{
		{Config: adkllmagent.Config{Name: "unknown", Model: testLLM{}}, SkillsMode: "skills_sandbox"},
		{Config: adkllmagent.Config{Name: "absolute", Model: testLLM{}}, SkillsMode: "aio_sandbox", Skills: []string{"/workspace/skills"}},
		{Config: adkllmagent.Config{Name: "relative", Model: testLLM{}}, SkillsMode: "aio_sandbox", Skills: []string{"relative/skills"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%q, %v) accepted invalid mode or path", cfg.SkillsMode, cfg.Skills)
		}
	}
}

func TestAIOBindingFailureStopsBeforeModel(t *testing.T) {
	t.Setenv("AGENTKIT_TOOL_ID", "")
	model := &aioCountingModel{}
	a, err := New(&Config{Config: adkllmagent.Config{Name: "aio-failure", Model: model}, SkillsMode: "aio_sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := adkrunner.New(adkrunner.Config{AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for _, err := range r.Run(context.Background(), "user", "session", genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("missing sandbox binding did not produce an error")
	}
	if got := model.calls.Load(); got != 0 {
		t.Fatalf("model calls after failed binding = %d, want 0", got)
	}
}
