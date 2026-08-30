package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverSkillsFromDirDoesNotLoadResourceTree(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "document-tools")
	if err := os.MkdirAll(filepath.Join(skillDir, "assets", "large-tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: document-tools\ndescription: creates documents\n---\nUse the assets on demand.\n")
	writeTestFile(t, filepath.Join(skillDir, "assets", "large-tree", "template.txt"), "template")

	discovered, err := DiscoverSkillsFromDir(root)
	if err != nil {
		t.Fatalf("DiscoverSkillsFromDir() error = %v", err)
	}
	if got, want := len(discovered), 1; got != want {
		t.Fatalf("len(discovered) = %d, want %d", got, want)
	}
	if discovered[0].Resources != nil {
		t.Fatal("discovered skill eagerly loaded resources")
	}
	if got, want := discovered[0].Instructions, "Use the assets on demand."; got != want {
		t.Fatalf("Instructions = %q, want %q", got, want)
	}
	data, err := discovered[0].ReadResource("assets/large-tree/template.txt", 1024)
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}
	if got, want := string(data), "template"; got != want {
		t.Fatalf("ReadResource() = %q, want %q", got, want)
	}
}

func TestReadResourceRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "safe-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: safe-skill\ndescription: safe resources\n---\ninstructions\n")
	outside := filepath.Join(root, "outside.txt")
	writeTestFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(skillDir, "assets", "escape.txt")); err != nil {
		t.Fatal(err)
	}

	skill, err := parseSkillMD(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, resourcePath := range []string{"../outside.txt", "assets/escape.txt", outside} {
		if _, err := skill.ReadResource(resourcePath, 1024); err == nil {
			t.Fatalf("ReadResource(%q) error = nil, want boundary error", resourcePath)
		}
	}
}

func TestReadResourcePreservesInMemorySkills(t *testing.T) {
	skill := &Skill{Resources: &Resources{
		References: map[string]string{"nested/guide.md": "guide"},
		Assets:     map[string]string{"template.txt": "asset"},
		Scripts:    map[string]*Script{"run.py": {Src: "print('ok')"}},
	}}
	for resourcePath, want := range map[string]string{
		"references/nested/guide.md": "guide",
		"assets/template.txt":        "asset",
		"scripts/run.py":             "print('ok')",
	} {
		data, err := skill.ReadResource(resourcePath, 1024)
		if err != nil {
			t.Fatalf("ReadResource(%q) error = %v", resourcePath, err)
		}
		if got := string(data); got != want {
			t.Fatalf("ReadResource(%q) = %q, want %q", resourcePath, got, want)
		}
	}
	for _, invalid := range []string{"../secret", "unknown/file", "assets/missing.txt"} {
		if _, err := skill.ReadResource(invalid, 1024); err == nil {
			t.Fatalf("ReadResource(%q) error = nil", invalid)
		}
	}
}

func TestDiscoverSkillsFromConfiguredRoot(t *testing.T) {
	root := os.Getenv("VEADK_TEST_SKILLS_ROOT")
	if root == "" {
		t.Skip("VEADK_TEST_SKILLS_ROOT is not set")
	}
	discovered, err := DiscoverSkillsFromDir(root)
	if err != nil {
		t.Fatalf("DiscoverSkillsFromDir() error = %v", err)
	}
	if len(discovered) == 0 {
		t.Fatal("DiscoverSkillsFromDir() returned no skills")
	}
	for _, skill := range discovered {
		t.Logf("discovered skill: %s", skill.Name())
	}
}

func BenchmarkDiscoverSkillsFromConfiguredRoot(b *testing.B) {
	root := os.Getenv("VEADK_TEST_SKILLS_ROOT")
	if root == "" {
		b.Skip("VEADK_TEST_SKILLS_ROOT is not set")
	}
	b.ResetTimer()
	for range b.N {
		if _, err := DiscoverSkillsFromDir(root); err != nil {
			b.Fatal(err)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
