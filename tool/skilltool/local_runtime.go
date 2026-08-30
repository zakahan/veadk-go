package skilltool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/volcengine/veadk-go/skills"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

const (
	defaultWorkspaceRoot        = "/tmp/veadk"
	defaultCommandTimeout       = 10 * time.Minute
	defaultMaxCommandOutputSize = 4 * 1024 * 1024
	maxReadLines                = 10_000
	maxScriptArguments          = 256
	maxScriptArgumentBytes      = 64 * 1024
	localRuntimeInstruction     = "Local skill execution uses a session-specific workspace with skills/, uploads/, and outputs/ directories.\n" +
		"Use read_file, write_file, and edit_file for text files. Use bash for commands required by a loaded skill.\n" +
		"Write final generated artifacts under outputs/. Paths passed to file tools must be relative to the session workspace.\n"
)

type LocalRuntimeConfig struct {
	SkillsRoot           string
	WorkspaceRoot        string
	CommandTimeout       time.Duration
	MaxCommandOutputSize int
}

type LocalRuntime struct {
	workspaceRoot        string
	commandTimeout       time.Duration
	maxCommandOutputSize int
	skills               map[string]*skills.Skill
}

func NewLocalSkillToolset(config LocalRuntimeConfig) (*SkillToolset, error) {
	discovered, err := skills.DiscoverSkillsFromDir(config.SkillsRoot)
	if err != nil {
		return nil, err
	}
	toolset, err := NewSkillToolset(discovered, nil)
	if err != nil {
		return nil, err
	}
	runtime, err := newLocalRuntime(config, toolset.skills)
	if err != nil {
		return nil, err
	}
	toolset.localRuntime = runtime
	localTools, err := runtime.tools()
	if err != nil {
		return nil, err
	}
	toolset.tools = append(toolset.tools, localTools...)
	return toolset, nil
}

func newLocalRuntime(config LocalRuntimeConfig, skillMap map[string]*skills.Skill) (*LocalRuntime, error) {
	workspaceRoot := strings.TrimSpace(config.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}
	workspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	commandTimeout := config.CommandTimeout
	if commandTimeout <= 0 {
		commandTimeout = defaultCommandTimeout
	}
	maxOutput := config.MaxCommandOutputSize
	if maxOutput <= 0 {
		maxOutput = defaultMaxCommandOutputSize
	}
	return &LocalRuntime{
		workspaceRoot:        workspaceRoot,
		commandTimeout:       commandTimeout,
		maxCommandOutputSize: maxOutput,
		skills:               skillMap,
	}, nil
}

func (r *LocalRuntime) tools() ([]tool.Tool, error) {
	result := make([]tool.Tool, 0, 4)
	readTool, err := functiontool.New(functiontool.Config{
		Name: "read_file", Description: "Read a UTF-8 text file from the current session workspace with line numbers.",
	}, r.readFile)
	if err != nil {
		return nil, fmt.Errorf("create read_file tool: %w", err)
	}
	result = append(result, readTool)
	writeTool, err := functiontool.New(functiontool.Config{
		Name: "write_file", Description: "Write a UTF-8 text file inside the current session workspace. Skill source files are read-only.",
	}, r.writeFile)
	if err != nil {
		return nil, fmt.Errorf("create write_file tool: %w", err)
	}
	result = append(result, writeTool)
	editTool, err := functiontool.New(functiontool.Config{
		Name: "edit_file", Description: "Replace exact text in a file inside the current session workspace.",
	}, r.editFile)
	if err != nil {
		return nil, fmt.Errorf("create edit_file tool: %w", err)
	}
	result = append(result, editTool)
	bashTool, err := functiontool.New(functiontool.Config{
		Name: "bash", Description: "Run a bash command in the current session workspace for a loaded skill workflow.",
	}, r.bash)
	if err != nil {
		return nil, fmt.Errorf("create bash tool: %w", err)
	}
	result = append(result, bashTool)
	return result, nil
}

type readFileArgs struct {
	FilePath string `json:"file_path" jsonschema:"Relative path inside the session workspace."`
	Offset   int    `json:"offset,omitempty" jsonschema:"Optional first line to read, starting at 1."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Optional maximum number of lines."`
}

func (r *LocalRuntime) readFile(ctx tool.Context, args readFileArgs) (map[string]any, error) {
	path, _, err := r.resolvePath(ctx.SessionID(), args.FilePath, false)
	if err != nil {
		return toolError("INVALID_FILE_PATH", err), nil
	}
	data, err := readLocalFile(path, MAX_SKILL_PAYLOAD_BYTES)
	if err != nil {
		if errors.Is(err, errLocalFileTooLarge) {
			return toolError("FILE_TOO_LARGE", err), nil
		}
		return toolError("READ_FILE_ERROR", err), nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return toolError("BINARY_FILE", fmt.Errorf("read_file supports text files only")), nil
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	start := args.Offset
	if start <= 0 {
		start = 1
	}
	limit := args.Limit
	if limit <= 0 || limit > maxReadLines {
		limit = maxReadLines
	}
	startIndex := start - 1
	if startIndex >= len(lines) {
		return map[string]any{"content": "", "line_count": len(lines)}, nil
	}
	end := startIndex + limit
	if end > len(lines) {
		end = len(lines)
	}
	var output strings.Builder
	for index, line := range lines[startIndex:end] {
		if len(line) > 2000 {
			line = line[:2000] + "..."
		}
		fmt.Fprintf(&output, "%6d|%s", startIndex+index+1, line)
		if index < end-startIndex-1 {
			output.WriteByte('\n')
		}
	}
	return map[string]any{"content": output.String(), "line_count": len(lines)}, nil
}

type writeFileArgs struct {
	FilePath string `json:"file_path" jsonschema:"Relative writable path inside the session workspace."`
	Content  string `json:"content" jsonschema:"UTF-8 text to write."`
}

func (r *LocalRuntime) writeFile(ctx tool.Context, args writeFileArgs) (map[string]any, error) {
	if len(args.Content) > MAX_SKILL_PAYLOAD_BYTES {
		return toolError("FILE_TOO_LARGE", fmt.Errorf("content exceeds %d bytes", MAX_SKILL_PAYLOAD_BYTES)), nil
	}
	path, _, err := r.resolvePath(ctx.SessionID(), args.FilePath, true)
	if err != nil {
		return toolError("INVALID_FILE_PATH", err), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return toolError("WRITE_FILE_ERROR", err), nil
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return toolError("WRITE_FILE_ERROR", err), nil
	}
	return map[string]any{"status": "success", "path": args.FilePath, "bytes_written": len(args.Content)}, nil
}

type editFileArgs struct {
	FilePath   string `json:"file_path" jsonschema:"Relative writable path inside the session workspace."`
	OldString  string `json:"old_string" jsonschema:"Exact text to replace."`
	NewString  string `json:"new_string" jsonschema:"Replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence when true."`
}

func (r *LocalRuntime) editFile(ctx tool.Context, args editFileArgs) (map[string]any, error) {
	if args.OldString == args.NewString {
		return toolError("INVALID_REPLACEMENT", fmt.Errorf("old_string and new_string must be different")), nil
	}
	path, _, err := r.resolvePath(ctx.SessionID(), args.FilePath, true)
	if err != nil {
		return toolError("INVALID_FILE_PATH", err), nil
	}
	data, err := readLocalFile(path, MAX_SKILL_PAYLOAD_BYTES)
	if err != nil {
		if errors.Is(err, errLocalFileTooLarge) {
			return toolError("FILE_TOO_LARGE", err), nil
		}
		return toolError("READ_FILE_ERROR", err), nil
	}
	content := string(data)
	count := strings.Count(content, args.OldString)
	if count == 0 {
		return toolError("TEXT_NOT_FOUND", fmt.Errorf("old_string was not found")), nil
	}
	if count > 1 && !args.ReplaceAll {
		return toolError("TEXT_NOT_UNIQUE", fmt.Errorf("old_string appears %d times", count)), nil
	}
	replacements := 1
	if args.ReplaceAll {
		replacements = -1
	}
	updated := strings.Replace(content, args.OldString, args.NewString, replacements)
	if len(updated) > MAX_SKILL_PAYLOAD_BYTES {
		return toolError("FILE_TOO_LARGE", fmt.Errorf("edited file exceeds %d bytes", MAX_SKILL_PAYLOAD_BYTES)), nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return toolError("WRITE_FILE_ERROR", err), nil
	}
	replaced := 1
	if args.ReplaceAll {
		replaced = count
	}
	return map[string]any{"status": "success", "path": args.FilePath, "replacements": replaced}, nil
}

type bashArgs struct {
	Command     string  `json:"command" jsonschema:"Bash command to execute."`
	Description string  `json:"description,omitempty" jsonschema:"Short description of the command."`
	Timeout     float64 `json:"timeout,omitempty" jsonschema:"Optional timeout in seconds."`
}

func (r *LocalRuntime) bash(ctx tool.Context, args bashArgs) (map[string]any, error) {
	if strings.TrimSpace(args.Command) == "" {
		return toolError("MISSING_COMMAND", fmt.Errorf("command is required")), nil
	}
	_, workspace, err := r.resolvePath(ctx.SessionID(), ".", false)
	if err != nil {
		return toolError("WORKSPACE_ERROR", err), nil
	}
	timeout := r.commandTimeout
	if args.Timeout > 0 {
		requested := time.Duration(args.Timeout * float64(time.Second))
		if requested < timeout {
			timeout = requested
		}
	}
	return r.runCommand(ctx, workspace, "bash", []string{"-lc", args.Command}, timeout, workspace), nil
}

func (r *LocalRuntime) runSkillScript(
	ctx tool.Context,
	skill *skills.Skill,
	scriptPath string,
	args []string,
) map[string]any {
	if len(args) > maxScriptArguments {
		return toolError("TOO_MANY_ARGUMENTS", fmt.Errorf("script accepts at most %d arguments", maxScriptArguments))
	}
	for _, arg := range args {
		if len(arg) > maxScriptArgumentBytes {
			return toolError("ARGUMENT_TOO_LARGE", fmt.Errorf("each script argument must not exceed %d bytes", maxScriptArgumentBytes))
		}
	}

	program, err := scriptInterpreter(scriptPath)
	if err != nil {
		return toolError("UNSUPPORTED_SCRIPT_TYPE", err)
	}
	_, workspace, err := r.resolvePath(ctx.SessionID(), ".", false)
	if err != nil {
		return toolError("WORKSPACE_ERROR", err)
	}
	commandArgs := make([]string, 0, len(args)+1)
	commandArgs = append(commandArgs, scriptPath)
	commandArgs = append(commandArgs, args...)
	return r.runCommand(ctx, workspace, program, commandArgs, r.commandTimeout, workspace, skill.GetSkillPath())
}

func scriptInterpreter(scriptPath string) (string, error) {
	switch strings.ToLower(filepath.Ext(scriptPath)) {
	case ".py":
		return "python", nil
	case ".js", ".mjs", ".cjs":
		return "node", nil
	case ".sh", ".bash":
		return "bash", nil
	default:
		return "", fmt.Errorf("script type %q is not supported", filepath.Ext(scriptPath))
	}
}

func (r *LocalRuntime) runCommand(
	ctx context.Context,
	workspace string,
	program string,
	args []string,
	timeout time.Duration,
	pythonPaths ...string,
) map[string]any {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(commandContext, program, args...)
	command.Dir = workspace
	command.Env = localCommandEnvironment(pythonPaths...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	stdout := &limitedBuffer{limit: r.maxCommandOutputSize}
	stderr := &limitedBuffer{limit: r.maxCommandOutputSize}
	command.Stdout = stdout
	command.Stderr = stderr

	runErr := command.Run()
	exitCode := 0
	status := "success"
	if runErr != nil {
		status = "error"
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			exitCode = exitError.ExitCode()
		}
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			status = "timeout"
		} else if errors.Is(commandContext.Err(), context.Canceled) {
			status = "canceled"
		}
	}
	result := map[string]any{
		"status": status, "exit_code": exitCode, "stdout": stdout.String(),
		"stderr": stderr.String(), "truncated": stdout.truncated || stderr.truncated,
	}
	if runErr != nil && stderr.buffer.Len() == 0 {
		result["error"] = runErr.Error()
	}
	return result
}

func localCommandEnvironment(pythonPaths ...string) []string {
	paths := make([]string, 0, len(pythonPaths)+1)
	for _, path := range pythonPaths {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if current := os.Getenv("PYTHONPATH"); current != "" {
		paths = append(paths, current)
	}
	return append(os.Environ(), "PYTHONPATH="+strings.Join(paths, string(os.PathListSeparator)))
}

func (r *LocalRuntime) resolvePath(sessionID, requested string, write bool) (string, string, error) {
	workspace, err := r.ensureWorkspace(sessionID)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(requested) == "" {
		return "", workspace, fmt.Errorf("path is required")
	}
	normalized := strings.ReplaceAll(strings.TrimSpace(requested), "\\", "/")
	if filepath.IsAbs(normalized) {
		return "", workspace, fmt.Errorf("absolute paths are not allowed")
	}
	cleaned := filepath.Clean(filepath.FromSlash(normalized))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", workspace, fmt.Errorf("path escapes the session workspace")
	}
	if cleaned == "skills" || strings.HasPrefix(cleaned, "skills"+string(filepath.Separator)) {
		if write {
			return "", workspace, fmt.Errorf("skill source files are read-only")
		}
		parts := strings.Split(filepath.ToSlash(cleaned), "/")
		if len(parts) < 3 {
			return "", workspace, fmt.Errorf("path must identify a file inside a skill")
		}
		skill, ok := r.skills[parts[1]]
		if !ok {
			return "", workspace, fmt.Errorf("skill %q is not available", parts[1])
		}
		target, resolveErr := skill.ResolveResourcePath(strings.Join(parts[2:], "/"))
		return target, workspace, resolveErr
	}

	target := filepath.Join(workspace, cleaned)
	existing := target
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", workspace, statErr
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", workspace, fmt.Errorf("cannot resolve path")
		}
		existing = parent
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", workspace, err
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", workspace, err
	}
	rel, err := filepath.Rel(resolvedWorkspace, resolvedExisting)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", workspace, fmt.Errorf("path escapes the session workspace")
	}
	if !write {
		resolvedTarget, resolveErr := filepath.EvalSymlinks(target)
		if resolveErr != nil {
			return "", workspace, resolveErr
		}
		rel, err = filepath.Rel(resolvedWorkspace, resolvedTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", workspace, fmt.Errorf("path escapes the session workspace")
		}
		target = resolvedTarget
	}
	return target, workspace, nil
}

func (r *LocalRuntime) ensureWorkspace(sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("session ID is required")
	}
	hash := sha256.Sum256([]byte(sessionID))
	sessionDir := hex.EncodeToString(hash[:16])
	workspace := filepath.Join(r.workspaceRoot, sessionDir)
	for _, subdir := range []string{"skills", "uploads", "outputs"} {
		if err := os.MkdirAll(filepath.Join(workspace, subdir), 0o755); err != nil {
			return "", fmt.Errorf("create workspace: %w", err)
		}
	}
	for name, skill := range r.skills {
		target, err := filepath.EvalSymlinks(skill.GetSkillPath())
		if err != nil {
			return "", fmt.Errorf("resolve skill %q: %w", name, err)
		}
		link := filepath.Join(workspace, "skills", name)
		if current, readErr := os.Readlink(link); readErr == nil {
			if current != target {
				return "", fmt.Errorf("workspace skill link %q points to unexpected target", name)
			}
			continue
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect skill link %q: %w", name, readErr)
		}
		if err := os.Symlink(target, link); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("link skill %q: %w", name, err)
		}
	}
	return workspace, nil
}

func toolError(code string, err error) map[string]any {
	return map[string]any{"error_code": code, "error": err.Error()}
}

var errLocalFileTooLarge = errors.New("local file is too large")

func readLocalFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", errLocalFileTooLarge, maxBytes)
	}
	return data, nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return originalLength, nil
	}
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		b.truncated = true
		return originalLength, nil
	}
	_, _ = b.buffer.Write(data)
	return originalLength, nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}
