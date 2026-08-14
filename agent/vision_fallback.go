package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// visionFallbackMaxTokens bounds the image-description output. Descriptions
// are consumed as context by the text-only model, not user-facing — 4096
// leaves generous headroom for dense images (screenshots, charts, tables)
// without letting a verbose dump eat the model's context window.
const visionFallbackMaxTokens = 4096

// imageDescriptionLabel prefixes every AI-generated image description so the
// text-only model can tell it apart from original message text.
const imageDescriptionLabel = "[Image description]\n"

// visionDescribeTemplate instructs the vision delegate to produce a factual,
// self-contained description usable by a model that cannot see images.
const visionDescribeTemplate = `You are an image description assistant. Describe the image below in detail so another AI model that cannot see images can fully understand it: include all visible text, objects, people, scenes, layout, colors, numbers and any other information. Be factual and precise. Respond in %s. Do not ask questions.`

// describeImagesIfNeeded converts image content parts in the run's message
// history into text descriptions when the current model cannot see images,
// and writes the descriptions back into rs.Messages in place.
//
// It runs on the loop goroutine right before the LLM call. Because the
// replacement is in-place, each image is described at most once per turn —
// later iterations find text parts instead of image parts and pass through
// untouched. The vision delegate is the first configured provider that
// supports images and is not the current provider; when none exists the
// image parts degrade to a text placeholder and the turn continues with the
// surrounding text (a warning is logged).
//
// Concurrency: rs.Messages is only ever written by the loop goroutine, so
// phase 1 (scan) needs no lock; the delegate API calls (phase 2, slow) run
// without any lock; the in-memory replacement pass (phase 3) takes rs.mu,
// matching the rs.append convention.
func (a *AIAgent) describeImagesIfNeeded(ctx context.Context, rs *RunState, opts *llm.ChatOptions) {
	// The current model sees images natively — no fallback needed.
	if a.Config.Resolved.SupportsVision {
		return
	}

	// Phase 1: locate messages carrying image parts (and their images, in
	// order). The loop goroutine is the sole writer, so this read is safe.
	type target struct {
		idx     int
		role    string
		msgText string
		images  []llm.ContentPart
	}
	var targets []target
	for i := range rs.Messages {
		msg := rs.Messages[i]
		var imgs []llm.ContentPart
		for _, p := range msg.ContentParts {
			if p.Type == llm.ContentPartImage {
				imgs = append(imgs, p)
			}
		}
		if len(imgs) > 0 {
			targets = append(targets, target{idx: i, role: msg.Role, msgText: msg.Content, images: imgs})
		}
	}
	if len(targets) == 0 {
		return
	}

	delegate := a.visionDelegateProvider()
	cache := make(map[string]string, 4) // image content hash → description (dedup within the turn)
	descs := make(map[int][]string, len(targets))
	described := 0

	// Phase 2: describe each image through the vision delegate. Slow API
	// calls run outside any lock; the per-image cache makes duplicate images
	// (e.g. a user attachment re-read by the ReadFile tool) cost one call.
	for _, t := range targets {
		ds := make([]string, 0, len(t.images))
		for _, img := range t.images {
			ds = append(ds, a.describeOneImage(ctx, delegate, img, t.msgText, cache, opts))
			described++
		}
		descs[t.idx] = ds
	}

	// Phase 3: apply the replacements under rs.mu (fast in-memory pass).
	rs.mu.Lock()
	for _, t := range targets {
		msg := &rs.Messages[t.idx]
		if t.role == "tool" {
			// Tool-role messages only carry string content on every wire
			// protocol (openai / openai-res / anthropic): merge the
			// descriptions into Content and drop the parts.
			var sb strings.Builder
			sb.WriteString(msg.Content)
			for _, d := range descs[t.idx] {
				sb.WriteString("\n\n")
				sb.WriteString(imageDescriptionLabel)
				sb.WriteString(d)
			}
			msg.Content = sb.String()
			msg.ContentParts = nil
		} else {
			// user / steer / assistant: replace image parts with text
			// description parts, keeping all other parts in place.
			newParts := make([]llm.ContentPart, 0, len(msg.ContentParts))
			di := 0
			for _, p := range msg.ContentParts {
				if p.Type == llm.ContentPartImage {
					newParts = append(newParts, llm.ContentPart{
						Type: llm.ContentPartText,
						Text: imageDescriptionLabel + descs[t.idx][di],
					})
					di++
				} else {
					newParts = append(newParts, p)
				}
			}
			msg.ContentParts = newParts
		}
	}
	rs.mu.Unlock()

	a.Config.Logger.Info(ctx, "Vision fallback: images described for text-only model",
		"provider", a.Config.Resolved.Name,
		"model", a.Config.Resolved.Model,
		"delegate", visionDelegateName(delegate),
		"images", described)
}

// describeOneImage sends a single image to the vision delegate and returns
// its description. Cache misses are described; hits reuse the earlier text.
// When no delegate is available, or the description call fails, a short
// placeholder is returned so the turn can still proceed with the surrounding
// text (the image parts themselves would make the API call fail on a
// text-only model).
func (a *AIAgent) describeOneImage(ctx context.Context, delegate llm.Provider, part llm.ContentPart, msgText string, cache map[string]string, opts *llm.ChatOptions) string {
	key := imageContentKey(part)
	if cached, ok := cache[key]; ok {
		return cached
	}

	if delegate == nil {
		desc := "（无法描述：未配置支持图片的 provider）"
		cache[key] = desc
		return desc
	}

	thinkingOff := false
	descCtx := llm.WithUsageKind(ctx, llm.UsageKindVision) // bill the fallback call under its own ledger bucket
	var sessionID string
	if opts != nil {
		sessionID = opts.SessionID
	}
	resp, err := delegate.CreateChat(descCtx, []llm.Message{{
		Role: "user",
		ContentParts: []llm.ContentPart{
			{Type: llm.ContentPartText, Text: a.visionDescribePrompt(msgText)},
			part,
		},
	}}, nil, llm.ChatOptions{
		MaxTokens: visionFallbackMaxTokens,
		Thinking:  &thinkingOff,
		SessionID: sessionID,
	})
	if err != nil {
		a.Config.Logger.Warn(ctx, "Vision fallback: image description failed, using placeholder",
			"err", err, "delegate", visionDelegateName(delegate))
		desc := "（图片描述失败）"
		cache[key] = desc
		return desc
	}

	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		desc = "（图片描述为空）"
	}
	cache[key] = desc
	return desc
}

// visionDescribePrompt builds the description request for the vision
// delegate. The message's own text (the user's question or tool context) is
// attached so the describer focuses on what the text-only model needs.
func (a *AIAgent) visionDescribePrompt(msgText string) string {
	prompt := fmt.Sprintf(visionDescribeTemplate, a.replyLanguage())
	if text := strings.TrimSpace(msgText); text != "" {
		prompt += "\n\n消息上下文:\n" + strutil.Truncate(text, 1000)
	}
	return prompt
}

// replyLanguage returns the language the agent replies in (used for the
// description prompt so descriptions match the conversation). Falls back to
// the config default ("English") when unavailable.
func (a *AIAgent) replyLanguage() string {
	if a.Config.FullConfig != nil && a.Config.FullConfig.Language != "" {
		return a.Config.FullConfig.Language
	}
	return "English"
}

// visionDelegateProvider returns the lazily-built image-description
// provider, or nil when no configured provider supports images. The result
// (including a negative one) is cached for the agent's lifetime — the
// provider list does not change at runtime.
func (a *AIAgent) visionDelegateProvider() llm.Provider {
	if a.visionDelegateOverride != nil {
		return a.visionDelegateOverride
	}
	a.visionDelegateOnce.Do(func() {
		a.visionDelegate, a.visionDelegateErr = a.buildVisionDelegate()
	})
	if a.visionDelegateErr != nil {
		a.Config.Logger.Warn(context.Background(), "Vision fallback: no vision-capable provider available", "err", a.visionDelegateErr)
		return nil
	}
	return a.visionDelegate
}

// buildVisionDelegate picks the first configured provider (config order)
// that supports images and is not the current provider, and wraps it for
// usage billing so description calls land in the ledger under the vision
// bucket. Returns an error when no candidate exists.
func (a *AIAgent) buildVisionDelegate() (llm.Provider, error) {
	full := a.Config.FullConfig
	if full == nil {
		return nil, fmt.Errorf("no FullConfig to resolve a vision provider from")
	}
	current := a.Config.Resolved.Name
	for i := range full.Providers {
		pCfg := &full.Providers[i]
		if pCfg.Name == current {
			continue // the current model is text-only; it cannot describe images
		}
		if !llm.ProviderConfigSupportsVision(pCfg) {
			continue
		}
		rp, err := llm.BuildProvider(full, pCfg.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve vision provider %q: %w", pCfg.Name, err)
		}
		return wrapForUsage(rp.Provider, a.usageRecorder(), full), nil
	}
	return nil, fmt.Errorf("no configured provider supports images")
}

// imageContentKey derives a content-addressed cache key for an image part.
// The same image embedded twice (user attachment + ReadFile) is described
// only once per turn.
func imageContentKey(part llm.ContentPart) string {
	h := sha256.Sum256([]byte(part.MediaType + "|" + part.Data))
	return hex.EncodeToString(h[:])
}

// visionDelegateName returns a display name for the delegate provider.
func visionDelegateName(p llm.Provider) string {
	if p == nil {
		return ""
	}
	if name := p.ProviderName(); name != "" {
		return name
	}
	return p.Model()
}
