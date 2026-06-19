package chrome

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManifestPath(t *testing.T) {
	path, err := manifestFilePath()
	if err != nil {
		t.Fatalf("manifestFilePath: %v", err)
	}
	if path == "" {
		t.Fatal("empty manifest path")
	}

	// Verify the path structure.
	if !filepath.IsAbs(path) {
		t.Errorf("manifest path should be absolute, got: %s", path)
	}
	if filepath.Base(filepath.Dir(path)) != "NativeMessagingHosts" {
		t.Errorf("manifest should be in NativeMessagingHosts dir, got: %s", path)
	}
	if filepath.Base(path) != ManifestName+".json" {
		t.Errorf("manifest filename should be %s.json, got: %s", ManifestName, filepath.Base(path))
	}
}

func TestInstallUninstall(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Native Messaging manifest installation only tested on darwin/linux")
	}

	// Use temp directory instead of real Chrome dir.
	tmpDir := t.TempDir()
	origManifestPath := manifestFilePath

	// Override manifestFilePath to use temp dir.
	manifestFilePath = func() (string, error) {
		return filepath.Join(tmpDir, ManifestName+".json"), nil
	}
	defer func() {
		manifestFilePath = origManifestPath
	}()

	// Test install.
	extID := "abcdefghijklmnopabcdefghijklmnop"
	if err := InstallExtensionID(extID); err != nil {
		t.Fatalf("InstallExtensionID: %v", err)
	}

	// Verify manifest file exists.
	manifestPath, _ := manifestFilePath()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// Verify content.
	if !contains(string(data), extID) {
		t.Errorf("manifest should contain extension ID %q", extID)
	}
	if !contains(string(data), ManifestName) {
		t.Errorf("manifest should contain name %q", ManifestName)
	}
	if !contains(string(data), "tachi") {
		t.Errorf("manifest should reference tachi binary")
	}

	// Test uninstall.
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest should be removed after uninstall, err: %v", err)
	}
}

func TestTachiBinaryPath(t *testing.T) {
	path := tachiBinaryPath()
	if path == "" {
		t.Fatal("empty binary path")
	}
	// Should be an absolute path or a simple name.
	if filepath.IsAbs(path) {
		// Should exist (since we're running inside the test binary).
		// The test binary itself may be in a temp dir, so just verify
		// it looks reasonable.
		if !contains(path, "tachi") && !contains(path, "test") {
			t.Logf("binary path doesn't contain 'tachi' or 'test': %s", path)
		}
	}
}
