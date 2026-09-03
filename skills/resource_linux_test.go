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

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadResourceRejectsSpecialFileWithoutOpeningIt(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "special-skill", "special files", "instructions")
	skillDir := filepath.Join(root, "special-skill")
	pipePath := filepath.Join(skillDir, "assets", "input.pipe")
	if err := os.MkdirAll(filepath.Dir(pipePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(pipePath, 0o600); err != nil {
		t.Fatal(err)
	}
	skill, err := parseSkillMD(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := skill.ReadResource("assets/input.pipe", 1024); !errors.Is(err, ErrResourceNotRegular) {
		t.Fatalf("special file error = %v, want ErrResourceNotRegular", err)
	}
}
