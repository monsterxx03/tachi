package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"gopkg.in/yaml.v3"
)

// frontmatter represents the YAML frontmatter of a SKILL.md file.
type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Version     string   `yaml:"version"`
	Author      string   `yaml:"author"`
	Enabled     *bool    `yaml:"enabled"` // nil = enabled (default); explicit false disables the skill
}

// enabledOrDefault resolves the optional frontmatter enabled flag.
// A nil flag (field absent) means the skill is enabled.
func enabledOrDefault(e *bool) bool {
	return e == nil || *e
}

// Store manages skill discovery, loading, and caching.
type Store struct {
	dirs   []string // search directories, ordered by priority (highest first)
	source []string // source label for each dir ("project", "claude", "cursor", "global")
	logger *logger.Logger
}

// NewStore creates a Store scanning project-level and global skill dirs in
// priority order (highest first):
//
//	<project>/.tachi/skills
//	<project>/.claude/skills
//	<project>/.cursor/skills
//	~/.tachi/skills
//
// Skills with the same name are shadowed by the first directory that contains
// them. projectRoot is the workspace root (usually os.Getwd()). If empty, only
// global skills are scanned.
//
// Deduplication: when a directory resolves to the same path as an
// already-added directory (e.g., when projectRoot equals $HOME so
// <project>/.tachi/skills equals ~/.tachi/skills), the duplicate is skipped.
func NewStore(projectRoot string) *Store {
	var dirs []string
	var source []string

	addDir := func(dir, src string) {
		clean := filepath.Clean(dir)
		for _, d := range dirs {
			if filepath.Clean(d) == clean {
				return // duplicate
			}
		}
		dirs = append(dirs, dir)
		source = append(source, src)
	}

	// Priority 1: project-level .tachi/skills (highest)
	if projectRoot != "" {
		addDir(filepath.Join(projectRoot, ".tachi", "skills"), SourceProject)
		addDir(filepath.Join(projectRoot, ".claude", "skills"), SourceClaude)
		addDir(filepath.Join(projectRoot, ".cursor", "skills"), SourceCursor)
	}

	// Lowest priority: global personal skills
	addDir(config.GlobalSkillsDir(), SourceGlobal)

	return &Store{dirs: dirs, source: source, logger: nil}
}

// newStore creates a Store with explicitly provided dirs and sources (for testing).
func newStore(dirs, source []string) *Store {
	return &Store{dirs: dirs, source: source, logger: nil}
}

// NewStoreWithDirs creates a Store scanning the given directories in priority
// order (highest first). Each entry in sources must be a valid source constant
// (SourceProject, SourceClaude, SourceCursor, or SourceGlobal) and have the
// same length as dirs. Intended for tests and other callers that need an
// isolated, hermetic skill scope.
func NewStoreWithDirs(dirs, sources []string) *Store {
	return newStore(dirs, sources)
}

// Reload is a no-op placeholder for callers that want to drop any internal
// caches a Store might keep. List() and Load() always re-read the disk, so
// no state needs to be cleared today — but exposing this method lets callers
// express the intent ("re-scan now") and stay forward-compatible.
//
// Returns the number of skills currently visible, mirroring List() length.
func (s *Store) Reload() int {
	return len(s.List())
}

// Dirs returns the directories this store scans, in priority order.
func (s *Store) Dirs() []string {
	out := make([]string, len(s.dirs))
	copy(out, s.dirs)
	return out
}

// Sources returns the source labels parallel to Dirs().
func (s *Store) Sources() []string {
	out := make([]string, len(s.source))
	copy(out, s.source)
	return out
}

// SetLogger sets the debug logger for this store.
func (s *Store) SetLogger(l *logger.Logger) {
	s.logger = l
}

// List returns {name, description, tags, source, enabled} for all discovered
// skills. Only scans first-level subdirectories containing SKILL.md.
// Higher-priority directories shadow lower-priority ones for same-named skills.
// Disabled skills (enabled: false in frontmatter) are included but flagged
// with Enabled=false — and they still shadow lower-priority same-named
// skills, so a project can suppress a global skill by disabling it locally.
// Callers exposing skills to the LLM should skip entries with !Enabled.
func (s *Store) List() []SkillMeta {
	seen := make(map[string]bool)
	var result []SkillMeta

	for i, dir := range s.dirs {
		source := s.source[i]

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // directory doesn't exist — skip
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			skillDir := filepath.Join(dir, entry.Name())
			skillFile := filepath.Join(skillDir, "SKILL.md")
			data, err := os.ReadFile(skillFile)
			if err != nil {
				continue // no SKILL.md in this directory
			}

			fm, _, err := parseFrontmatter(string(data))
			if err != nil {
				// YAML parse error — use directory name as fallback
				s.logger.Error(context.Background(), "skill: List: SKILL.md parse error", err, "dir", dir, "name", entry.Name())
				name := entry.Name()
				if validateErr := ValidateName(name); validateErr != nil {
					continue
				}
				if seen[name] {
					continue
				}
				seen[name] = true
				result = append(result, SkillMeta{
					Name:        name,
					Description: strutil.TruncatePlain(string(data), MaxDescriptionLen),
					Source:      source,
					Enabled:     true,
				})
				continue
			}

			name := fm.Name
			if name == "" {
				name = entry.Name()
			}
			if validateErr := ValidateName(name); validateErr != nil {
				s.logger.Info(context.Background(), "skill: List: invalid skill name", "name", name, "dir", dir, "error", validateErr)
				continue
			}
			if seen[name] {
				s.logger.Info(context.Background(), "skill: List: skill shadowed by higher-priority source", "skill", name, "dir", dir)
				continue // shadowed by higher-priority skill
			}
			seen[name] = true

			desc := fm.Description
			if desc == "" {
				desc = strutil.TruncatePlain(string(data), MaxDescriptionLen)
			} else if len(desc) > MaxDescriptionLen {
				desc = strutil.TruncatePlain(desc, MaxDescriptionLen)
			}

			result = append(result, SkillMeta{
				Name:        name,
				Description: desc,
				Tags:        fm.Tags,
				Source:      source,
				Enabled:     enabledOrDefault(fm.Enabled),
			})
		}
	}

	// Sort by name for deterministic output
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// Load reads and parses a skill's SKILL.md, returns full content + metadata.
// Respects priority: project skills shadow global ones.
// Returns an error for disabled skills (enabled: false in frontmatter) —
// a disabled skill is not loadable until re-enabled on disk.
func (s *Store) Load(name string) (*Skill, error) {
	for i, dir := range s.dirs {
		source := s.source[i]

		sk, err := s.loadFromDir(dir, source, name)
		if err != nil {
			continue
		}
		if !sk.Meta.Enabled {
			return nil, fmt.Errorf("skill %q is disabled (remove enabled: false in %s to re-enable)", name, filepath.Join(sk.Dir, "SKILL.md"))
		}
		return sk, nil
	}

	return nil, fmt.Errorf("skill %q not found", name)
}

// loadFromDir reads and parses a skill from a specific directory, returning
// an error if the skill does not exist there. Unlike Load, it does NOT
// reject disabled skills — callers like Update need access to them.
func (s *Store) loadFromDir(dir, source, name string) (*Skill, error) {
	skillDir := filepath.Join(dir, name)
	skillFile := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return nil, err
	}

	rawContent := string(data)
	fm, body, err := parseFrontmatter(rawContent)
	if err != nil {
		// YAML parse error — still return the skill with directory name
		s.logger.Error(context.Background(), "skill: Load: SKILL.md parse error", err, "dir", dir, "name", name)
		body = rawContent
	}

	skillName := name
	if fm != nil && fm.Name != "" {
		skillName = fm.Name
	}

	desc := ""
	tags := []string{}
	enabled := true
	if fm != nil {
		desc = fm.Description
		tags = fm.Tags
		enabled = enabledOrDefault(fm.Enabled)
	}

	if desc == "" {
		desc = strutil.TruncatePlain(body, MaxDescriptionLen)
	} else if len(desc) > MaxDescriptionLen {
		desc = strutil.TruncatePlain(desc, MaxDescriptionLen)
	}

	// Load supporting files (references/, templates/, scripts/)
	files, err := loadSupportingFiles(skillDir)
	if err != nil {
		// Non-fatal: still return skill without supporting files
		files = nil
	}

	return &Skill{
		Meta: SkillMeta{
			Name:        skillName,
			Description: desc,
			Tags:        tags,
			Source:      source,
			Enabled:     enabled,
		},
		Body:       body,
		RawContent: rawContent,
		Dir:        skillDir,
		Files:      files,
	}, nil
}

// ResolveCommand maps a slash-command name (e.g. "/code-review") to the
// canonical skill name. Returns the skill name and whether it was found.
// The input may or may not include a leading "/".
// Disabled skills (enabled: false) do not resolve — they cannot be activated.
func (s *Store) ResolveCommand(cmd string) (string, bool) {
	name := strings.TrimPrefix(cmd, "/")
	name = strings.ToLower(name)

	metas := s.List()
	for _, m := range metas {
		if !m.Enabled {
			continue
		}
		if strings.EqualFold(m.Name, name) {
			return m.Name, true
		}
	}
	return "", false
}

// Create writes a new skill to the filesystem at the appropriate location.
// source must be "project" (.tachi/skills/) or "global" (~/.tachi/skills/).
// Returns the created Skill.
// If overwrite is false and the skill already exists, an error is returned.
func (s *Store) Create(name, description, body string, tags []string, source string, overwrite bool) (*Skill, error) {
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("invalid skill name: %w", err)
	}
	if len(description) > MaxDescriptionLen {
		return nil, fmt.Errorf("description exceeds %d characters", MaxDescriptionLen)
	}
	if !isWritableSource(source) {
		return nil, fmt.Errorf("unknown source %q: must be %q or %q", source, SourceProject, SourceGlobal)
	}

	// Determine target directory
	idx := slices.Index(s.source, source)
	if idx < 0 {
		return nil, fmt.Errorf("skill directory for source %q is not available", source)
	}
	targetDir := s.dirs[idx]

	skillDir := filepath.Join(targetDir, name)

	// Check for existing skill
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if _, err := os.Stat(skillFile); err == nil {
		if !overwrite {
			return nil, fmt.Errorf("skill %q already exists at %s (use overwrite=true to replace)", name, skillFile)
		}
		s.logger.Info(context.Background(), "skill: Create: overwriting existing skill", "skill", name, "path", skillFile)
	}

	// Create directory
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Build SKILL.md content
	content := buildSkillMarkdown(name, description, body, tags, true)

	if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write SKILL.md: %w", err)
	}

	s.logger.Info(context.Background(), "skill: Create: created skill", "skill", name, "path", skillFile)

	return &Skill{
		Meta: SkillMeta{
			Name:        name,
			Description: description,
			Tags:        tags,
			Source:      source,
			Enabled:     true,
		},
		Body:       body,
		RawContent: content,
		Dir:        skillDir,
		Files:      map[string]string{},
	}, nil
}

// Delete removes a skill from the filesystem. source narrows the search scope
// ("project" or "global"); empty means search all dirs (respects priority).
func (s *Store) Delete(name string, source string) error {
	if err := ValidateName(name); err != nil {
		return fmt.Errorf("invalid skill name: %w", err)
	}

	skillDir, _, err := s.findSkillDir(name, source)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("failed to delete skill directory %q: %w", skillDir, err)
	}

	s.logger.Info(context.Background(), "skill: Delete: removed skill", "skill", name, "path", skillDir)
	return nil
}

// Update modifies an existing skill's description, body, and/or tags.
// name is required. description, body, and tags are optional — only non-zero
// values are applied (empty string keeps existing description/body; nil tags
// keeps existing tags). source narrows the search scope
// ("project" or "global"); empty means search all dirs (respects priority).
func (s *Store) Update(name string, description, body string, tags []string, source string) (*Skill, error) {
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("invalid skill name: %w", err)
	}
	if description != "" && len(description) > MaxDescriptionLen {
		return nil, fmt.Errorf("description exceeds %d characters", MaxDescriptionLen)
	}

	// Find the skill directory, respecting source if specified
	skillDir, effectiveSource, err := s.findSkillDir(name, source)
	if err != nil {
		return nil, fmt.Errorf("skill %q not found: %w", name, err)
	}

	skillFile := filepath.Join(skillDir, "SKILL.md")

	// If no fields are being updated, just re-read and return. Use loadFromDir
	// directly (not Load) so disabled skills remain updatable — Load rejects
	// them, which would make a disabled skill impossible to re-enable via
	// a content update.
	if description == "" && body == "" && tags == nil {
		return s.loadFromDir(filepath.Dir(skillDir), effectiveSource, name)
	}

	// Parse existing file to merge updates
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read SKILL.md: %w", err)
	}

	fm, existingBody, parseErr := parseFrontmatter(string(data))
	if parseErr != nil {
		// Parse error — treat as raw body without frontmatter
		fm = &frontmatter{Name: name}
		existingBody = string(data)
	}

	// Apply updates (zero means keep existing)
	if description == "" {
		if fm.Description != "" {
			description = fm.Description
		} else {
			description = strutil.TruncatePlain(existingBody, MaxDescriptionLen)
		}
	}
	if body == "" {
		body = existingBody
	}
	if tags == nil {
		tags = fm.Tags
	}
	// The enabled flag is not updatable via Update — preserve the on-disk value.
	enabled := enabledOrDefault(fm.Enabled)

	// Build new SKILL.md
	content := buildSkillMarkdown(name, description, body, tags, enabled)

	if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write SKILL.md: %w", err)
	}

	s.logger.Info(context.Background(), "skill: Update: updated skill", "skill", name, "path", skillFile)

	return &Skill{
		Meta: SkillMeta{
			Name:        name,
			Description: description,
			Tags:        tags,
			Source:      effectiveSource,
			Enabled:     enabled,
		},
		Body:       body,
		RawContent: content,
		Dir:        skillDir,
		Files:      loadSupportingFilesSafe(skillDir),
	}, nil
}

// findSkillDir locates the on-disk directory for a named skill.
// source narrows search — empty means search all dirs.
func (s *Store) findSkillDir(name, source string) (string, string, error) {
	if source != "" {
		if !isWritableSource(source) {
			return "", "", fmt.Errorf("unknown source %q: must be %q or %q", source, SourceProject, SourceGlobal)
		}
		idx := slices.Index(s.source, source)
		if idx < 0 {
			return "", "", fmt.Errorf("skill directory for source %q is not available", source)
		}
		skillDir := filepath.Join(s.dirs[idx], name)
		skillFile := filepath.Join(skillDir, "SKILL.md")
		if _, err := os.Stat(skillFile); err != nil {
			return "", "", fmt.Errorf("skill %q not found in source %q", name, source)
		}
		return skillDir, source, nil
	}

	// Search all dirs in priority order
	for i, dir := range s.dirs {
		skillDir := filepath.Join(dir, name)
		skillFile := filepath.Join(skillDir, "SKILL.md")
		if _, err := os.Stat(skillFile); err == nil {
			return skillDir, s.source[i], nil
		}
	}
	return "", "", fmt.Errorf("skill %q not found", name)
}

// buildSkillMarkdown constructs the full SKILL.md content from fields.
// The enabled flag is only written when false — skills are enabled by
// default, so the common case keeps frontmatter minimal.
func buildSkillMarkdown(name, description, body string, tags []string, enabled bool) string {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: %s\n", description)
	if len(tags) > 0 {
		// Manually format YAML array to avoid dependency on yaml.Marshal for this simple case
		b.WriteString("tags:\n")
		for _, t := range tags {
			fmt.Fprintf(&b, "  - %s\n", t)
		}
	}
	if !enabled {
		b.WriteString("enabled: false\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)

	// Ensure trailing newline
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// parseFrontmatter extracts YAML frontmatter and body from SKILL.md content.
// Frontmatter is delimited by --- on its own lines.
func parseFrontmatter(content string) (*frontmatter, string, error) {
	if !strings.HasPrefix(content, "---") {
		return nil, content, fmt.Errorf("no frontmatter delimiter found")
	}

	// Find closing delimiter
	rest := content[3:] // skip opening "---"
	before, after, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, content, fmt.Errorf("unclosed frontmatter")
	}

	yamlBlock := before
	body := after // skip "\n---"

	// Handle optional trailing newline(s) after closing delimiter
	body = strings.TrimLeft(body, "\n")

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil, body, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	return &fm, body, nil
}

// loadSupportingFiles reads all supporting files from a skill directory.
// Returns a map of relative path → content for text files under 1MB.
// Skips SKILL.md itself and binary files.
func loadSupportingFiles(dir string) (map[string]string, error) {
	result := make(map[string]string)

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible files
		}
		if d.IsDir() {
			if path != dir && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip SKILL.md itself
		if filepath.Base(path) == "SKILL.md" && filepath.Dir(path) == dir {
			return nil
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}

		// Read file, skip if > 1MB or binary
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 { // 1MB
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Skip binary files
		if isBinary(data) {
			return nil
		}

		result[relPath] = string(data)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// loadSupportingFilesSafe is like loadSupportingFiles but returns nil on error.
func loadSupportingFilesSafe(dir string) map[string]string {
	files, err := loadSupportingFiles(dir)
	if err != nil {
		return nil
	}
	return files
}

// isBinary detects binary content by checking for null bytes in the first 8KB.
func isBinary(data []byte) bool {
	checkLen := min(len(data), 8192)
	return slices.Contains(data[:checkLen], 0)
}
