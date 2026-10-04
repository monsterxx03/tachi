package commands

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/skill"
)

// The skill list is what the MODEL reads to decide whether to activate a skill, and its source
// tag is the only thing that says where a skill came from — a machine's own skills, the project's,
// or the ones the operating system ships for it. Every source constant therefore has to render.
func TestFormatSkillListTagsEverySource(t *testing.T) {
	sources := map[string]string{
		skill.SourceProject: "🏠",
		skill.SourceClaude:  "🤖",
		skill.SourceCursor:  "🖱️",
		skill.SourceGlobal:  "🌐",
		skill.SourceOmarchy: "🐧",
	}

	for source, tag := range sources {
		metas := []skill.SkillMeta{{Name: "demo", Description: "does a thing", Source: source, Enabled: true}}
		got := FormatSkillList(metas)

		// The line that names the skill is the one to read: the list opens with a header.
		var skillLine string
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "**demo**") {
				skillLine = line
				break
			}
		}
		if skillLine == "" {
			t.Fatalf("source %q: no line names the skill: %q", source, got)
		}
		if !strings.Contains(skillLine, tag) {
			t.Errorf("source %q: %q is missing its tag %q", source, skillLine, tag)
		}
		if !strings.Contains(got, "/demo") {
			t.Errorf("source %q: the skill cannot be activated: %q", source, got)
		}
	}
}

// An empty list still has to say where a skill COULD come from.
func TestFormatSkillListOnAnEmptyStore(t *testing.T) {
	got := FormatSkillList(nil)
	if !strings.Contains(got, "No skills found") {
		t.Errorf("FormatSkillList(nil) = %q", got)
	}
}
