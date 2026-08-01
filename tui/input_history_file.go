package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

func (i *InputArea) loadInputHistoryFile(path string, limit int) []string {
	if path == "" || limit <= 0 {
		return make([]string, 0)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make([]string, 0)
		}
		i.logger.Error(context.Background(), "input history: read", err, "path", path)
		return make([]string, 0)
	}
	var entries []string
	if err := json.Unmarshal(data, &entries); err != nil {
		i.logger.Error(context.Background(), "input history: parse", err, "path", path)
		return make([]string, 0)
	}
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries
}

func (i *InputArea) saveInputHistoryFile(path string, entries []string) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFilePrivate(path, data)
}
