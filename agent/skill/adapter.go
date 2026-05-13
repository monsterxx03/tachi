// Package skill provides the adapter that bridges the skill.Store to the
// interfaces expected by agent/tools and agent/systemreminder packages,
// avoiding circular imports.
package skill

import (
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
)

// compile-time interface checks
var (
	_ tools.SkillLister = (*Store)(nil)
	_ tools.SkillLoader = (*Store)(nil)
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