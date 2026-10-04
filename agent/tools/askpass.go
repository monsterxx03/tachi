package tools

// How a command that needs a password gets one.
//
// Every command the bash tool runs is started with Setsid (see the comment at its exec), which
// is what keeps an interactive prompt from hanging the TUI — and what leaves `sudo` with no way
// to ask: it wants /dev/tty, and there is none. sudo's own answer for exactly that case is
// SUDO_ASKPASS — `man sudo`: "used to read the password if no terminal is available or if the
// -A option is specified" — so this file installs a helper and points the child at it.
//
// Nothing about the password passes through Tachi: the helper is a dialog that prints to sudo's
// stdout, so the text never enters this process, the model's context, or any file. That is also
// why the helper is a shell script rather than a Tachi subcommand — SUDO_ASKPASS takes a path
// and no arguments, and a script is something a user can read and debug.
//
// Installed on use rather than at startup: the file only matters when a command runs, and a
// session that never touches sudo should not have Tachi writing executables anywhere.
//
// An ACP session is the exception this cannot reach: its commands run in the CLIENT's terminal
// (BashTool.SetACPMode), so the client's environment decides there — and that terminal usually
// has a real TTY, which is a prompt sudo raises by itself.

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// sudoAskpassScript is the master copy of the helper (agent/tools/sudo-askpass.sh); the
// installed copy under TACHI_HOME/bin is replaced whenever it differs, which is also how an
// upgraded Tachi lands a fixed helper.
//
//go:embed sudo-askpass.sh
var sudoAskpassScript string

// sudoAskpassName is the installed file's name. It carries no extension: SUDO_ASKPASS is
// executed as-is, and a name a reader can recognise in `ps` is worth more than one that hints
// at its language.
const sudoAskpassName = "sudo-askpass"

// askpassWarned keeps a permanently unwritable TACHI_HOME from logging once per command.
var askpassWarned atomic.Bool

// bashEnv is the environment for one command: the process's own, plus the askpass helper when
// nothing else has already named one.
//
// An existing SUDO_ASKPASS wins — the session may have set one (a dotfile, a desktop-wide
// helper) and a deliberate choice should not be overridden by a default. Empty counts as unset,
// which is what an empty SUDO_ASKPASS means to sudo anyway.
func bashEnv() []string {
	env := os.Environ()
	if os.Getenv("SUDO_ASKPASS") != "" {
		return env
	}
	path := askpassHelper()
	if path == "" {
		return env
	}

	// REPLACE any entry rather than appending a second one. An ambient SUDO_ASKPASS= (empty —
	// what "unset" looks like to sudo, and what a wrapper may leave behind) would otherwise sit
	// in the child next to ours, and the two are read differently: glibc's getenv, which is what
	// sudo itself uses, takes the FIRST match — so the empty one could win and no dialog would
	// ever appear. Same class of trap the smoke sandbox documents for HOME.
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "SUDO_ASKPASS=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "SUDO_ASKPASS="+path)
}

// askpassHelper installs the helper if it is missing or out of date and returns its path, or
// "" when it cannot be installed (a read-only home, say) — in which case sudo behaves exactly
// as it did before any of this existed.
//
// Read-and-compare on every call rather than a once-per-process flag: the check is one small
// read against the cost of forking bash, it keeps an upgraded helper from being pinned for the
// life of a long-running session, and it leaves no hidden state for a test to trip over.
func askpassHelper() string {
	dir := filepath.Join(config.BaseDir(), "bin")
	path := filepath.Join(dir, sudoAskpassName)

	if current, err := os.ReadFile(path); err == nil && string(current) == sudoAskpassScript {
		return path
	}
	if err := fileutil.AtomicWriteFile(path, []byte(sudoAskpassScript), 0o755, 0o755); err != nil {
		if !askpassWarned.Swap(true) {
			log.Printf("bash: cannot install the askpass helper at %s (commands needing a password will fail): %v", path, err)
		}
		return ""
	}
	return path
}
