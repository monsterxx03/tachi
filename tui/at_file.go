package tui

import (
	"context"
	"os"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/pkg/fileindex"
)

// searchAtFiles searches the working directory for files matching query. An
// empty query lists its immediate entries. The underlying index is cached, so
// this stays cheap enough to run on every keystroke.
func (i *InputArea) searchAtFiles(query string) ([]fileindex.Match, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return i.files.Search(cwd, query, fileindex.DefaultLimit)
}

// ExpandAtReferences expands the @-path references in a user message for the
// LLM: text files and directories are inlined, images become content parts for
// multi-modal input, binaries are annotated by path. The TUI keeps displaying
// the unexpanded text.
func (m *Model) ExpandAtReferences(message string) atfile.Result {
	cwd, _ := os.Getwd()
	result := atfile.Expand(cwd, message)
	if result.Refs > 0 {
		m.logger.Info(context.Background(), "at_file: expanded @ references in message",
			"refs", result.Refs, "images", len(result.Images))
	}
	return result
}
