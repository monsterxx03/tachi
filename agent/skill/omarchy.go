package skill

import (
	"os"
	"path/filepath"
)

// The skills omarchy ships for whichever agent runs on it.
//
// omarchy's own agent tools (`omarchy agent`, `omarchy agent crash`) hand these to the CLI agent
// the user picked as its default, and their content is agent-agnostic on purpose — diagnose-crash
// says "if your harness has no skill mechanism, read the skill files directly". Tachi IS an agent
// on that machine, so it reads the same tree; without this the skills are simply missing, and a
// user who wants them has to symlink each one into ~/.tachi/skills by hand (which works, and is
// how this was done before, but it is a manual bridge that breaks on every new machine and on
// every skill omarchy adds).
//
// Scanned rather than symlinked into the user's store: nothing is written to their config, a
// removed skill leaves no dangling link behind, and a skill omarchy updates is picked up by the
// next scan instead of going stale. It is the LAST root (see NewStore), so a copy of the same
// name in the project or in ~/.tachi/skills wins — the user's own version is the more specific
// statement of what they want.
const omarchySkillFromEnv = "OMARCHY_PATH"

// omarchyDefaultPath is where omarchy lives when OMARCHY_PATH says nothing: an omarchy session
// exports it (default/hypr/envs.lua), but a Tachi started outside one may not have inherited it.
const omarchyDefaultPath = "/usr/share/omarchy"

// omarchyAgentSkillsDir returns omarchy's agent-skills directory, or "" when this machine has
// none (every non-omarchy machine, and omarchy before it shipped any).
func omarchyAgentSkillsDir() string {
	root := os.Getenv(omarchySkillFromEnv)
	if root == "" {
		root = omarchyDefaultPath
	}
	dir := filepath.Join(root, "default", "agents", "skills")

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return dir
}
