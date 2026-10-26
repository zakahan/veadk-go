// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package llmagent

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"

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

func TestAIOSkillsModeDoesNotReadHostPaths(t *testing.T) {
	cfg := &Config{
		Config:     adkllmagent.Config{Name: "aio-agent", Model: testLLM{}},
		SkillsMode: "aio_sandbox",
		Skills:     []string{"/a/host/path/that/does/not/exist"},
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New() inspected a sandbox path on the host: %v", err)
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

func TestSkillsModeRejectsUnknownAndRelativeSandboxPaths(t *testing.T) {
	for _, cfg := range []*Config{
		{Config: adkllmagent.Config{Name: "unknown", Model: testLLM{}}, SkillsMode: "skills_sandbox"},
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
