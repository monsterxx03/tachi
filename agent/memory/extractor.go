package memory

import "context"

// KeywordExtractor extracts search keywords from a user query.
// Used by TopicBackend to improve recall quality for text-based search
// (ripgrep) which lacks semantic understanding. Implementations may use
// LLM, NLP libraries, or simple heuristics.
//
// ExtractKeywords should return 3-5 keywords that capture the search
// intent. On failure, the caller falls back to the raw query.
type KeywordExtractor interface {
	ExtractKeywords(ctx context.Context, query string) ([]string, error)
}
