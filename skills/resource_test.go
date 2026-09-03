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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadResourceFromDiskAndMemory(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "reader", "reads files", "instructions")
	writeTestFile(t, filepath.Join(root, "reader", "references", "nested", "guide.md"), "guide")
	writeTestFile(t, filepath.Join(root, "reader", "root-guide.md"), "root guide")
	if err := os.Symlink("root-guide.md", filepath.Join(root, "reader", "linked-guide.md")); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0xff, 0x00, 0x01}
	binaryPath := filepath.Join(root, "reader", "assets", "template.bin")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, binary, 0o644); err != nil {
		t.Fatal(err)
	}

	discovered, err := DiscoverSkillsFromDir(root)
	if err != nil || len(discovered) != 1 {
		t.Fatalf("DiscoverSkillsFromDir() = %v, %v", skillNames(discovered), err)
	}
	for resourcePath, want := range map[string]string{
		"references/nested/guide.md": "guide",
		"root-guide.md":              "root guide",
		"linked-guide.md":            "root guide",
		"assets/template.bin":        string(binary),
	} {
		data, err := discovered[0].ReadResource(resourcePath, 1024)
		if err != nil {
			t.Fatalf("ReadResource(%q) error = %v", resourcePath, err)
		}
		if got := string(data); got != want {
			t.Fatalf("ReadResource(%q) = %q, want %q", resourcePath, got, want)
		}
	}

	inMemory := &Skill{Resources: &Resources{
		References: map[string]string{"guide.md": "memory-guide"},
		Assets:     map[string]string{"data.txt": "memory-asset"},
		Scripts:    map[string]*Script{"run.py": {Src: "print('ok')"}},
	}}
	for resourcePath, want := range map[string]string{
		"references/guide.md": "memory-guide",
		"assets/data.txt":     "memory-asset",
		"scripts/run.py":      "print('ok')",
	} {
		data, err := inMemory.ReadResource(resourcePath, 1024)
		if err != nil || string(data) != want {
			t.Fatalf("in-memory ReadResource(%q) = %q, %v", resourcePath, data, err)
		}
	}
}

func TestReadResourceRejectsInvalidPathsTypesAndSizes(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "safe-skill", "safe resources", "instructions")
	skillDir := filepath.Join(root, "safe-skill")
	writeTestFile(t, filepath.Join(skillDir, "assets", "small.txt"), "12345")
	if err := os.MkdirAll(filepath.Join(skillDir, "assets", "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	writeTestFile(t, outside, "outside-secret")
	if err := os.Symlink(outside, filepath.Join(skillDir, "assets", "escape.txt")); err != nil {
		t.Fatal(err)
	}

	skill, err := parseSkillMD(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, resourcePath := range []string{
		"", ".", "..", "../outside.txt", "assets/../../outside.txt", "/etc/passwd",
		"C:\\outside.txt", "assets/escape.txt",
	} {
		if _, err := skill.ReadResource(resourcePath, 1024); !errors.Is(err, ErrInvalidResourcePath) {
			t.Errorf("ReadResource(%q) error = %v, want ErrInvalidResourcePath", resourcePath, err)
		}
	}
	if _, err := skill.ReadResource("assets/missing.txt", 1024); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("missing resource error = %v, want ErrResourceNotFound", err)
	}
	if _, err := skill.ReadResource("unknown/file.txt", 1024); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("unknown resource error = %v, want ErrResourceNotFound", err)
	}
	if _, err := skill.ReadResource("assets", 1024); !errors.Is(err, ErrResourceNotRegular) {
		t.Fatalf("resource directory error = %v, want ErrResourceNotRegular", err)
	}
	if _, err := skill.ReadResource("assets/directory", 1024); !errors.Is(err, ErrResourceNotRegular) {
		t.Fatalf("directory error = %v, want ErrResourceNotRegular", err)
	}
	if _, err := skill.ReadResource("assets/small.txt", 4); !errors.Is(err, ErrResourceTooLarge) {
		t.Fatalf("oversized resource error = %v, want ErrResourceTooLarge", err)
	}
	if data, err := skill.ReadResource("assets/small.txt", 5); err != nil || string(data) != "12345" {
		t.Fatalf("exact-limit resource = %q, %v", data, err)
	}
	if _, err := skill.ReadResource("assets/small.txt", -1); !errors.Is(err, ErrResourceTooLarge) {
		t.Fatalf("negative limit error = %v, want ErrResourceTooLarge", err)
	}
}

func TestReadResourceHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "reader", "reads", "instructions")
	writeTestFile(t, filepath.Join(root, "reader", "assets", "data.txt"), strings.Repeat("x", 128*1024))
	skill, err := parseSkillMD(filepath.Join(root, "reader"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := skill.ReadResourceContext(ctx, "assets/data.txt", 256*1024); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadResourceContext() error = %v, want context.Canceled", err)
	}
}

func TestReadResourceSymlinkSwapNeverEscapesRoot(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, "racing-skill", "race test", "instructions")
	skillDir := filepath.Join(root, "racing-skill")
	assetsDir := filepath.Join(skillDir, "assets")
	writeTestFile(t, filepath.Join(assetsDir, "safe.txt"), "safe")
	outside := filepath.Join(root, "outside.txt")
	writeTestFile(t, outside, "outside-secret")
	current := filepath.Join(assetsDir, "current.txt")
	writeTestFile(t, current, "safe")

	skill, err := parseSkillMD(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var mutation sync.WaitGroup
	mutation.Add(1)
	go func() {
		defer mutation.Done()
		safeReplacement := current + ".safe"
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(current)
			_ = os.Symlink(outside, current)
			_ = os.WriteFile(safeReplacement, []byte("safe"), 0o644)
			_ = os.Rename(safeReplacement, current)
		}
	}()
	for range 500 {
		data, readErr := skill.ReadResource("assets/current.txt", 1024)
		if readErr == nil && string(data) != "safe" {
			close(stop)
			mutation.Wait()
			t.Fatalf("read escaped content %q", data)
		}
	}
	close(stop)
	mutation.Wait()
}
