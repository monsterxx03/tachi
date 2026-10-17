package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// provider 定义住在 provider.*.json 里（provider_json.go）。这些测试钉住的是那几条规则：文件
// 怎么被找到、同名怎么办、保存时条目去哪个文件、空掉的文件会不会留下。

// loadProvidersFixture writes one config.yaml plus the named provider files and loads the result.
func loadProvidersFixture(t *testing.T, files map[string]string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("language: zh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg, dir
}

// Every provider.*.json in the directory is read, and each entry remembers the file it came from —
// that is the whole mechanism a save writes back with.
func TestLoadProvidersReadsEveryProviderFileAndStampsItsSource(t *testing.T) {
	cfg, dir := loadProvidersFixture(t, map[string]string{
		"provider.alpha.json": `{"providers": [{"name": "a1", "type": "openai", "model": "m1"},
		                                       {"name": "a2", "type": "openai", "model": "m2"}]}`,
		"provider.beta.json": `{"providers": [{"name": "b1", "type": "anthropic", "model": "m3"}]}`,
		// Not provider files: they must be ignored, not parsed.
		"mcp.json":            `{"servers": []}`,
		"provider.json":       `{"providers": [{"name": "nameless-file"}]}`,
		"notes.json":          `not even json`,
		"provider.tmpl.jsonx": `{}`,
	})
	if names := providerNamesOf(cfg); strings.Join(names, ",") != "a1,a2,b1" {
		t.Fatalf("providers = %v, want a1,a2,b1 in file-name order", names)
	}
	for _, p := range cfg.Providers {
		want := "provider.alpha.json"
		if p.Name == "b1" {
			want = "provider.beta.json"
		}
		if filepath.Base(p.SourceFile) != want {
			t.Errorf("%s came from %q, want %s", p.Name, p.SourceFile, want)
		}
		if !strings.HasPrefix(p.SourceFile, dir) {
			t.Errorf("%s: SourceFile %q is not in the config's directory", p.Name, p.SourceFile)
		}
	}
}

// A name defined in two files is refused rather than resolved: neither file is "more true", and a
// silent winner means an endpoint that answers differently depending on a file name.
func TestLoadProvidersRefusesADuplicateName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("language: zh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"provider.one.json", "provider.two.json"} {
		body := `{"providers": [{"name": "dup", "type": "openai", "model": "m"}]}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("a duplicate provider name across two files must be refused")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("the error must name the provider: %v", err)
	}
}

// A `providers:` block left in config.yaml is a leftover from an older install, and it is dropped
// without a word: the files are the only source, so the stale block has nothing to say — and a
// load that refused it would turn an upgrade into a startup failure.
func TestLoadFromIgnoresProvidersInYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "language: zh\nproviders:\n  - name: old\n    type: openai\n    model: stale\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProviderFixture(t, dir, []ProviderConfig{{Name: "live", Type: ProviderTypeOpenAI, Model: "m"}})

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a stale providers: block must not fail the load: %v", err)
	}
	if names := strings.Join(providerNamesOf(cfg), ","); names != "live" {
		t.Errorf("providers = %v, want only what the files define", names)
	}
	if cfg.Language != "zh" {
		t.Errorf("the rest of the file must still load: language = %q", cfg.Language)
	}
}

// A save puts each entry back into the file it came from, gives a new entry a file of its own, and
// removes a file nothing is left in.
func TestPlanProviderFilesRoutesEachEntryToItsFile(t *testing.T) {
	cfg, dir := loadProvidersFixture(t, map[string]string{
		"provider.alpha.json": `{"providers": [{"name": "a1", "type": "openai", "model": "m1"}]}`,
		"provider.beta.json":  `{"providers": [{"name": "b1", "type": "openai", "model": "m2"}]}`,
	})
	// Edit a1 in place, drop b1 altogether, and add a brand new one.
	cfg.Providers[0].Model = "m1-edited"
	cfg.Providers = cfg.Providers[:1]
	cfg.Providers = append(cfg.Providers, ProviderConfig{
		Name: "fresh", Type: ProviderTypeOpenAI, Model: "m3", BaseURL: "https://example.test", APIKey: "k",
	})

	plan, err := PlanProviderFiles(dir, cfg.Providers)
	if err != nil {
		t.Fatalf("PlanProviderFiles: %v", err)
	}
	if _, ok := plan.Files[filepath.Join(dir, "provider.alpha.json")]; !ok {
		t.Errorf("the edited entry must go back to its own file: %v", plan.Paths())
	}
	freshPath := filepath.Join(dir, "provider.fresh.json")
	if _, ok := plan.Files[freshPath]; !ok {
		t.Errorf("a new entry gets a file named after it: %v", plan.Paths())
	}
	if len(plan.Deletes) != 1 || filepath.Base(plan.Deletes[0]) != "provider.beta.json" {
		t.Errorf("the emptied file must be deleted: %v", plan.Deletes)
	}
	if strings.Contains(string(plan.Files[freshPath]), "SourceFile") {
		t.Errorf("SourceFile must not be written into a file: %s", plan.Files[freshPath])
	}

	undo, err := plan.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "provider.beta.json")); !os.IsNotExist(err) {
		t.Errorf("provider.beta.json should be gone: %v", err)
	}
	reloaded, err := LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if names := strings.Join(providerNamesOf(reloaded), ","); names != "a1,fresh" {
		t.Errorf("providers after Apply = %v, want a1,fresh", names)
	}
	if reloaded.Providers[0].Model != "m1-edited" {
		t.Errorf("a1 model = %q, want the edit", reloaded.Providers[0].Model)
	}

	// The undo puts the directory back: this is what a failed config.yaml write relies on.
	undo()
	back, err := LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload after undo: %v", err)
	}
	if names := strings.Join(providerNamesOf(back), ","); names != "a1,b1" {
		t.Errorf("providers after undo = %v, want a1,b1", names)
	}
	if _, err := os.Stat(freshPath); !os.IsNotExist(err) {
		t.Errorf("the file the undo created must be removed again: %v", err)
	}
}

// A file keeps its own mode through a save: it holds an api_key.
func TestProviderFileKeepsItsMode(t *testing.T) {
	cfg, dir := loadProvidersFixture(t, map[string]string{
		"provider.alpha.json": `{"providers": [{"name": "a1", "type": "openai", "model": "m1"}]}`,
	})
	path := filepath.Join(dir, "provider.alpha.json")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Providers[0].Model = "changed"
	if _, err := SyncProviderFiles(dir, cfg.Providers); err != nil {
		t.Fatalf("SyncProviderFiles: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// A config with no provider files loads fine and has no providers: that is a state before the
// first install writes one, not an error.
func TestLoadProvidersWithNoFiles(t *testing.T) {
	cfg, _ := loadProvidersFixture(t, nil)
	if len(cfg.Providers) != 0 {
		t.Errorf("providers = %+v, want none", cfg.Providers)
	}
}
