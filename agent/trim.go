package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// Trimming tool results is the SECOND way to free context, and it is the opposite of
// compaction in every respect that matters to the reader: compaction summarises the
// conversation into a new session and costs an LLM call, while a trim rewrites the
// same session and costs nothing but the cached prefix it invalidates.
//
// It is worth doing because the bulk of a long conversation is not the conversation:
// in a 4MB session measured on this machine, ~45% was tool output and ~48% thinking
// blocks — text the model has already read and (with a tool it can run again) can
// always get back. A trim removes the oldest outputs, keeps everything a reader said
// and everything the model concluded, and leaves the files, the checkpoints and the
// session identity untouched.
//
// What it is NOT: an automatic step. It changes the request prefix at the first
// trimmed result, so the provider re-reads the rest of the history once — a cost paid
// deliberately, when the reader asks for room (docs/agents/prompt-cache.md).
const (
	// trimKeepRecentResults is how many of the newest tool results are left alone.
	// The model is generally still working with those; the ones behind them have
	// already been used to reach a conclusion that is in the transcript.
	trimKeepRecentResults = 8

	// trimMinResultChars is the size below which a result is not worth touching: the
	// placeholder itself has a cost, and the saving would be noise.
	trimMinResultChars = 2000

	// trimImageBudgetBytes is how many image bytes the conversation may keep. Images
	// are counted by BYTES and not by result count because they are the one kind of
	// output whose size has nothing to do with how much text it is: a single
	// screenshot outweighs a hundred prose results, and a request body is refused
	// (413) long before the token window is full. Past this budget older images go
	// even when they sit inside the recent-results window above — a picture the model
	// already read and described is exactly what the placeholder is for.
	trimImageBudgetBytes = 4 << 20
)

// trimKeepTools are the results a trim never touches, whatever their size.
//
// AskUserQuestion is here because its "output" is a person's DECISION, not data: the
// choice they made, and the model has no way to ask again for it — losing it does not
// cost a re-read, it costs the conversation's premises. The three task tools are here
// for the weaker version of the same argument: what they return is a distilled answer
// somebody else already paid for.
var trimKeepTools = map[string]bool{
	tools.ToolNameAskUser:      true,
	tools.ToolNameSubAgent:     true,
	tools.ToolNameDelegateTask: true,
	tools.ToolNameRemoteTask:   true,
}

// sessionToolResultTrimmer is the optional half of a SessionManager: the one that can
// rewrite recorded tool results. *session.Manager has it; a test double that keeps no
// records can answer for neither, so it is asked as an interface rather than added to
// SessionManager (the same shape sessionSwitchNotifier has in compact.go).
type sessionToolResultTrimmer interface {
	TrimToolResults(replacements map[string]string, tag string) (session.TrimResult, error)
}

// TrimReport is what a trim did, and the history the session has afterwards.
//
// History is nil when nothing was trimmed — an unchanged conversation needs no
// adoption — and Blocked names a session this operation refuses (a managed one) the
// same way RewindPreview does: a refusal is an answer, not an error.
type TrimReport struct {
	History []llm.Message `json:"-"`
	Trimmed int           `json:"trimmed"`
	// CharsSaved is text taken out of the records. Images are reported separately:
	// measured in characters they look like nothing (a 4.7MB screenshot's note is one
	// short line), which is precisely how a trim could claim success while leaving a
	// request the provider refuses.
	CharsSaved int64  `json:"charsSaved"`
	BytesSaved int64  `json:"bytesSaved"`
	Sidecar    string `json:"sidecar,omitempty"`
	Blocked    string `json:"blocked,omitempty"`
}

// TrimToolResults frees context in the CURRENT session by replacing old tool outputs
// with a one-line note. No LLM is involved, and no session is created.
//
// The decision is made from the session's own records rather than from a history the
// caller passes in: what the next request is rebuilt from is the record, so that is
// what has to change. The rebuilt history is returned so a frontend adopts exactly
// what a reload would produce (the same rule RewindResult.History follows).
func (a *AIAgent) TrimToolResults(ctx context.Context) (TrimReport, error) {
	if err := a.managedSessionWriteRefusal(); err != nil {
		return TrimReport{Blocked: err.Error()}, nil
	}
	sm := a.Config.SessionManager
	if sm == nil {
		return TrimReport{}, errors.New("no session manager")
	}
	if sm.Current() == nil {
		return TrimReport{}, errors.New("没有活跃的会话")
	}
	msgs, err := sm.LoadMessages()
	if err != nil {
		return TrimReport{}, fmt.Errorf("读取会话记录失败: %w", err)
	}

	plan := planToolResultTrim(msgs, imageSizeLookup(sm))
	if len(plan.replacements) == 0 {
		return TrimReport{}, nil
	}

	trimmer, ok := sm.(sessionToolResultTrimmer)
	if !ok {
		return TrimReport{}, errors.New("this session manager cannot trim tool results")
	}
	res, err := trimmer.TrimToolResults(plan.replacements, trimTag())
	if err != nil {
		return TrimReport{}, fmt.Errorf("裁剪会话记录失败: %w", err)
	}

	history, err := a.LoadSessionHistory()
	if err != nil {
		return TrimReport{Trimmed: res.Trimmed, Sidecar: res.Sidecar},
			fmt.Errorf("裁剪已完成，但重建历史失败: %w", err)
	}
	// The estimate described the conversation with the outputs still in it, and the
	// anchor described a real prompt that carried them: both are now wrong in the
	// direction that matters (the ring would show a conversation that no longer
	// exists). Recompute, and drop the anchor — the same reason a rewind does.
	a.reestimateAfterHistoryRewrite(ctx, history)

	a.Config.Logger.Info(ctx, "Agent: trimmed tool results",
		"trimmed", res.Trimmed, "charsSaved", plan.charsSaved,
		"imageBytesSaved", plan.bytesSaved, "sidecar", res.Sidecar)
	return TrimReport{
		History:    history,
		Trimmed:    res.Trimmed,
		CharsSaved: plan.charsSaved,
		BytesSaved: plan.bytesSaved,
		Sidecar:    res.Sidecar,
	}, nil
}

// imageSizeLookup sizes the blobs a session's records point at, once each. A missing
// blob reports 0: it cannot be sent, so it cannot be worth trimming either.
func imageSizeLookup(sm SessionManager) func(session.Part) int64 {
	sizes := map[string]int64{}
	return func(p session.Part) int64 {
		if p.Ref == "" {
			return 0
		}
		if size, ok := sizes[p.Ref]; ok {
			return size
		}
		var size int64
		if raw, err := sm.LoadImage(p.Ref); err == nil {
			size = int64(len(raw))
		}
		sizes[p.Ref] = size
		return size
	}
}

// trimTag names the sidecar a trim's original is kept under, to the second. The
// records are the audit trail; the tag only has to keep two trims of one session
// from overwriting each other.
func trimTag() string { return time.Now().Format("20060102-150405") }

// trimPlan is the set of rewrites and what they are worth.
type trimPlan struct {
	replacements map[string]string
	charsSaved   int64
	bytesSaved   int64 // images carried by the trimmed results, in bytes
}

// planToolResultTrim decides which recorded tool results to replace.
//
// It walks BACKWARDS so "the newest N results" is a position in the conversation, not
// a count of whatever happened to be eligible: a small result counts towards the
// window, otherwise a run of small reads would leave results from hours ago in place
// merely because nothing big came after them.
//
// imageBytes sizes one image part. It is a function rather than a field on the record
// because the bytes live in the session's blob directory, which only the store knows
// how to read — and it is only asked about the images a trim might touch, not about
// every image in the conversation.
func planToolResultTrim(msgs []session.Message, imageBytes func(session.Part) int64) trimPlan {
	plan := trimPlan{replacements: map[string]string{}}
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Type != session.MessageTypeToolResult || msg.AutoExpand {
			continue
		}
		seen++
		if seen <= trimKeepRecentResults || !trimmableResult(msg) {
			continue
		}
		plan.trimResult(msg, imageBytes)
	}
	// Images are then trimmed on their own budget, which can reach INTO the recent
	// window: a session whose last three calls were screenshots has nothing older
	// worth removing and is still too big to send.
	plan.trimImagesOverBudget(msgs, imageBytes)
	return plan
}

// trimResult records the rewrite of one result, and what it frees: its text in
// characters, and any image it carried in bytes.
func (p *trimPlan) trimResult(msg session.Message, imageBytes func(session.Part) int64) {
	images, imageBytesTotal := imageStats(msg.Parts, imageBytes)
	placeholder := trimPlaceholder(msg.Name, utf8.RuneCountInString(msg.Result), images)
	p.replacements[msg.ToolCallID] = placeholder
	p.charsSaved += int64(utf8.RuneCountInString(msg.Result)) -
		int64(utf8.RuneCountInString(placeholder))
	p.bytesSaved += imageBytesTotal
}

// trimImagesOverBudget keeps the newest image and as many of the ones behind it as the
// byte budget allows, whatever the recent-results window says.
func (p *trimPlan) trimImagesOverBudget(msgs []session.Message, imageBytes func(session.Part) int64) {
	kept, total := 0, int64(0)
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Type != session.MessageTypeToolResult || msg.AutoExpand {
			continue
		}
		images, bytes := imageStats(msg.Parts, imageBytes)
		if images == 0 {
			continue
		}
		if _, already := p.replacements[msg.ToolCallID]; already {
			continue // the text rule took it, and its image with it
		}
		kept++
		// The newest image always stays: it is the one the model may still be talking
		// about, and a history whose last picture vanished would be worse than a 413
		// (the size is handled by fitting the image, not by deleting it).
		if kept == 1 || total+bytes <= trimImageBudgetBytes {
			total += bytes
			continue
		}
		p.trimResult(msg, imageBytes)
	}
}

// imageStats counts the image parts of one result and their total bytes.
//
// Text parts are not counted: they are already part of the rune count, and calling
// them images (as a len(msg.Parts) would) made a trimmed MCP result claim pictures it
// never had.
func imageStats(parts []session.Part, imageBytes func(session.Part) int64) (int, int64) {
	count, total := 0, int64(0)
	for _, part := range parts {
		if part.Type != session.PartTypeImage {
			continue
		}
		count++
		total += imageBytes(part)
	}
	return count, total
}

// trimmableResult reports whether one result is a candidate at all.
//
// AutoExpand records are excluded by the caller: they are the user's own /sh output,
// kept out of the model's history entirely (session.Message.AutoExpand), so rewriting
// them would damage the transcript and save nothing.
func trimmableResult(msg session.Message) bool {
	if msg.ToolCallID == "" || trimKeepTools[msg.Name] {
		return false
	}
	// An image is worth removing at any text length: the tokens are in the parts.
	if len(msg.Parts) > 0 {
		return true
	}
	return utf8.RuneCountInString(msg.Result) >= trimMinResultChars
}

// trimPlaceholder is what a trimmed result becomes, in the history AND on disk.
//
// It says which tool, how much was removed, and — the part that keeps the model
// useful — that running the tool again gets it back. Without that last sentence the
// note reads like a tool that failed.
func trimPlaceholder(name string, chars, images int) string {
	if images > 0 {
		return fmt.Sprintf("[输出已裁剪] %s 的原始输出（含 %d 张图片）为节省上下文已被移除；需要时重新执行这个工具即可。",
			name, images)
	}
	return fmt.Sprintf("[输出已裁剪] %s 的原始输出共 %d 字符，为节省上下文已被移除；需要时重新执行这个工具即可。",
		name, chars)
}
