package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

func loadInputHistoryFile(path string, limit int) []string {
	if path == "" || limit <= 0 {
		return make([]string, 0)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make([]string, 0)
		}
		debuglog.Log("input history: read %s: %v", path, err)
		return make([]string, 0)
	}
	var entries []string
	if err := json.Unmarshal(data, &entries); err != nil {
		debuglog.Log("input history: parse %s: %v", path, err)
		return make([]string, 0)
	}
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries
}

func saveInputHistoryFile(path string, entries []string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
