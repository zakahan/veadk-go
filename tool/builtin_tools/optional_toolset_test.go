// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package builtin_tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

type optionalToolsetResult struct {
	tools []tool.Tool
	err   error
}

type sequenceToolset struct {
	mu      sync.Mutex
	results []optionalToolsetResult
	calls   int
}

func (*sequenceToolset) Name() string { return "test_optional_toolset" }

func (s *sequenceToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.calls
	s.calls++
	if index >= len(s.results) {
		index = len(s.results) - 1
	}
	result := s.results[index]
	return result.tools, result.err
}

func TestOptionalToolsetDegradesAndRecovers(t *testing.T) {
	delegate := &sequenceToolset{results: []optionalToolsetResult{
		{err: errors.New("MCP unavailable")},
		{tools: []tool.Tool{nil}},
		{err: errors.New("MCP unavailable again")},
	}}
	optional := newOptionalToolset(delegate, time.Hour)
	if got, want := optional.Name(), delegate.Name(); got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}

	tools, err := optional.Tools(nil)
	if err != nil || len(tools) != 0 {
		t.Fatalf("first Tools() = (%v, %v), want empty tools and nil error", tools, err)
	}
	tools, err = optional.Tools(nil)
	if err != nil || len(tools) != 1 {
		t.Fatalf("recovered Tools() = (%v, %v), want one tool and nil error", tools, err)
	}
	tools, err = optional.Tools(nil)
	if err != nil || len(tools) != 0 {
		t.Fatalf("third Tools() = (%v, %v), want empty tools and nil error", tools, err)
	}
}

func TestOptionalToolsetPreservesContextErrors(t *testing.T) {
	for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(target.Error(), func(t *testing.T) {
			delegate := &sequenceToolset{results: []optionalToolsetResult{{
				err: fmt.Errorf("wrapped: %w", target),
			}}}
			_, err := newOptionalToolset(delegate, time.Hour).Tools(nil)
			if !errors.Is(err, target) {
				t.Fatalf("Tools() error = %v, want %v", err, target)
			}
		})
	}
}

func TestOptionalToolsetConcurrentFailures(t *testing.T) {
	delegate := &sequenceToolset{results: []optionalToolsetResult{{err: errors.New("down")}}}
	optional := newOptionalToolset(delegate, time.Hour)
	const callers = 32
	var waitGroup sync.WaitGroup
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if tools, err := optional.Tools(nil); err != nil || len(tools) != 0 {
				t.Errorf("Tools() = (%v, %v), want empty tools and nil error", tools, err)
			}
		}()
	}
	waitGroup.Wait()
}
