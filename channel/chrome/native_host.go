package chrome

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ManifestName is the native messaging host name (also used as the manifest
// filename: com.tachi.chrome.json).
const ManifestName = "com.tachi.chrome"

// NativeManifest represents the Chrome Native Messaging host manifest file.
//
// Chrome reads this file to discover native messaging hosts. It must be
// placed in the correct system location:
//
//	macOS: ~/Library/Application Support/Google/Chrome/NativeMessagingHosts/
//	Linux: ~/.config/google-chrome/NativeMessagingHosts/
//	Windows: %APPDATA%\Google\Chrome\NativeMessagingHosts\
type NativeManifest struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Path          string   `json:"path"`
	Type          string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// InstallExtensionID writes the Native Messaging manifest file for the
// given extension ID. After calling this, the extension can connect to
// Tachi via chrome.runtime.connectNative("com.tachi.chrome").
//
// The manifest points to the tachi binary. Chrome will launch tachi with
// no arguments by default — the extension should pass --chrome as a CLI
// arg via its connectNative call (Chrome passes args from the manifest
// to the native host process).
func InstallExtensionID(extensionID string) error {
	manifestPath, err := manifestFilePath()
	if err != nil {
		return fmt.Errorf("chrome: manifest path: %w", err)
	}

	manifest := NativeManifest{
		Name:        ManifestName,
		Description: "Tachi Chrome Extension Bridge — AI agent for browser",
		Path:        tachiBinaryPath(),
		Type:        "stdio",
		AllowedOrigins: []string{
			"chrome-extension://" + extensionID + "/",
		},
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("chrome: marshal manifest: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		return fmt.Errorf("chrome: create manifest dir: %w", err)
	}

	if err := os.WriteFile(manifestPath, data, 0644); err != nil {
		return fmt.Errorf("chrome: write manifest: %w", err)
	}

	fmt.Printf("✅ Chrome Native Messaging manifest installed:\n  %s\n", manifestPath)
	fmt.Printf("   Extension ID: %s\n", extensionID)
	fmt.Printf("   Binary: %s\n", tachiBinaryPath())
	return nil
}

// Uninstall removes the Native Messaging manifest file.
func Uninstall() error {
	manifestPath, err := manifestFilePath()
	if err != nil {
		return fmt.Errorf("chrome: manifest path: %w", err)
	}

	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		fmt.Println("✅ No manifest found (already uninstalled).")
		return nil
	}

	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("chrome: remove manifest: %w", err)
	}

	fmt.Printf("✅ Chrome Native Messaging manifest removed:\n  %s\n", manifestPath)
	return nil
}

// ManifestPath returns the absolute path where the native messaging manifest
// would be installed, without modifying the filesystem.
func ManifestPath() (string, error) {
	return manifestFilePath()
}

// defaultManifestPath computes the target path for the native messaging manifest
// based on the current operating system.
func defaultManifestPath() (string, error) {
	var base string

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		base = filepath.Join(home, "Library", "Application Support", "Google", "Chrome")

	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		// Use google-chrome; also works for chromium, brave, edge etc.
		// Users can symlink or manually copy if using a different Chromium-based browser.
		base = filepath.Join(home, ".config", "google-chrome")

	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("%%APPDATA%% not set")
		}
		base = filepath.Join(appData, "Google", "Chrome")

	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return filepath.Join(base, "NativeMessagingHosts", ManifestName+".json"), nil
}

// manifestFilePath is a variable so tests can override it.
var manifestFilePath = defaultManifestPath

// tachiBinaryPath returns the absolute path to the currently running tachi
// binary. This path is written into the manifest so Chrome knows which
// executable to launch.
func tachiBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		// Fallback: try looking it up in PATH.
		if path, err := os.Executable(); err == nil {
			return path
		}
		return "tachi" // best-effort
	}
	return exe
}
