package main

// 通知的措辞与它的一条判据的纯函数那一半。
//
// 它没有构建标签，因为两个宿主都要用：窗口版把它交给操作系统的通知中心（notify.go），
// 无头版把它交给手机上的推送（push.go）。措辞只写一份，两块屏上的同一件事才不会各说一句。

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// Notification copy is fixed rather than configurable, and deliberately terse:
// one short line saying WHAT happened, never a quote of what the model said. A
// notification here is a "come back to the window" signal — the reply, the
// question text and every detail are one click away, and quoting them would turn
// a glanceable ping into a wall of text.
const (
	notifyAppTitle = "Tachi"
	notifyAskTitle = "Tachi 需要你的回答"
	// A parked session's own sentence, shared with the sidebar's marker (asks.go's
	// waitingQuestion / waitingPermission): the notification body and the row say the same
	// thing about the same wait, so they read it from one constant instead of keeping two
	// copies in step by hand. The notification composes around it — the body for a question
	// appends the count, the one for a command names the command.
	notifyAskWaiting  = waitingQuestion
	notifyPermTitle   = "Tachi 等待你的授权"
	notifyPermWaiting = waitingPermission
	// A finished turn and a failed one are two different sentences: in the second case what
	// the reader is being told is that the reply they were waiting for is NOT coming. (A stop
	// they asked for is neither — see notifyTurnFailed.)
	notifyTurnDone   = "回合完成"
	notifyTurnFailed = "回合出错"
	// A side-channel run is announced by WHAT it was, not as 回合完成: it never had a turn on
	// screen, so the name is the part the reader does not already know.
	notifyReviewName = "评审"
	notifyCommitName = "提交"
	notifyOneOffName = "旁路运行"
	// notifySubtitleMaxRune keeps a long session title from pushing the body out
	// of the visible area.
	notifySubtitleMaxRune = 40
	// notifyPermBodyMaxRune bounds the command shown as a permission request's body:
	// the head of the command is what identifies it, and an untruncated pipeline
	// would push the rest of the notification out of view.
	notifyPermBodyMaxRune = 80
	// notifyFailureReasonMaxRune bounds the error text carried by a failed turn's body. The
	// head of the chain is what names the failure ("local error: tls: bad record MAC"), and
	// an untruncated one — a provider's JSON blob, a stack of wrappers — would push the rest
	// of the notification out of view.
	notifyFailureReasonMaxRune = 80
)

// permBody is the one-line body for a parked ask. An unparsable command (empty)
// falls back to the bare waiting phrase rather than an empty line.
func permBody(command string) string {
	cmd := truncateRunes(strings.TrimSpace(command), notifyPermBodyMaxRune)
	if cmd == "" {
		return notifyPermWaiting
	}
	return notifyPermWaiting + "：" + cmd
}

// turnDoneBody is the one-line body for a finished turn.
func turnDoneBody(iterations int, took time.Duration) string {
	if iterations <= 0 {
		return notifyTurnDone
	}
	return fmt.Sprintf("%s · %d 次迭代 · %s", notifyTurnDone, iterations, took.Round(time.Millisecond))
}

// askBody is the one-line body for a parked AskUserQuestion. The count only
// appears when there is more than one — "（1 个问题）" is noise.
func askBody(questions int) string {
	if questions > 1 {
		return fmt.Sprintf("%s（%d 个问题）", notifyAskWaiting, questions)
	}
	return notifyAskWaiting
}

func cronDoneBody(jobName string, ok bool) string {
	name := truncateRunes(strings.TrimSpace(jobName), notifySubtitleMaxRune)
	if name == "" {
		name = "定时任务"
	}
	if ok {
		return "「" + name + "」已完成"
	}
	return "「" + name + "」没有跑成功"
}

// oneOffDoneBody is the one-line body for a finished run. `outcome` is the command lane's end
// reason: "complete", "interrupted" (the reader stopped it), or anything else — a failure.
func oneOffDoneBody(kind string, findings int, outcome string) string {
	name := oneOffRunName(kind)
	switch outcome {
	case "interrupted":
		return name + "已停止"
	case "complete":
		if kind == commandCommit {
			return name + "完成" // a commit has no findings to report
		}
		if findings == 0 {
			return name + "完成 · 未报问题"
		}
		return fmt.Sprintf("%s完成 · %d 条意见", name, findings)
	default:
		return name + "未完成（见日志）"
	}
}

// oneOffRunName is the reader-facing name of a run's kind. A review's rounds are recorded as
// review-round-N (see agent.OneOffRecorder), so this is a prefix test.
func oneOffRunName(kind string) string {
	switch {
	case strings.HasPrefix(kind, commandReview):
		return notifyReviewName
	case kind == commandCommit:
		return notifyCommitName
	default:
		return notifyOneOffName
	}
}

// turnFailedBody is the one-line body for a failed turn.
func turnFailedBody(err error) string {
	reason := failureReason(err)
	if reason == "" {
		return notifyTurnFailed
	}
	return notifyTurnFailed + " · " + reason
}

// failureReason is the INNERMOST error's message, made safe to put on one line.
//
// The innermost, because the wrapping layers only say which layer noticed while the end of
// the chain is the only part that says what actually went wrong: the loop's error arrives as
// 「API call failed: stream error (iteration 163): local error: tls: bad record MAC」, and of
// those three sentences only the last tells the reader anything the notification does not.
//
// OneLine rather than Truncate, for the reason that helper exists: an error message coming
// back from a provider is text from another machine, so it can hold newlines and escapes —
// and this text is going on a single notification line.
func failureReason(err error) string {
	if err == nil {
		return ""
	}
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return strutil.OneLine(err.Error(), notifyFailureReasonMaxRune)
		}
		err = inner
	}
}

// truncateRunes cuts s to at most max runes, appending an ellipsis when
// something was dropped. Rune-wise so a CJK session title is not cut
// mid-character.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
