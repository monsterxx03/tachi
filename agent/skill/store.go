package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"gopkg.in/yaml.v3"
)

// frontmatter represents the YAML frontmatter of a SKILL.md file.
type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Version     string   `yaml:"version"`
	Author      string   `yaml:"author"`
}

// Store manages skill discovery, loading, and caching.
type Store struct {
	dirs   []string // search directories, ordered by priority (highest first)
	source []string // source label for each dir ("project" or "global")
	logger *debuglog.Logger
}

// NewStore creates a Store scanning both project-level and global skill dirs.
// projectRoot is the workspace root (usually os.Getwd()). If empty, only
// global skills are scanned.
func NewStore(projectRoot string) *Store {
	var dirs []string
	var source []string

	// Priority 1: project-level skills (highest)
	if projectRoot != "" {
		dirs = append(dirs, filepath.Join(projectRoot, ".tachi", "skills"))
		source = append(source, SourceProject)
	}

	// Priority 2: global personal skills
	dirs = append(dirs, config.GlobalSkillsDir())
	source = append(source, SourceGlobal)

	return &Store{dirs: dirs, source: source, logger: debuglog.DefaultLogger}
}

// newStore creates a Store with explicitly provided dirs and sources (for testing).
func newStore(dirs, source []string) *Store {
	return &Store{dirs: dirs, source: source, logger: debuglog.DefaultLogger}
}

// SetLogger sets the debug logger for this store.
func (s *Store) SetLogger(l *debuglog.Logger) {
	s.logger = l
}

// List returns {name, description, tags, source} for all discovered skills.
// Only scans first-level subdirectories containing SKILL.md.
// Higher-priority directories shadow lower-priority ones for same-named skills.
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
				s.logger.Log("skill: List: %s/%s SKILL.md parse error: %v", dir, entry.Name(), err)
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
					Description: truncateDescription(string(data), MaxDescriptionLen),
					Source:      source,
				})
				continue
			}

			name := fm.Name
			if name == "" {
				name = entry.Name()
			}
			if validateErr := ValidateName(name); validateErr != nil {
				s.logger.Log("skill: List: invalid skill name %q in %s: %v", name, dir, validateErr)
				continue
			}
			if seen[name] {
				s.logger.Log("skill: List: skill %q from %s shadowed by higher-priority source", name, dir)
				continue // shadowed by higher-priority skill
			}
			seen[name] = true

			desc := fm.Description
			if desc == "" {
				desc = truncateDescription(string(data), MaxDescriptionLen)
			} else if len(desc) > MaxDescriptionLen {
				desc = truncateDescription(desc, MaxDescriptionLen)
			}

			result = append(result, SkillMeta{
				Name:        name,
				Description: desc,
				Tags:        fm.Tags,
				Source:      source,
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
func (s *Store) Load(name string) (*Skill, error) {
	for i, dir := range s.dirs {
		source := s.source[i]

		skillDir := filepath.Join(dir, name)
		skillFile := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		rawContent := string(data)
		fm, body, err := parseFrontmatter(rawContent)
		if err != nil {
			// YAML parse error — still return the skill with directory name
			s.logger.Log("skill: Load: %s/%s SKILL.md parse error: %v", dir, name, err)
			body = rawContent
		}

		skillName := name
		if fm != nil && fm.Name != "" {
			skillName = fm.Name
		}

		desc := ""
		tags := []string{}
		if fm != nil {
			desc = fm.Description
			tags = fm.Tags
		}

		if desc == "" {
			desc = truncateDescription(body, MaxDescriptionLen)
		} else if len(desc) > MaxDescriptionLen {
			desc = truncateDescription(desc, MaxDescriptionLen)
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
			},
			Body:       body,
			RawContent: rawContent,
			Dir:        skillDir,
			Files:      files,
		}, nil
	}

	return nil, fmt.Errorf("skill %q not found", name)
}

// ResolveCommand maps a slash-command name (e.g. "/code-review") to the
// canonical skill name. Returns the skill name and whether it was found.
// The input may or may not include a leading "/".
func (s *Store) ResolveCommand(cmd string) (string, bool) {
	name := strings.TrimPrefix(cmd, "/")
	name = strings.ToLower(name)

	metas := s.List()
	for _, m := range metas {
		if strings.EqualFold(m.Name, name) {
			return m.Name, true
		}
	}
	return "", false
}

// Create writes a new skill to the filesystem at the appropriate location.
// source must be "project" or "global". Returns the created Skill.
// If overwrite is false and the skill already exists, an error is returned.
func (s *Store) Create(name, description, body string, tags []string, source string, overwrite bool) (*Skill, error) {
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("invalid skill name: %w", err)
	}
	if len(description) > MaxDescriptionLen {
		return nil, fmt.Errorf("description exceeds %d characters", MaxDescriptionLen)
	}
	if source != SourceProject && source != SourceGlobal {
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
		s.logger.Log("skill: Create: overwriting existing skill %q at %s", name, skillFile)
	}

	// Create directory
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Build SKILL.md content
	content := buildSkillMarkdown(name, description, body, tags)

	if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write SKILL.md: %w", err)
	}

	s.logger.Log("skill: Create: created skill %q at %s", name, skillFile)

	return &Skill{
		Meta: SkillMeta{
			Name:        name,
			Description: description,
			Tags:        tags,
			Source:      source,
		},
		Body:  body,
		RawContent: content,
		Dir:   skillDir,
		Files: map[string]string{},
	}, nil
}

// Delete removes a skill from the filesystem. source narrows the search scope
// ("project" or "global"); empty means search all dirs.
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

	s.logger.Log("skill: Delete: removed skill %q at %s", name, skillDir)
	return nil
}

// Update modifies an existing skill's description, body, and/or tags.
// name is required. description, body, and tags are optional — only non-zero
// values are applied (empty string keeps existing description/body; nil tags
// keeps existing tags). source narrows the search scope ("project" or
// "global"); empty means search all dirs (respects priority).
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

	// If no fields are being updated, just re-read and return
	if description == "" && body == "" && tags == nil {
		return s.Load(name)
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
			description = truncateDescription(existingBody, MaxDescriptionLen)
		}
	}
	if body == "" {
		body = existingBody
	}
	if tags == nil {
		tags = fm.Tags
	}

	// Build new SKILL.md
	content := buildSkillMarkdown(name, description, body, tags)

	if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write SKILL.md: %w", err)
	}

	s.logger.Log("skill: Update: updated skill %q at %s", name, skillFile)

	return &Skill{
		Meta: SkillMeta{
			Name:        name,
			Description: description,
			Tags:        tags,
			Source:      effectiveSource,
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
		if source != SourceProject && source != SourceGlobal {
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
func buildSkillMarkdown(name, description, body string, tags []string) string {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("name: %s\n", name))
	b.WriteString(fmt.Sprintf("description: %s\n", description))
	if len(tags) > 0 {
		// Manually format YAML array to avoid dependency on yaml.Marshal for this simple case
		b.WriteString("tags:\n")
		for _, t := range tags {
			b.WriteString(fmt.Sprintf("  - %s\n", t))
		}
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
	endIdx := strings.Index(rest, "\n---")
	if endIdx < 0 {
		return nil, content, fmt.Errorf("unclosed frontmatter")
	}

	yamlBlock := rest[:endIdx]
	body := rest[endIdx+4:] // skip "\n---"

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
	checkLen := len(data)
	if checkLen > 8192 {
		checkLen = 8192
	}
	for _, b := range data[:checkLen] {
		if b == 0 {
			return true
		}
	}
	return false
}

// truncateDescription truncates text to maxLen characters for use as
// a fallback description when no frontmatter description is provided.
func truncateDescription(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen])
}