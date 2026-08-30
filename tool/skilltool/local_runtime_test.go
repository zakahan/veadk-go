package skilltool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/volcengine/veadk-go/skills"
	"google.golang.org/adk/tool"
)

type localToolContext struct {
	tool.Context
	base      context.Context
	sessionID string
}

func (c *localToolContext) SessionID() string {
	return c.sessionID
}

func (c *localToolContext) Deadline() (time.Time, bool) {
	return c.base.Deadline()
}

func (c *localToolContext) Done() <-chan struct{} {
	return c.base.Done()
}

func (c *localToolContext) Err() error {
	return c.base.Err()
}

func (c *localToolContext) Value(key any) any {
	return c.base.Value(key)
}

func TestLocalSkillRuntimeWorkspaceAndTools(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "document-tools")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := strings.Join([]string{
		"---",
		"name: document-tools",
		"description: creates documents",
		"---",
		"Follow the reference.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "usage.md"), []byte("usage"), 0o644); err != nil {
		t.Fatal(err)
	}

	toolset, err := NewLocalSkillToolset(LocalRuntimeConfig{
		SkillsRoot:     skillsRoot,
		WorkspaceRoot:  t.TempDir(),
		CommandTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewLocalSkillToolset() error = %v", err)
	}
	if got, want := len(toolset.tools), 8; got != want {
		t.Fatalf("tool count = %d, want %d", got, want)
	}

	ctx := &localToolContext{base: context.Background(), sessionID: "session-a"}
	writeResult, err := toolset.localRuntime.writeFile(ctx, writeFileArgs{
		FilePath: "outputs/result.txt",
		Content:  "hello old",
	})
	if err != nil || writeResult["status"] != "success" {
		t.Fatalf("writeFile() = %#v, %v", writeResult, err)
	}
	editResult, err := toolset.localRuntime.editFile(ctx, editFileArgs{
		FilePath:  "outputs/result.txt",
		OldString: "old",
		NewString: "new",
	})
	if err != nil || editResult["status"] != "success" {
		t.Fatalf("editFile() = %#v, %v", editResult, err)
	}
	readResult, err := toolset.localRuntime.readFile(ctx, readFileArgs{FilePath: "outputs/result.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readResult["content"], "     1|hello new"; got != want {
		t.Fatalf("read content = %q, want %q", got, want)
	}
	skillRead, err := toolset.localRuntime.readFile(ctx, readFileArgs{FilePath: "skills/document-tools/references/usage.md"})
	if err != nil || skillRead["content"] != "     1|usage" {
		t.Fatalf("read skill resource = %#v, %v", skillRead, err)
	}

	for _, invalid := range []string{"../escape.txt", "/tmp/escape.txt", "skills/document-tools/SKILL.md"} {
		result, err := toolset.localRuntime.writeFile(ctx, writeFileArgs{FilePath: invalid, Content: "no"})
		if err != nil {
			t.Fatal(err)
		}
		if result["error_code"] != "INVALID_FILE_PATH" {
			t.Fatalf("writeFile(%q) = %#v, want INVALID_FILE_PATH", invalid, result)
		}
	}

	otherCtx := &localToolContext{base: context.Background(), sessionID: "session-b"}
	otherRead, err := toolset.localRuntime.readFile(otherCtx, readFileArgs{FilePath: "outputs/result.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if otherRead["error_code"] == nil {
		t.Fatalf("second session unexpectedly read first session file: %#v", otherRead)
	}
}

func TestLocalSkillRuntimeBashUsesWorkspaceAndTimeout(t *testing.T) {
	runtime, err := newLocalRuntime(LocalRuntimeConfig{
		WorkspaceRoot:  t.TempDir(),
		CommandTimeout: 50 * time.Millisecond,
	}, map[string]*skills.Skill{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &localToolContext{base: context.Background(), sessionID: "bash-session"}

	result, err := runtime.bash(ctx, bashArgs{Command: "printf hello > outputs/bash.txt && pwd"})
	if err != nil || result["status"] != "success" {
		t.Fatalf("bash() = %#v, %v", result, err)
	}
	path, _, err := runtime.resolvePath(ctx.SessionID(), "outputs/bash.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("bash output file = %q, %v", data, err)
	}

	timeoutResult, err := runtime.bash(ctx, bashArgs{Command: "sleep 1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := timeoutResult["status"], "timeout"; got != want {
		t.Fatalf("timeout status = %q, want %q; result=%#v", got, want, timeoutResult)
	}
}

func TestLocalSkillRuntimeRejectsWorkspaceSymlinkEscape(t *testing.T) {
	runtime, err := newLocalRuntime(LocalRuntimeConfig{WorkspaceRoot: t.TempDir()}, map[string]*skills.Skill{})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := runtime.ensureWorkspace("symlink-session")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.Symlink(outside, filepath.Join(workspace, "outputs", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.resolvePath("symlink-session", "outputs/escape.txt", true); err == nil {
		t.Fatal("resolvePath() error = nil, want symlink escape error")
	}
}

func TestLocalSkillRuntimeRunsSkillScriptSafely(t *testing.T) {
	skillsRoot := t.TempDir()
	skillDir := filepath.Join(skillsRoot, "script-tools")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := strings.Join([]string{
		"---",
		"name: script-tools",
		"description: runs test scripts",
		"---",
		"Run scripts safely.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "scripts", "echo.sh"),
		[]byte(`printf '%s|%s|%s' "$1" "$PWD" "$PYTHONPATH"`),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "timeout.sh"), []byte("sleep 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "unsupported.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}

	toolset, err := NewLocalSkillToolset(LocalRuntimeConfig{
		SkillsRoot:     skillsRoot,
		WorkspaceRoot:  t.TempDir(),
		CommandTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &localToolContext{base: context.Background(), sessionID: "script-session"}
	injectionArg := "; touch outputs/argument-was-executed"
	result, err := toolset.runSkillScriptToolHandler(ctx, runSkillScriptArgs{
		SkillName:  "script-tools",
		ScriptPath: "scripts/echo.sh",
		Args:       []string{injectionArg},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result["status"], "success"; got != want {
		t.Fatalf("run script status = %q, want %q; result=%#v", got, want, result)
	}
	stdout, _ := result["stdout"].(string)
	if !strings.HasPrefix(stdout, injectionArg+"|") || !strings.Contains(stdout, skillDir) {
		t.Fatalf("run script stdout = %q, want literal argument and skill PYTHONPATH", stdout)
	}
	workspace, err := toolset.localRuntime.ensureWorkspace(ctx.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "outputs", "argument-was-executed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script argument was interpreted by a shell: %v", err)
	}

	timeoutResult, err := toolset.runSkillScriptToolHandler(ctx, runSkillScriptArgs{
		SkillName: "script-tools", ScriptPath: "timeout.sh",
	})
	if err != nil || timeoutResult["status"] != "timeout" {
		t.Fatalf("timeout script = %#v, %v", timeoutResult, err)
	}

	unsupported, err := toolset.runSkillScriptToolHandler(ctx, runSkillScriptArgs{
		SkillName: "script-tools", ScriptPath: "unsupported.txt",
	})
	if err != nil || unsupported["error_code"] != "UNSUPPORTED_SCRIPT_TYPE" {
		t.Fatalf("unsupported script = %#v, %v", unsupported, err)
	}

	invalid, err := toolset.runSkillScriptToolHandler(ctx, runSkillScriptArgs{
		SkillName: "script-tools", ScriptPath: "../outside.sh",
	})
	if err != nil || invalid["error_code"] != "INVALID_SCRIPT_PATH" {
		t.Fatalf("invalid script path = %#v, %v", invalid, err)
	}
}

func TestLocalSkillRuntimeBoundsFileReads(t *testing.T) {
	runtime, err := newLocalRuntime(LocalRuntimeConfig{WorkspaceRoot: t.TempDir()}, map[string]*skills.Skill{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &localToolContext{base: context.Background(), sessionID: "large-file-session"}
	path, _, err := runtime.resolvePath(ctx.SessionID(), "outputs/large.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(MAX_SKILL_PAYLOAD_BYTES)+1); err != nil {
		t.Fatal(err)
	}

	readResult, err := runtime.readFile(ctx, readFileArgs{FilePath: "outputs/large.txt"})
	if err != nil || readResult["error_code"] != "FILE_TOO_LARGE" {
		t.Fatalf("read large file = %#v, %v", readResult, err)
	}
	editResult, err := runtime.editFile(ctx, editFileArgs{
		FilePath: "outputs/large.txt", OldString: "a", NewString: "b",
	})
	if err != nil || editResult["error_code"] != "FILE_TOO_LARGE" {
		t.Fatalf("edit large file = %#v, %v", editResult, err)
	}
}
