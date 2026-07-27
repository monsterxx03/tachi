package acp

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
)

func TestBuildSystemPromptForCwd(t *testing.T) {
	t.Run("contains basic info", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{Language: "中文"}, "/home/user/project", agent.ModeAuto, "")
		assert.Contains(t, prompt, "Reply in 中文")
		assert.Contains(t, prompt, "- Working directory: /home/user/project")
		assert.Contains(t, prompt, "Tachi")
	})

	t.Run("language fallback", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{}, "/tmp", agent.ModeAuto, "")
		assert.Contains(t, prompt, "Reply in ")
	})

	t.Run("git detection", func(t *testing.T) {
		// Use a temp dir that's not a git repo
		prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, "/nonexistent-dir", agent.ModeAuto, "")
		assert.Contains(t, prompt, "Git repository: no")
	})

	t.Run("plan mode appends plan prompt", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, "/tmp", agent.ModePlan, "")
		assert.Contains(t, prompt, "PLAN MODE")
		assert.Contains(t, prompt, "SavePlan")
	})
}

func TestBuildSystemPromptForCwd_InGitRepo(t *testing.T) {
	// Create temp dir and init git repo
	tmpDir := t.TempDir()
	err := exec.Command("git", "-C", tmpDir, "init").Run()
	require.NoError(t, err)

	prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, tmpDir, agent.ModeAuto, "")
	assert.Contains(t, prompt, "Git repository: yes")
	assert.Contains(t, prompt, "Working directory: "+tmpDir)
}
