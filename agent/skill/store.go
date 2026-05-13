package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	logger *debuglog.Logger
}

// NewStore creates a Store scanning both project-level and global skill dirs.
// projectRoot is the workspace root (usually os.Getwd()). If empty, only
// global skills are scanned.
func NewStore(projectRoot string) *Store {
	var dirs []string

	// Priority 1: project-level skills (highest)
	if projectRoot != "" {
		dirs = append(dirs, filepath.Join(projectRoot, ".tachi", "skills"))
	}

	// Priority 2: global personal skills
	homeDir, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(homeDir, ".tachi", "skills"))
	}

	return &Store{dirs: dirs, logger: debuglog.DefaultLogger}
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
		source := SourceProject
		if i > 0 {
			source = SourceGlobal
		}

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
		source := SourceProject
		if i > 0 {
			source = SourceGlobal
		}

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