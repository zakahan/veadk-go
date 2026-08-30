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
	"sync"
	"time"

	"github.com/volcengine/veadk-go/log"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

const defaultOptionalToolsetWarningInterval = time.Minute

type optionalToolset struct {
	delegate        tool.Toolset
	warningInterval time.Duration

	mu          sync.Mutex
	unavailable bool
	lastWarning time.Time
}

func newOptionalToolset(delegate tool.Toolset, warningInterval time.Duration) tool.Toolset {
	if warningInterval <= 0 {
		warningInterval = defaultOptionalToolsetWarningInterval
	}
	return &optionalToolset{
		delegate:        delegate,
		warningInterval: warningInterval,
	}
}

func (s *optionalToolset) Name() string {
	return s.delegate.Name()
}

func (s *optionalToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := s.delegate.Tools(ctx)
	if err == nil {
		s.mu.Lock()
		recovered := s.unavailable
		s.unavailable = false
		s.mu.Unlock()
		if recovered {
			log.Info("optional toolset recovered", "toolset", s.Name())
		}
		return tools, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}

	now := time.Now()
	s.mu.Lock()
	shouldWarn := !s.unavailable || now.Sub(s.lastWarning) >= s.warningInterval
	s.unavailable = true
	if shouldWarn {
		s.lastWarning = now
	}
	s.mu.Unlock()
	if shouldWarn {
		log.Warn(
			"optional toolset unavailable; continuing without its tools",
			"toolset", s.Name(),
			"error", err,
		)
	}
	return nil, nil
}
