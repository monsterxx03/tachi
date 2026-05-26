// Package skill provides the adapter that bridges the skill.Store to the
// interfaces expected by agent/tools and agent/systemreminder packages,
// avoiding circular imports.
package skill

import (
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
)

// compile-time interface checks
var (
	_ tools.SkillManager              = (*Store)(nil)
	_ systemreminder.SkillMetaProvider = (*Store)(nil)
)

// ---- tools.SkillLister ----

// ListSkills implements tools.SkillLister.
func (s *Store) ListSkills() []tools.SkillListEntry {
	metas := s.List()
	entries := make([]tools.SkillListEntry, 0, len(metas))
	for _, m := range metas {
		entries = append(entries, tools.SkillListEntry{
			Name:        m.Name,
			Description: m.Description,
			Tags:        m.Tags,
			Source:      m.Source,
		})
	}
	return entries
}

// ---- tools.SkillLoader ----

// LoadSkill implements tools.SkillLoader.
func (s *Store) LoadSkill(name string) (*tools.SkillData, error) {
	sk, err := s.Load(name)
	if err != nil {
		return nil, err
	}
	return &tools.SkillData{
		Name:        sk.Meta.Name,
		Description: sk.Meta.Description,
		Body:        sk.Body,
		Source:      sk.Meta.Source,
		Dir:         sk.Dir,
		Files:       sk.Files,
	}, nil
}

// ---- systemreminder.SkillMetaProvider ----

// ListSkillMetas implements systemreminder.SkillMetaProvider.
func (s *Store) ListSkillMetas() []systemreminder.SkillMetaRecord {
	metas := s.List()
	records := make([]systemreminder.SkillMetaRecord, 0, len(metas))
	for _, m := range metas {
		records = append(records, systemreminder.SkillMetaRecord{
			Name:        m.Name,
			Description: m.Description,
			Tags:        m.Tags,
		})
	}
	return records
}

// ---- tools.SkillCreator ----

// CreateSkill implements tools.SkillCreator.
func (s *Store) CreateSkill(params tools.SkillCreateParams) (*tools.SkillCreateResult, error) {
	sk, err := s.Create(params.Name, params.Description, params.Body, params.Tags, params.Source, params.Overwrite)
	if err != nil {
		return nil, err
	}
	return &tools.SkillCreateResult{
		Name:        sk.Meta.Name,
		Description: sk.Meta.Description,
		Tags:        sk.Meta.Tags,
		Source:      sk.Meta.Source,
		Path:        filepath.Join(sk.Dir, "SKILL.md"),
	}, nil
}

// ---- tools.SkillDeleter ----

// DeleteSkill implements tools.SkillDeleter.
func (s *Store) DeleteSkill(name, source string) error {
	return s.Delete(name, source)
}

// ---- tools.SkillUpdater ----

// UpdateSkill implements tools.SkillUpdater.
func (s *Store) UpdateSkill(params tools.SkillUpdateParams) (*tools.SkillUpdateResult, error) {
	sk, err := s.Update(params.Name, params.Description, params.Body, params.Tags, params.Source)
	if err != nil {
		return nil, err
	}
	return &tools.SkillUpdateResult{
		Name:        sk.Meta.Name,
		Description: sk.Meta.Description,
		Tags:        sk.Meta.Tags,
		Source:      sk.Meta.Source,
		Path:        filepath.Join(sk.Dir, "SKILL.md"),
	}, nil
}