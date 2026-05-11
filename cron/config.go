package cron

import (
	"os"
	"path/filepath"
)

// DefaultStorePath returns the default path for crons.json.
func DefaultStorePath() string {
	return filepath.Join(configDir(), "crons.json")
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tachi"
	}
	return filepath.Join(home, ".tachi")
}