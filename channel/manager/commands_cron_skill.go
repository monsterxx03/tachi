package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/cron"
)

// --- /cron ---

// cronUsage is what /cron answers when it is given a verb without a job id.
const cronUsage = "Usage:\n" +
	"  /cron                  — list this thread's cron jobs\n" +
	"  /cron <id>             — show one job in full\n" +
	"  /cron pause <id>       — stop scheduling it\n" +
	"  /cron resume <id>      — schedule it again\n" +
	"  /cron run <id>         — fire it once now\n" +
	"  /cron delete <id>      — delete it"

// handleCronCommand handles the /cron slash command for one thread. Pass an
// empty threadID to address every job (the unthreaded, global call).
//
//	/cron                   list the jobs of this thread
//	/cron <id>              full detail of one job
//	/cron pause|resume|run|delete <id>
//
// Every by-id action is scoped exactly like the list is: a job this thread
// cannot see reads as "not found" rather than as a permission error, so an id
// seen in another conversation cannot be operated on here by accident — the
// same invariant the model-side Cron tool enforces with its allowed() check.
// "Cannot see it" and "does not exist" are deliberately the same answer.
func (m *Manager) handleCronCommand(threadID, args string) (string, error) {
	if m.scheduler == nil {
		return "Cron scheduler is not enabled. Set cron.enabled: true in config.yaml.", nil
	}

	verb, id := parseCronCommand(args)
	if verb == "list" {
		return m.listCronJobs(threadID)
	}
	if id == "" {
		return cronUsage, nil
	}

	job, err := m.cronJobForThread(threadID, id)
	if err != nil {
		return "", err
	}

	switch verb {
	case "get":
		return cron.FormatJobDetail(job, threadID), nil
	case "pause":
		paused, err := m.scheduler.Pause(job.ID)
		if err != nil {
			return "", fmt.Errorf("cron: pause %s: %w", job.ID, err)
		}
		return "⏸️ Paused.\n\n" + cron.FormatJobDetail(paused, threadID), nil
	case "resume":
		resumed, err := m.scheduler.Resume(job.ID)
		if err != nil {
			return "", fmt.Errorf("cron: resume %s: %w", job.ID, err)
		}
		return "▶️ Resumed.\n\n" + cron.FormatJobDetail(resumed, threadID), nil
	case "run":
		// Asynchronous: the run goes through the same path as a scheduled fire,
		// so LastRun and a oneshot's own cleanup behave identically.
		if err := m.scheduler.RunNow(job.ID); err != nil {
			return "", fmt.Errorf("cron: run %s: %w", job.ID, err)
		}
		return fmt.Sprintf("▶️ Triggered **%s** [%s] — running now; the result is delivered to this thread when it finishes.", job.Name, job.ID), nil
	case "delete":
		// Echo the full job BEFORE removing it: the prompt is the part that
		// cannot be recovered afterwards, so the reply has to carry it.
		detail := cron.FormatJobDetail(job, threadID)
		if err := m.scheduler.Delete(job.ID); err != nil {
			return "", fmt.Errorf("cron: delete %s: %w", job.ID, err)
		}
		return "🗑️ Deleted (shown below so it can be recreated):\n\n" + detail, nil
	}

	return cronUsage, nil
}

// parseCronCommand splits /cron arguments into a verb and a job id. A bare id
// means "show it" — that is the shape people type first, and it matches what
// the list prints next to each job.
func parseCronCommand(args string) (verb, id string) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "list", ""
	}

	switch fields[0] {
	case "list":
		return "list", ""
	case "get", "show":
		verb = "get"
	case "pause", "resume", "run", "delete":
		verb = fields[0]
	default:
		// Not a verb: the whole first field is the id.
		return "get", fields[0]
	}
	if len(fields) > 1 {
		id = fields[1]
	}
	return verb, id
}

// cronJobForThread loads one job and checks the thread may see it. Errors say
// "not found" for both a missing id and a job owned by another thread.
func (m *Manager) cronJobForThread(threadID, id string) (*cron.Job, error) {
	job, err := m.scheduler.Get(id)
	if err != nil {
		return nil, fmt.Errorf("cron: get %s: %w", id, err)
	}
	if job == nil || (threadID != "" && job.TargetThreadID != threadID) {
		return nil, fmt.Errorf("cron: job %q not found in this thread (use /cron to list)", id)
	}
	return job, nil
}

// listCronJobs renders the jobs this thread can see, oldest first.
func (m *Manager) listCronJobs(threadID string) (string, error) {
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
	fmt.Fprintf(&sb, "📋 Cron Jobs (%d)\n\n", len(jobs))
	for _, job := range jobs {
		sb.WriteString(cron.FormatJobSummary(job, threadID))
		sb.WriteString("\n")
	}
	sb.WriteString("`/cron <id>` for details · `/cron pause|resume|run|delete <id>`")

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

// reloadAgentSkills calls ReloadSkills on every cached AIAgent so the new skill store is picked up,
// the SkillListReminder is marked dirty, and the skill tool is re-registered with the updated store.
//
// each() snapshots the pool and calls out WITHOUT its lock, so a long reload never holds up another
// thread's acquire — and the per-agent lock is taken by withAgent, one agent at a time, which is what
// serializes an agent that happens to be mid-turn on another thread.
func (m *Manager) reloadAgentSkills() {
	var reloaded int
	m.agents.each(func(_ string, ca *cachedAgent) {
		if ca.withAgent(func(a *agent.AIAgent) { a.ReloadSkills() }) {
			reloaded++
		}
	})
	m.logger.Info(context.Background(), "channel: skill reload propagated", "count", reloaded)
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
