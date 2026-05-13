package cron

import (
	"github.com/monsterxx03/tachi/config"
)

// DefaultStorePath returns the default path for crons.json.
func DefaultStorePath() string {
	return config.CronStorePath()
}