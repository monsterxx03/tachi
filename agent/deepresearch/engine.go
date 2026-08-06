// Package deepresearch provides a config-driven deep research engine.
//
// The engine orchestrates a recursive search tree where each level:
//  1. Generates 'breadth' search queries via a direct LLM call
//  2. Launches parallel SubAgents to search + extract learnings
//  3. If depth > 0 and new learnings exist, recurses with depth-1 and breadth/2
//  4. Writes a final report via SubAgent or direct LLM call
//
// DeepResearch is NOT a tool — it is only accessible via the /research
// slash command. The engine is a plain Go struct, decoupled from the
// agent's tool registry, that handlers use directly.
package deepresearch

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// ---- Public types ----

// SubagentRunner executes sub-agent tasks for research.
// The engine delegates all search+extraction work to sub-agents.
type SubagentRunner interface {
	Run(ctx context.Context, prompt string, allowedTools []string) (string, error)
}

// ProgressFunc is an optional callback for reporting research progress.
// The callback may be called from multiple goroutines concurrently;
// the caller is responsible for thread-safety (e.g. use a buffered channel
// or mutex-protected writer).
// When nil, no progress is reported.
type ProgressFunc func(format string, args ...any)

// DeepResearch is the config-driven deep research engine.
// It does NOT implement the Tool interface — it is used by slash command
// handlers across TUI, Channel, and ACP modes.
type DeepResearch struct {
	cfg             *config.DeepResearchConfig
	providersCfg    []config.ProviderConfig
	defaultProvider llm.Provider
	runner          SubagentRunner
	logger          *logger.Logger
	// reportMaxTokens caps the direct LLM call that generates the report
	// HTML. Follows the top-level config max_tokens (never hardcoded).
	reportMaxTokens int
	// lastReportPath is the output path of the most recent Run(), set so
	// callers can register the artifact with the session (see ReportPath).
	lastReportPath string

	// providerCache caches named providers to avoid re-creating them on each
	// call to getProvider. The default provider is NOT cached here since it
	// is owned by the caller.
	providerCache map[string]llm.Provider
}

// New creates a DeepResearch engine.
//
//   - cfg: DeepResearch configuration (from config.yaml deep_research section)
//   - providersCfg: the full list of provider configs, used to resolve
//     provider references by name (e.g. query_generator_provider)
//   - defaultProvider: fallback LLM provider when a named provider is not found
//   - runner: used to execute sub-agent research tasks
//   - logger: optional logger for debug output
//   - reportMaxTokens: MaxTokens budget for the report-generation LLM call,
//     taken from the top-level config max_tokens; <= 0 falls back to the
//     package default.
func New(
	cfg *config.DeepResearchConfig,
	providersCfg []config.ProviderConfig,
	defaultProvider llm.Provider,
	runner SubagentRunner,
	logger *logger.Logger,
	reportMaxTokens int,
) *DeepResearch {
	if reportMaxTokens <= 0 {
		reportMaxTokens = config.DefaultMaxTokens
	}
	return &DeepResearch{
		cfg:             cfg,
		providersCfg:    providersCfg,
		defaultProvider: defaultProvider,
		runner:          runner,
		logger:          logger,
		reportMaxTokens: reportMaxTokens,
		providerCache:   make(map[string]llm.Provider),
	}
}

// progress is a helper to call the progress callback if non-nil.
// Thread-safe by design — the callback itself must be thread-safe.
func (dr *DeepResearch) progress(pfn ProgressFunc, format string, args ...any) {
	if pfn != nil {
		pfn(format, args...)
	}
}

// Run executes deep research on the given topic and returns the report.
// The report is saved as an HTML file in ~/.tachi/research/ and the returned
// string includes the file path.
//
// progress is an optional callback for reporting intermediate progress.
// It may be called from multiple goroutines; the caller must ensure
// thread-safety. Pass nil to disable progress reporting.
func (dr *DeepResearch) Run(ctx context.Context, topic string, depth, breadth int, progress ProgressFunc) (string, error) {
	// Clamp to configured limits
	if depth > dr.cfg.MaxDepth {
		depth = dr.cfg.MaxDepth
	}
	if breadth > dr.cfg.MaxBreadth {
		breadth = dr.cfg.MaxBreadth
	}
	if breadth < 1 {
		breadth = 1
	}

	// Apply research timeout — report writing gets its own timeout below
	researchCtx, researchCancel := context.WithTimeout(ctx, dr.cfg.Timeout)
	defer researchCancel()

	dr.log("DeepResearch: starting topic=%q depth=%d breadth=%d", topic, depth, breadth)
	dr.progress(progress, "🔬 **研究启动**: 主题「%s」| 深度 %d | 广度 %d", topic, depth, breadth)

	// Pre-compute output path once so success and error paths share the same filename.
	outputPath := dr.reportPath(topic)
	dr.lastReportPath = outputPath

	allLearnings, allURLs, err := dr.deepResearch(researchCtx, topic, depth, breadth, nil, progress)
	if err != nil {
		if researchCtx.Err() != nil {
			dr.log("DeepResearch: timed out or cancelled after %v, generating partial report", dr.cfg.Timeout)
			dr.progress(progress, "⚠️ **研究超时或被中断**，正在基于已有发现生成部分报告...")
			report := dr.buildPartialReport(topic, allLearnings, allURLs, researchCtx.Err())
			dr.saveReport(outputPath, report)
			return report, nil
		}
		return "", err
	}

	dr.log("DeepResearch: research complete, writing report (learnings=%d, urls=%d)", len(allLearnings), len(allURLs))
	dr.progress(progress, "📄 **正在生成研究报告**（%d 条发现, %d 个来源）...", len(allLearnings), len(allURLs))

	// Report writing uses its own timeout, independent of the research phase.
	reportCtx := ctx
	if dr.cfg.ReportTimeout > 0 {
		var reportCancel context.CancelFunc
		reportCtx, reportCancel = context.WithTimeout(ctx, dr.cfg.ReportTimeout)
		defer reportCancel()
	}

	// The sub-agent writes the HTML file via WriteFile to outputPath.
	report, err := dr.writeReport(reportCtx, topic, allLearnings, allURLs, outputPath)
	if err != nil {
		dr.log("DeepResearch: report writing failed: %v, returning partial results", err)
		dr.progress(progress, "⚠️ 报告生成失败: %v，返回部分结果", err)
		report = dr.buildPartialReport(topic, allLearnings, allURLs, nil)
		dr.saveReport(outputPath, report)
		return report, nil
	}

	dr.progress(progress, "✅ **研究报告已保存**: `%s`", outputPath)
	return report, nil
}

// ---- Private types ----

// searchQuery is a single search query with its research goal, generated by the LLM.
type searchQuery struct {
	Query        string `json:"query"`
	ResearchGoal string `json:"researchGoal"`
}

// ---- Private methods ----

// deepResearch is the recursive research algorithm.
// It generates queries, launches parallel sub-agents, collects results,
// and optionally recurses with halved breadth.
//
// contextLearnings contains learnings from previous recursion levels,
// used as context for generating the next level's search queries.
// Returns the combined learnings and URLs from this level and all
// deeper levels.
//
// progress is forwarded from Run() for reporting intermediate progress.
func (dr *DeepResearch) deepResearch(
	ctx context.Context,
	query string,
	depth, breadth int,
	contextLearnings []string,
	progress ProgressFunc,
) (learnings []string, urls []string, err error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	dr.progress(progress, "🔍 **正在生成 %d 个搜索查询**...", breadth)

	// 1. Generate search queries via direct LLM call
	dr.log("DeepResearch: generating %d queries for depth=%d", breadth, depth)
	queries, err := dr.generateQueries(ctx, query, contextLearnings, breadth)
	if err != nil {
		return nil, nil, fmt.Errorf("generate queries: %w", err)
	}
	if len(queries) == 0 {
		dr.log("DeepResearch: no queries generated, stopping recursion")
		dr.progress(progress, "⚠️ 未生成搜索查询，当前分支研究终止")
		return nil, nil, nil
	}

	dr.progress(progress, "📝 **已生成 %d 个搜索查询**，正在并行搜索...", len(queries))

	// 2. Launch parallel sub-agents for each query
	type agentResult struct {
		learnings []string
		urls      []string
		err       error
	}

	results := make([]agentResult, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup

	researcherTools := dr.cfg.ResearcherTools()

	for i, sq := range queries {
		wg.Add(1)
		go func(idx int, sq searchQuery) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				mu.Lock()
				results[idx].err = ctx.Err()
				mu.Unlock()
				return
			default:
			}

			prompt := dr.buildResearcherPrompt(sq.Query, sq.ResearchGoal)

			dr.log("DeepResearch: launching sub-agent %d/%d: query=%q", idx+1, len(queries), sq.Query)
			dr.progress(progress, "🔎 **研究员 %d/%d**: 「%s」", idx+1, len(queries), sq.Query)

			output, runErr := dr.runner.Run(ctx, prompt, researcherTools)
			if runErr != nil {
				mu.Lock()
				results[idx].err = runErr
				mu.Unlock()
				dr.log("DeepResearch: sub-agent %d failed: %v", idx+1, runErr)
				dr.progress(progress, "❌ **研究员 %d/%d** 失败: %v", idx+1, len(queries), runErr)
				return
			}

			lrnd, urls := extractLearningsAndURLs(output)
			mu.Lock()
			results[idx].learnings = lrnd
			results[idx].urls = urls
			mu.Unlock()
			dr.log("DeepResearch: sub-agent %d complete: %d learnings, %d urls", idx+1, len(lrnd), len(urls))
			dr.progress(progress, "✅ **研究员 %d/%d** 完成: %d 条发现, %d 个来源", idx+1, len(queries), len(lrnd), len(urls))
		}(i, sq)
	}
	wg.Wait()

	// 3. Collect results
	var allLearnings []string
	var allURLs []string
	hasNewLearnings := false
	for _, r := range results {
		if r.err != nil {
			continue
		}
		if len(r.learnings) > 0 {
			hasNewLearnings = true
		}
		allLearnings = append(allLearnings, r.learnings...)
		allURLs = append(allURLs, r.urls...)
	}

	dr.progress(progress, "📊 **本轮完成**: 共 %d 条发现, %d 个来源", len(allLearnings), len(allURLs))

	// Check max learnings limit
	if len(allLearnings) >= dr.cfg.MaxLearnings {
		allLearnings = allLearnings[:dr.cfg.MaxLearnings]
		dr.log("DeepResearch: reached max learnings (%d), stopping", dr.cfg.MaxLearnings)
		dr.progress(progress, "⏹️ 已达最大发现数上限（%d），停止搜索", dr.cfg.MaxLearnings)
		return allLearnings, allURLs, nil
	}

	// 4. Recurse if depth > 0 and we have new learnings
	if depth > 0 && hasNewLearnings {
		nextBreadth := max(breadth/2, 1)
		dr.log("DeepResearch: recursing with depth=%d breadth=%d", depth-1, nextBreadth)
		dr.progress(progress, "⏬ **深入下一层**（剩余深度 %d, 广度 %d）...", depth-1, nextBreadth)

		// Pass accumulated learnings as context for next-level query generation
		nextLearnings, nextURLs, recurseErr := dr.deepResearch(ctx, query, depth-1, nextBreadth, allLearnings, progress)
		if recurseErr != nil {
			return allLearnings, allURLs, recurseErr
		}

		// Merge deeper-level results
		allLearnings = append(allLearnings, nextLearnings...)
		allURLs = append(allURLs, nextURLs...)
		if len(allLearnings) > dr.cfg.MaxLearnings {
			allLearnings = allLearnings[:dr.cfg.MaxLearnings]
		}
	}

	if depth > 0 && !hasNewLearnings {
		dr.log("DeepResearch: no new learnings found, stopping recursion")
		dr.progress(progress, "⏹️ 未发现新信息，停止深入")
	}

	return allLearnings, allURLs, nil
}

// generateQueries calls the LLM to generate search queries for the current
// research level. The LLM response must be a JSON array of searchQuery objects.
func (dr *DeepResearch) generateQueries(
	ctx context.Context,
	query string,
	learnings []string,
	num int,
) ([]searchQuery, error) {
	provider, err := dr.getProvider(dr.cfg.QueryGeneratorProvider)
	if err != nil {
		return nil, fmt.Errorf("get provider for query generation: %w", err)
	}

	// Build prompt from template
	promptTmpl := dr.cfg.QueryGeneratorPrompt()

	learningsText := ""
	if len(learnings) > 0 {
		learningsText = strings.Join(learnings, "\n- ")
		if learningsText != "" {
			learningsText = "- " + learningsText
		}
	}

	now := time.Now()
	timeContext := fmt.Sprintf("\nCurrent date and time: %s (%s, %s)",
		now.Format(strutil.TimeFormatDateTimeShort),
		now.Weekday().String(),
		now.Location().String(),
	)

	systemPrompt := strings.NewReplacer(
		"{breadth}", fmt.Sprintf("%d", num),
		"{query}", query,
		"{learnings}", learningsText,
		"{language}", dr.cfg.Language(),
	).Replace(promptTmpl) + timeContext

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: fmt.Sprintf("Generate %d search queries for: %s", num, query)},
	}

	disabled := false
	// Usage billing: tag the call kind; session anchoring comes from the
	// caller's ctx (llm.WithSessionID) when a session is active, otherwise
	// the row falls to the global bucket (acceptable, documented).
	ctx = llm.WithUsageKind(ctx, llm.UsageKindResearchQuery)
	resp, err := provider.CreateChat(ctx, messages, nil, llm.ChatOptions{
		MaxTokens: 2048,
		Thinking:  &disabled,
	})
	if err != nil {
		return nil, fmt.Errorf("LLM query generation failed: %w", err)
	}

	// Parse JSON from response (may be wrapped in markdown code blocks)
	jsonStr := extractJSONArray(resp.Content)
	if jsonStr == "" {
		return nil, fmt.Errorf("LLM did not return valid JSON array of queries")
	}

	var queries []searchQuery
	if err := json.Unmarshal([]byte(jsonStr), &queries); err != nil {
		return nil, fmt.Errorf("parse queries JSON: %w\nRaw: %s", err, strutil.Truncate(jsonStr, 200))
	}

	if len(queries) == 0 {
		return nil, nil
	}

	// Clamp to requested number
	if len(queries) > num {
		queries = queries[:num]
	}

	return queries, nil
}

// buildResearcherPrompt builds the prompt for a research sub-agent.
func (dr *DeepResearch) buildResearcherPrompt(query, researchGoal string) string {
	prompt := strings.NewReplacer(
		"{query}", query,
		"{researchGoal}", researchGoal,
		"{language}", dr.cfg.Language(),
	).Replace(dr.cfg.ResearcherPrompt())

	now := time.Now()
	timeInfo := fmt.Sprintf("\n\nCurrent date and time: %s (%s, %s)",
		now.Format(strutil.TimeFormatDateTimeShort),
		now.Weekday().String(),
		now.Location().String(),
	)
	return prompt + timeInfo
}

// writeReport generates the final research report.
// The HTML is produced by a direct LLM call and written to outputPath by
// the engine itself — never delegated to a sub-agent. This guarantees the
// announced "saved" path always matches what is actually on disk (a
// sub-agent writer could fail to write or write elsewhere, and the engine
// had no way to verify before announcing success).
//
// Returns a concise summary (not the HTML) for display in chat UIs; the
// full HTML lives in the file at outputPath.
func (dr *DeepResearch) writeReport(
	ctx context.Context,
	topic string,
	learnings []string,
	urls []string,
	outputPath string,
) (string, error) {
	if len(learnings) == 0 {
		report := dr.buildPartialReport(topic, learnings, urls, nil)
		if err := fileutil.WriteFileShared(outputPath, []byte(report)); err != nil {
			return "", fmt.Errorf("save partial report: %w", err)
		}
		return report, nil
	}

	html, err := dr.generateReportHTML(ctx, topic, learnings, urls)
	if err != nil {
		return "", err
	}

	if err := fileutil.WriteFileShared(outputPath, []byte(html)); err != nil {
		return "", fmt.Errorf("save report: %w", err)
	}

	return buildReportSummary(topic, learnings, urls, outputPath), nil
}

// generateReportHTML produces the final HTML report via a direct LLM call.
// A direct call (instead of a sub-agent) lets the engine control MaxTokens —
// reports routinely exceed the sub-agent's 4096-token cap — and keeps file
// writing in the engine, so the saved path is always the reported path.
func (dr *DeepResearch) generateReportHTML(
	ctx context.Context,
	topic string,
	learnings []string,
	urls []string,
) (string, error) {
	provider, err := dr.getProvider(dr.cfg.QueryGeneratorProvider)
	if err != nil {
		return "", fmt.Errorf("get provider for report writing: %w", err)
	}

	prompt := dr.buildReportWriterPrompt(topic, learnings, urls, "")

	disabled := false
	// Usage billing: tag the call kind (see generateQueries for anchoring).
	ctx = llm.WithUsageKind(ctx, llm.UsageKindResearchReport)
	resp, err := provider.CreateChat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: fmt.Sprintf("Write the research report for: %s", topic)},
	}, nil, llm.ChatOptions{
		MaxTokens: dr.reportMaxTokens,
		Thinking:  &disabled,
	})
	if err != nil {
		return "", fmt.Errorf("LLM report generation failed: %w", err)
	}

	html := strings.TrimSpace(resp.Content)
	if html == "" {
		return "", fmt.Errorf("LLM returned empty report")
	}
	return html, nil
}

// buildReportSummary returns a concise summary of the completed research
// report for display in chat UIs — the full HTML stays in the file at
// outputPath and is never pasted into the conversation.
func buildReportSummary(topic string, learnings []string, urls []string, outputPath string) string {
	return fmt.Sprintf("📄 **研究报告已完成**\n\n- **主题**: %s\n- **发现**: %d 条\n- **来源**: %d 个\n- **文件**: `%s`",
		topic, len(learnings), len(urls), outputPath)
}

// buildReportWriterPrompt builds the prompt for the report writer.
// outputPath is informational — the engine writes the file itself after
// the LLM returns the HTML, so the prompt only needs the HTML content back.
func (dr *DeepResearch) buildReportWriterPrompt(topic string, learnings []string, urls []string, outputPath string) string {
	learningsText := strings.Join(learnings, "\n\n---\n\n")
	urlsText := strings.Join(urls, "\n")

	promptTmpl := dr.cfg.ReportWriterPrompt()
	prompt := strings.NewReplacer(
		"{query}", topic,
		"{learnings}", learningsText,
		"{urls}", urlsText,
		"{output_path}", outputPath,
		"{language}", dr.cfg.Language(),
	).Replace(promptTmpl)

	now := time.Now()
	timeInfo := fmt.Sprintf("\n\nCurrent date and time: %s (%s, %s)",
		now.Format(strutil.TimeFormatDateTimeShort),
		now.Weekday().String(),
		now.Location().String(),
	)
	return prompt + timeInfo
}

// buildPartialReport builds a simple report from whatever learnings were
// collected, even if the research was interrupted.
func (dr *DeepResearch) buildPartialReport(topic string, learnings []string, urls []string, err error) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Deep Research Report: %s\n\n", topic)

	if err != nil {
		fmt.Fprintf(&sb, "> ⚠️ Research was interrupted: %v\n\n", err)
	}

	if len(learnings) == 0 {
		sb.WriteString("No findings were collected.\n")
		return sb.String()
	}

	sb.WriteString("## Findings\n\n")
	for i, l := range learnings {
		fmt.Fprintf(&sb, "### Finding %d\n\n%s\n\n", i+1, l)
	}

	if len(urls) > 0 {
		sb.WriteString("## Sources\n\n")
		seen := make(map[string]bool)
		for _, u := range urls {
			u = strings.TrimSpace(u)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			fmt.Fprintf(&sb, "- %s\n", u)
		}
	}

	return sb.String()
}

// reportPath generates the output file path for a research report.
// The file is placed in ~/.tachi/research/ with a date-and-topic filename.
// Does not create directories or write anything.
func (dr *DeepResearch) reportPath(topic string) string {
	slug := slugifyTopic(topic)
	now := time.Now()
	filename := fmt.Sprintf("%s-%s.html", now.Format("2006-01-02_1504"), slug)
	return filepath.Join(config.ResearchDir(), filename)
}

// ReportPath returns the output path of the most recent Run() call ("" when
// Run has never completed). The path is set at the START of Run (before
// research), so a failed Run may return a path whose file does not exist —
// callers MUST os.Stat the path before registering it as an artifact.
func (dr *DeepResearch) ReportPath() string {
	return dr.lastReportPath
}

// saveReport saves the report content to the given file path.
// Creates the parent directory if it doesn't exist.
// Returns the file path, or empty string on failure (failures are logged, not fatal).
func (dr *DeepResearch) saveReport(filePath string, content string) string {
	if err := fileutil.WriteFileShared(filePath, []byte(content)); err != nil {
		dr.log("DeepResearch: failed to save report to %s: %v", filePath, err)
		return ""
	}

	return filePath
}

var reSlugifyTopic = regexp.MustCompile(`[^a-z0-9\x{4e00}-\x{9fff}\x{3040}-\x{309f}\x{30a0}-\x{30ff}\-_]+`)

// slugifyTopic converts a topic string into a filesystem-safe slug.
func slugifyTopic(topic string) string {
	slug := strings.TrimSpace(topic)
	slug = strings.ToLower(slug)
	// Replace non-alphanumeric characters (except CJK and common punctuation) with hyphens
	slug = reSlugifyTopic.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	slug = strutil.TruncatePlain(slug, 80)
	return strings.Trim(slug, "-")
}

// getProvider resolves a provider by name from the providers config list.
// Named providers are cached to avoid re-creating them on each call.
// If name is empty or not found, falls back to the default provider.
func (dr *DeepResearch) getProvider(name string) (llm.Provider, error) {
	if name == "" {
		if dr.defaultProvider != nil {
			return dr.defaultProvider, nil
		}
		return nil, fmt.Errorf("no provider name specified and no default provider available")
	}

	// Check cache first
	if p, ok := dr.providerCache[name]; ok {
		return p, nil
	}

	for _, p := range dr.providersCfg {
		if p.Name == name {
			resolved, err := p.NewProvider()
			if err != nil {
				return nil, fmt.Errorf("resolve provider %q: %w", name, err)
			}
			dr.providerCache[name] = resolved.Provider
			return resolved.Provider, nil
		}
	}

	// Not found — fall back to default
	if dr.defaultProvider != nil {
		dr.log("DeepResearch: provider %q not found in config, falling back to default", name)
		return dr.defaultProvider, nil
	}
	return nil, fmt.Errorf("provider %q not found and no default available", name)
}

// log writes a debug log message if the logger is configured.
func (dr *DeepResearch) log(format string, args ...any) {
	if dr.logger != nil {
		dr.logger.Info(context.Background(), fmt.Sprintf(format, args...))
	}
}

// ---- Helper functions ----

// urlRegex matches URLs for extraction from sub-agent output.
// Uses \x60 to match backtick within the raw string literal.
var urlRegex = regexp.MustCompile(`https?://[^\s\)\]}>"'\x60]+`)

// extractLearningsAndURLs extracts learnings and URLs from a sub-agent's output.
// It treats the full output as learning content, and extracts URLs separately.
func extractLearningsAndURLs(output string) (learnings []string, urls []string) {
	if output == "" {
		return nil, nil
	}

	// Use the full output as a single learning
	learnings = append(learnings, strings.TrimSpace(output))

	// Extract URLs
	matches := urlRegex.FindAllString(output, -1)
	seen := make(map[string]bool)
	for _, u := range matches {
		u = strings.TrimRight(u, ".,;:!?")
		if !seen[u] {
			urls = append(urls, u)
			seen[u] = true
		}
	}

	return learnings, urls
}

// extractJSONArray extracts a JSON array from an LLM response.
// Handles responses wrapped in markdown code blocks (```json ... ```).
func extractJSONArray(input string) string {
	input = strings.TrimSpace(input)

	// Try to find a JSON array inside markdown code blocks
	if _, after, ok := strings.Cut(input, "```"); ok {
		rest := after
		// Skip optional "json" language marker
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimSpace(rest)

		if before, _, ok := strings.Cut(rest, "```"); ok {
			content := strings.TrimSpace(before)
			if strings.HasPrefix(content, "[") {
				return content
			}
		}
	}

	// No code block — try to find array directly
	if idx := strings.Index(input, "["); idx >= 0 {
		// Find matching closing bracket
		depth := 0
		for i := idx; i < len(input); i++ {
			switch input[i] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					return input[idx : i+1]
				}
			}
		}
	}

	return ""
}
