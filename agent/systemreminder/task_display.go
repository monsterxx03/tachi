package systemreminder

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// TaskDisplayText is the one-line summary a conversation shows for a wake turn
// (the model sees the full block; this is what the transcript bubble says). It
// lives here so every entry point words it the same way.
func TaskDisplayText(events []tasks.Event) string {
	if len(events) == 1 {
		return "🔔 " + taskDisplaySummary(events[0])
	}
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Name)
	}
	return fmt.Sprintf("🔔 %d 个任务完成：%s", len(events), strutil.Truncate(strings.Join(names, "、"), 80))
}

func taskDisplaySummary(e tasks.Event) string {
	name := e.Name
	if name == "" {
		name = strutil.Truncate(e.Command, 40)
	}
	subject := name
	switch e.Kind {
	case tasks.KindSubagent:
		// The name is an opaque id; "子任务" is what makes the line readable.
		subject = "子任务 " + name
	case tasks.KindRemote:
		subject = "远程任务 " + name
	}
	switch {
	case e.Status == tasks.StatusWaiting:
		return subject + " 等待回答"
	case e.Status == tasks.StatusError:
		return subject + " 出错"
	case e.ExitCode == 0:
		return subject + " 完成"
	default:
		return fmt.Sprintf("%s 退出码 %d", subject, e.ExitCode)
	}
}
