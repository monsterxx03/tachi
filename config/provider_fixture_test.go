package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"gopkg.in/yaml.v3"
)

// Fixtures used to put `providers:` straight into config.yaml. That is no longer a shape the
// loader accepts — provider 定义住在 provider.*.json 里（见 provider_json.go）——所以测试的写法
// 也跟着换：写下一份 yaml，再把里面那一段搬到 `provider.test.json`。
//
// writeConfigAndProviders 就是这次搬家，测试本体仍然只写「一个 yaml 文档」。
func writeConfigAndProviders(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	splitProvidersFromYAML(t, dir, path)
	return path
}

// splitProvidersFromYAML moves the top-level `providers:` block out of the yaml at path and into
// provider.test.json beside it. A document without that block is left alone.
func splitProvidersFromYAML(t *testing.T, dir, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc := &yaml.Node{}
	if err := yaml.Unmarshal(data, doc); err != nil || len(doc.Content) == 0 {
		return // 不是 yaml 文档：留给被测代码去报错
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "providers" {
			continue
		}
		var providers []ProviderConfig
		if err := root.Content[i+1].Decode(&providers); err != nil {
			t.Fatalf("fixture providers: %v", err)
		}
		writeProviderFixture(t, dir, providers)
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
		out, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatalf("fixture re-marshal: %v", err)
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatalf("rewrite fixture: %v", err)
		}
		return
	}
}

// writeProviderFixture writes providers into `provider.test.json` inside dir.
func writeProviderFixture(t *testing.T, dir string, providers []ProviderConfig) string {
	t.Helper()
	path := filepath.Join(dir, providerFilePrefix+"test"+providerFileSuffix)
	data, err := fileutil.MarshalJSON(providerFile{Providers: providers})
	if err != nil {
		t.Fatalf("marshal provider fixture: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write provider fixture: %v", err)
	}
	return path
}
