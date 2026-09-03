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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const DefaultMaxResourceBytes int64 = 16 * 1024 * 1024

var (
	ErrInvalidResourcePath = errors.New("invalid skill resource path")
	ErrResourceNotFound    = errors.New("skill resource not found")
	ErrResourceTooLarge    = errors.New("skill resource is too large")
	ErrResourceNotRegular  = errors.New("skill resource is not a regular file")
	ErrResourceChanged     = errors.New("skill resource changed while opening")
	ErrResourceRead        = errors.New("skill resource read failed")
)

// ReadResource reads a relative file without allowing access outside the skill
// directory. A zero maxBytes uses
// DefaultMaxResourceBytes; negative limits are rejected.
func (s *Skill) ReadResource(resourcePath string, maxBytes int64) ([]byte, error) {
	return s.ReadResourceContext(context.Background(), resourcePath, maxBytes)
}

// ReadResourceContext is ReadResource with cancellation support.
func (s *Skill) ReadResourceContext(ctx context.Context, resourcePath string, maxBytes int64) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxResourceBytes
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("%w: maximum size must not be negative", ErrResourceTooLarge)
	}

	cleaned, err := cleanResourcePath(resourcePath)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrResourceNotFound
	}
	if strings.TrimSpace(s.SkillMDPath) == "" {
		return s.readInMemoryResource(ctx, cleaned, maxBytes)
	}
	return s.readDiskResource(ctx, cleaned, maxBytes)
}

func cleanResourcePath(resourcePath string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(resourcePath), "\\", "/")
	if normalized == "" || filepath.IsAbs(filepath.FromSlash(normalized)) || hasWindowsVolumePrefix(normalized) {
		return "", fmt.Errorf("%w: path must be relative", ErrInvalidResourcePath)
	}
	cleaned := filepath.Clean(filepath.FromSlash(normalized))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes the skill directory", ErrInvalidResourcePath)
	}
	return cleaned, nil
}

func hasWindowsVolumePrefix(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':' && path[2] == '/'
}

func (s *Skill) readDiskResource(ctx context.Context, cleaned string, maxBytes int64) ([]byte, error) {
	root, err := os.OpenRoot(s.GetSkillPath())
	if err != nil {
		return nil, fmt.Errorf("%w: open skill root", ErrResourceRead)
	}
	defer func() { _ = root.Close() }()

	if err := validateResolvedResourcePath(s.GetSkillPath(), cleaned); err != nil {
		return nil, err
	}
	before, err := root.Stat(cleaned)
	if err != nil {
		return nil, classifyResourceOpenError(err)
	}
	if !before.Mode().IsRegular() {
		return nil, ErrResourceNotRegular
	}
	if before.Size() > maxBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResourceTooLarge, maxBytes)
	}

	file, err := root.Open(cleaned)
	if err != nil {
		return nil, classifyResourceOpenError(err)
	}
	defer func() { _ = file.Close() }()
	afterOpen, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat opened resource", ErrResourceRead)
	}
	if !afterOpen.Mode().IsRegular() {
		return nil, ErrResourceNotRegular
	}
	if !os.SameFile(before, afterOpen) {
		return nil, ErrResourceChanged
	}

	data, err := readBoundedContext(ctx, file, maxBytes, ErrResourceTooLarge)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrResourceTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: read resource", ErrResourceRead)
	}
	afterRead, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat resource after read", ErrResourceRead)
	}
	if afterRead.Size() != afterOpen.Size() || !afterRead.ModTime().Equal(afterOpen.ModTime()) {
		return nil, ErrResourceChanged
	}
	return data, nil
}

func validateResolvedResourcePath(rootPath, cleaned string) error {
	resolvedRoot, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return fmt.Errorf("%w: resolve skill root", ErrResourceRead)
	}
	resolvedTarget, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, cleaned))
	if err != nil {
		return classifyResourceOpenError(err)
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil {
		return fmt.Errorf("%w: resolve resource path", ErrResourceRead)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: symbolic link escapes the skill directory", ErrInvalidResourcePath)
	}
	return nil
}

func classifyResourceOpenError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrResourceNotFound, fs.ErrNotExist)
	}
	return fmt.Errorf("%w: open resource", ErrResourceRead)
}

func (s *Skill) readInMemoryResource(ctx context.Context, cleaned string, maxBytes int64) ([]byte, error) {
	if s.Resources == nil {
		return nil, ErrResourceNotFound
	}
	parts := strings.SplitN(filepath.ToSlash(cleaned), "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, fmt.Errorf("%w: in-memory paths must start with references/, assets/, or scripts/", ErrInvalidResourcePath)
	}
	var (
		content string
		found   bool
	)
	switch parts[0] {
	case "references":
		content, found = s.Resources.GetReference(parts[1])
	case "assets":
		content, found = s.Resources.GetAsset(parts[1])
	case "scripts":
		var script *Script
		script, found = s.Resources.GetScript(parts[1])
		if found && script != nil {
			content = script.Src
		} else {
			found = false
		}
	default:
		return nil, fmt.Errorf("%w: in-memory paths must start with references/, assets/, or scripts/", ErrInvalidResourcePath)
	}
	if !found {
		return nil, ErrResourceNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data := []byte(content)
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResourceTooLarge, maxBytes)
	}
	return data, nil
}
