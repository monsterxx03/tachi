package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProjectPermissions_Missing(t *testing.T) {
	p, err := LoadProjectPermissions(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(p.Bash.Deny) != 0 || len(p.Bash.Ask) != 0 || len(p.Bash.Allow) != 0 {
		t.Errorf("missing file should return zero rules, got %+v", p.Bash)
	}

	// Empty root is also fine.
	if _, err := LoadProjectPermissions(""); err != nil {
		t.Fatalf("empty root should not error, got %v", err)
	}
}

func TestLoadProjectPermissions_Valid(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".tachi")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `permissions:
  bash:
    deny:
      - "git push --force*"
    ask:
      - "rm *"
      - "sudo *"
    allow:
      - "git status*"
`
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadProjectPermissions(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Bash.Deny) != 1 || p.Bash.Deny[0] != "git push --force*" {
		t.Errorf("unexpected deny rules: %v", p.Bash.Deny)
	}
	if len(p.Bash.Ask) != 2 || p.Bash.Ask[0] != "rm *" {
		t.Errorf("unexpected ask rules: %v", p.Bash.Ask)
	}
	if len(p.Bash.Allow) != 1 || p.Bash.Allow[0] != "git status*" {
		t.Errorf("unexpected allow rules: %v", p.Bash.Allow)
	}
}

func TestLoadProjectPermissions_InvalidYAML(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".tachi")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte("permissions: [broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProjectPermissions(root); err == nil {
		t.Error("invalid YAML should return an error")
	}
}
