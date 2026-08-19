package acp

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
)

func TestBuildSystemPromptForCwd(t *testing.T) {
	t.Run("contains basic info", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{Language: "中文"}, "/home/user/project", nil, agent.ModeAuto, "")
		assert.Contains(t, prompt, "Reply in 中文")
		assert.Contains(t, prompt, "- Working directory: /home/user/project")
		assert.Contains(t, prompt, "Tachi")
	})

	t.Run("language fallback", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{}, "/tmp", nil, agent.ModeAuto, "")
		assert.Contains(t, prompt, "Reply in ")
	})

	t.Run("git detection", func(t *testing.T) {
		// Use a temp dir that's not a git repo
		prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, "/nonexistent-dir", nil, agent.ModeAuto, "")
		assert.Contains(t, prompt, "Git repository: no")
	})

	t.Run("plan mode appends plan prompt", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, "/tmp", nil, agent.ModePlan, "")
		assert.Contains(t, prompt, "PLAN MODE")
		assert.Contains(t, prompt, "SavePlan")
	})

	t.Run("extra system prompt appended", func(t *testing.T) {
		prompt := buildSystemPromptForCwd(&config.Config{Language: "en", ExtraSystemPrompt: "## Extra\nCustom content."}, "/tmp", nil, agent.ModeAuto, "")
		assert.Contains(t, prompt, "## Extra\nCustom content.")
		// Appended after the environment section, before the plan-mode prompt.
		promptPlan := buildSystemPromptForCwd(&config.Config{Language: "en", ExtraSystemPrompt: "## Extra\nCustom content."}, "/tmp", nil, agent.ModePlan, "")
		assert.Contains(t, promptPlan, "## Extra\nCustom content.")
		assert.True(t, strings.Index(promptPlan, "## Extra") < strings.Index(promptPlan, "PLAN MODE"),
			"extra system prompt must precede the plan-mode supplement")
	})
}

func TestBuildSystemPromptForCwd_InGitRepo(t *testing.T) {
	// Create temp dir and init git repo
	tmpDir := t.TempDir()
	err := exec.Command("git", "-C", tmpDir, "init").Run()
	require.NoError(t, err)

	prompt := buildSystemPromptForCwd(&config.Config{Language: "en"}, tmpDir, nil, agent.ModeAuto, "")
	assert.Contains(t, prompt, "Git repository: yes")
	assert.Contains(t, prompt, "Working directory: "+tmpDir)
}
