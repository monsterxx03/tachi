package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// --- /cron ---

// handleCronCommand handles the /cron slash command, listing cron jobs
// scoped to the current thread. Pass an empty threadID to list all jobs.
func (m *Manager) handleCronCommand(threadID string) (string, error) {
	if m.scheduler == nil {
		return "Cron scheduler is not enabled. Set cron.enabled: true in config.yaml.", nil
	}

	allJobs, err := m.scheduler.List()
	if err != nil {
		return "", fmt.Errorf("cron: list: %w", err)
	}

	// Filter by current thread, matching CronTool.handleList() behavior.
	var jobs []*cron.Job
	for _, job := range allJobs {
		if threadID == "" || job.TargetThreadID == threadID {
			jobs = append(jobs, job)
		}
	}

	if len(jobs) == 0 {
		if threadID == "" {
			return "No cron jobs configured.\n\nYou can ask me to create one! Example:\n\"帮我设置一个每天早上9点的日报提醒\"", nil
		}
		return "No cron jobs configured for this thread.", nil
	}

	slices.SortFunc(jobs, func(a, b *cron.Job) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Cron Jobs (%d)\n", len(jobs))

	for _, job := range jobs {
		status := "🟢 Active"
		if job.Status == cron.JobStatusPaused {
			status = "⏸️ Paused"
		}
		if job.Type == cron.JobTypeOneshot {
			status += " · Oneshot"
		}
		fmt.Fprintf(&sb, "\n%s **%s** [%s]\n", status, job.Name, job.ID)
		fmt.Fprintf(&sb, "  Schedule: `%s`\n", job.Schedule)
		fmt.Fprintf(&sb, "  Prompt: %s\n", strutil.Truncate(job.Prompt, 60))
		if !job.LastRunAt.IsZero() {
			icon := "✅"
			if job.LastRunStatus == "error" {
				icon = "❌"
			}
			fmt.Fprintf(&sb, "  Last run: %s %s\n", icon, job.LastRunAt.Format("01-02 15:04"))
		}
	}

	return sb.String(), nil
}

// --- /skill ---

// handleSkillCommand dispatches /skill sub-commands:
//
//	/skill or /skill list  → list skills
//	/skill reload          → re-scan skill directories
//	/skill <name>          → handled via agent turn (not via this method)
func (m *Manager) handleSkillCommand(args string) (string, error) {
	args = strings.TrimSpace(args)
	switch args {
	case "", "list":
		return m.handleSkillList()
	case "reload":
		return m.handleSkillReload()
	default:
		// /skill <name> — activation goes through agent turn.
		// If we reach here via synchronous dispatch, the name is unknown.
		// This shouldn't normally happen because buildHandler intercepts
		// skill activations before handleSlashCommand, but we handle it
		// gracefully via the CommandHandler path.
		if m.skillStore != nil {
			if _, found := m.skillStore.ResolveCommand(args); found {
				return "", fmt.Errorf("skill activation requires an agent turn; send via message, not typed command")
			}
		}
		return "Unknown /skill sub-command. Available: list, reload, <skill-name>", nil
	}
}

// handleSkillList returns a formatted list of all available skills.
func (m *Manager) handleSkillList() (string, error) {
	if m.skillStore == nil {
		return "Skill system not available.", nil
	}

	metas := m.skillStore.List()
	return cmds.FormatSkillList(metas), nil
}

// handleSkillReload re-scans skill directories and returns the updated count.
func (m *Manager) handleSkillReload() (string, error) {
	if m.skillStore == nil {
		return "Skill system not available.", nil
	}

	// Re-scan using the same directory scope the store was constructed
	// with. (Tests that injected a hermetic store via Config.SkillStore
	// keep their scope; production callers that used the default
	// ~/.tachi/skills layout get the same dirs back.)
	m.skillStore = skill.NewStoreWithDirs(m.skillStore.Dirs(), m.skillStore.Sources())
	metas := m.skillStore.List()

	// Propagate the reload to all cached agents so their SkillListReminder
	// re-fires and the skill tool uses the updated store.
	m.reloadAgentSkills()

	return fmt.Sprintf("Skills 已重新加载 — 发现 %d 个 skill(s)", len(metas)), nil
}

// reloadAgentSkills calls ReloadSkills on every cached AIAgent so the new
// skill store is picked up, the SkillListReminder is marked dirty, and the
// skill tool is re-registered with the updated store.
//
// Lock ordering: agentCacheMu → ca.mu (consistent with acquireAgent and
// evictAllAgents). Each ca.mu is acquired and released individually so that
// agents in use on other threads are naturally serialized.
func (m *Manager) reloadAgentSkills() {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	for _, ca := range m.agentCache {
		ca.mu.Lock()
		if ca.agent != nil {
			ca.agent.ReloadSkills()
		}
		ca.mu.Unlock()
	}
	m.logger.Info(context.Background(), "channel: skill reload propagated", "count", len(m.agentCache))
}

// prepareSkillActivation builds the user message content for skill activation.
// Returns the activation message string, or an error message as string + error.
func (m *Manager) prepareSkillActivation(skillName string, extraArgs string) (string, string, error) {
	if m.skillStore == nil {
		return "", "Skill system not available.", fmt.Errorf("skill system not available")
	}

	sk, err := m.skillStore.Load(skillName)
	if err != nil {
		return "", fmt.Sprintf("Skill **%s** 未找到。使用 `/skill` 查看可用 skills。", skillName), err
	}

	msg := skill.BuildActivationMessage(sk, extraArgs)
	return msg, "", nil
}

// isSkillActivation checks if the message is a skill activation pattern:
//   - /skill <name> [args]
//   - /<skillname> [args]
//
// Returns (skillName, extraArgs, isActivation).
func (m *Manager) isSkillActivation(content string) (string, string, bool) {
	parts := strings.Fields(strings.TrimPrefix(content, "/"))
	if len(parts) == 0 {
		return "", "", false
	}

	// /skill <name> [args]
	if strings.HasPrefix(content, "/skill ") && len(parts) >= 2 {
		sub := strings.TrimPrefix(content, "/skill ")
		subParts := strings.Fields(sub)
		if len(subParts) == 0 {
			return "", "", false
		}
		skillName := subParts[0]
		if skillName == "list" || skillName == "reload" {
			return "", "", false // handled synchronously
		}
		extraArgs := ""
		if len(subParts) > 1 {
			extraArgs = strings.Join(subParts[1:], " ")
		}
		return skillName, extraArgs, true
	}

	// /<skillname> [args]
	skillName := parts[0]
	if name, found := m.skillStore.ResolveCommand(skillName); found {
		extraArgs := ""
		if len(parts) > 1 {
			extraArgs = strings.Join(parts[1:], " ")
		}
		return name, extraArgs, true
	}

	return "", "", false
}
