// Package bm25 implements the Okapi BM25 ranking function as a pure
// algorithm layer with no dependencies beyond the standard library.
//
// It supports per-field weighting: a document is a sequence of named-by-
// position fields (e.g. name, server, description), each carrying its own
// boost. BM25 is computed independently per field — term frequency
// saturation and length normalization are field-local, as in Lucene — and
// the field scores are combined with their boosts. Document frequency (IDF)
// is global across the corpus.
//
// The package is intentionally free of business logic: callers supply
// already-tokenized text (see the tokenizer split in the caller) and handle
// query syntax themselves. The index supports incremental Add; for removals,
// rebuild a fresh Index (corpora in tachi are small — tens to hundreds of
// short documents — so rebuild cost is negligible).
package bm25

import (
	"math"
	"sort"
)

// Field is a weighted token sequence within a Document. Tokens are expected
// to be pre-normalized (lowercased, stop-word filtered) by the caller; the
// index performs no normalization of its own.
type Field struct {
	// Boost scales this field's contribution. A non-positive value (0 or
	// negative) uses the default 1.0.
	Boost float64
	// Tokens are the raw terms of the field, in any order (frequency is
	// counted, order is ignored).
	Tokens []string
}

// Document is a bag of fields. Fields are matched by position across all
// documents in an index: Document.Fields[0] is assumed to be the same logical
// field in every document. Missing trailing fields are treated as empty.
type Document struct {
	Fields []Field
}

// Params holds the BM25 tuning parameters. A zero K1 falls back to the
// default; B is honored as-is (0 disables length normalization, values
// outside [0,1] fall back to the default).
type Params struct {
	// K1 controls term-frequency saturation: higher values let additional
	// occurrences of a term keep adding score (default 1.2).
	K1 float64
	// B controls length normalization: 0 disables it, 1 is full
	// normalization against average field length (default 0.75).
	B float64
}

// DefaultParams returns the widely used BM25 defaults (k1=1.2, b=0.75).
func DefaultParams() Params {
	return Params{K1: 1.2, B: 0.75}
}

// Scored pairs a document index with its BM25 score, returned by TopN.
type Scored struct {
	Doc   int
	Score float64
}

// Index is an immutable-by-contract BM25 index over a fixed set of documents
// (plus optional incremental Add). Concurrent reads are safe; Add may only
// run while no other goroutine is reading or adding, so callers sharing an
// index across goroutines must hold an external lock for its whole lifetime.
type Index struct {
	params Params

	nDocs       int
	nFields     int
	avgFieldLen []float64 // per-field average length across all docs

	df map[string]int // term → number of documents containing it (global)

	// fieldTF[doc][field] → term → frequency in that field.
	fieldTF [][]map[string]int
	// fieldLen[doc][field] → number of tokens in that field.
	fieldLen [][]int
	// docBoost[doc][field] → effective boost, resolved at add time (0 → 1.0)
	// so the hot scoring path reads a precomputed value.
	docBoost [][]float64
}

// New builds an index over docs. The number of fields is the maximum across
// all documents; documents with fewer fields are padded, extra fields beyond
// it are ignored. Zero-valued Params fields fall back to the defaults.
func New(docs []Document, p Params) *Index {
	ix := &Index{
		params: normalizeParams(p),
		df:     make(map[string]int),
	}
	ix.nFields = maxFields(docs)
	ix.avgFieldLen = make([]float64, ix.nFields)
	for _, d := range docs {
		ix.addDoc(d)
	}
	return ix
}

// Add appends a document to the index, updating corpus statistics
// (document frequency and per-field average lengths) incrementally. It is
// the incremental counterpart of including the document in New.
func (ix *Index) Add(d Document) {
	ix.addDoc(d)
}

// Len returns the number of documents in the index.
func (ix *Index) Len() int {
	return ix.nDocs
}

// IDF returns the inverse document frequency of term. Unknown or
// empty terms score 0. The Lucene variant is used so the value is always
// non-negative, even when a term appears in more than half the corpus.
func (ix *Index) IDF(term string) float64 {
	if term == "" || ix.nDocs == 0 {
		return 0
	}
	df := float64(ix.df[term])
	return math.Log(1 + (float64(ix.nDocs)-df+0.5)/(df+0.5))
}

// Score returns the BM25 score of query against document doc. A score of 0
// means no term of the query matched any field. An out-of-range doc returns
// 0. Duplicate terms in query are not deduplicated — each occurrence adds to
// the score — so callers should pass deduplicated terms (matching Lucene's
// BooleanQuery semantics).
func (ix *Index) Score(query []string, doc int) float64 {
	if doc < 0 || doc >= ix.nDocs || len(query) == 0 {
		return 0
	}
	var total float64
	for _, term := range query {
		if term == "" {
			continue
		}
		idf := ix.IDF(term)
		if idf == 0 {
			continue
		}
		for f := 0; f < ix.nFields; f++ {
			tf := ix.fieldTF[doc][f][term]
			if tf == 0 {
				continue
			}
			total += idf * ix.docBoost[doc][f] * ix.termScore(float64(tf), float64(ix.fieldLen[doc][f]), ix.avgFieldLen[f])
		}
	}
	return total
}

// Scores returns the BM25 score of query against every document, in
// document order.
func (ix *Index) Scores(query []string) []float64 {
	scores := make([]float64, ix.nDocs)
	for d := range ix.nDocs {
		scores[d] = ix.Score(query, d)
	}
	return scores
}

// TopN returns the n highest-scoring documents for query, sorted by score
// descending. Ties are broken by document index (stable, deterministic).
// n <= 0 returns nil; n > Len returns all scored documents.
//
// Every document is scored and the results sorted; fine for the small
// corpora tachi targets. A bounded heap would avoid the full sort if
// corpora ever grow.
func (ix *Index) TopN(query []string, n int) []Scored {
	if n <= 0 || ix.nDocs == 0 {
		return nil
	}
	scored := make([]Scored, 0, min(n, ix.nDocs))
	for d := range ix.nDocs {
		sc := ix.Score(query, d)
		if sc > 0 {
			scored = append(scored, Scored{Doc: d, Score: sc})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Doc < scored[j].Doc
	})
	if len(scored) > n {
		scored = scored[:n]
	}
	return scored
}

// ---- internals ----

func (ix *Index) termScore(tf, fieldLen float64, avgFieldLen float64) float64 {
	// tf * (k1 + 1) / (tf + k1 * (1 - b + b * fieldLen / avgFieldLen))
	//
	// When the field is empty corpus-wide (avg 0) there is nothing to
	// normalize against — drop the length term entirely (equivalent to
	// b = 0 for that field).
	if avgFieldLen <= 0 {
		return tf * (ix.params.K1 + 1) / (tf + ix.params.K1)
	}
	denom := tf + ix.params.K1*(1-ix.params.B+ix.params.B*fieldLen/avgFieldLen)
	return tf * (ix.params.K1 + 1) / denom
}

func (ix *Index) addDoc(d Document) {
	// Extend the field count if this document introduces more fields than
	// any previous one (happens when the index starts empty and Add grows
	// it). Existing documents get zero-padded field slots so positional
	// indexing stays consistent.
	if len(d.Fields) > ix.nFields {
		old := ix.nFields
		ix.nFields = len(d.Fields)
		ix.avgFieldLen = append(ix.avgFieldLen, make([]float64, ix.nFields-old)...)
		for i := range ix.fieldTF {
			ix.fieldTF[i] = append(ix.fieldTF[i], make([]map[string]int, ix.nFields-old)...)
			ix.fieldLen[i] = append(ix.fieldLen[i], make([]int, ix.nFields-old)...)
			ix.docBoost[i] = append(ix.docBoost[i], make([]float64, ix.nFields-old)...)
		}
	}

	idx := ix.nDocs
	ix.nDocs++
	if len(ix.fieldTF) <= idx {
		ix.fieldTF = append(ix.fieldTF, make([]map[string]int, ix.nFields))
		ix.fieldLen = append(ix.fieldLen, make([]int, ix.nFields))
		ix.docBoost = append(ix.docBoost, make([]float64, ix.nFields))
	}

	tf := ix.fieldTF[idx]
	seen := make(map[string]bool) // distinct terms of this doc, for df
	for f := 0; f < min(len(d.Fields), ix.nFields); f++ {
		field := d.Fields[f]
		fieldTf := tf[f]
		if fieldTf == nil {
			fieldTf = make(map[string]int)
			tf[f] = fieldTf
		}
		boost := field.Boost
		if boost <= 0 {
			boost = 1
		}
		ix.docBoost[idx][f] = boost
		for _, tok := range field.Tokens {
			if tok == "" {
				continue
			}
			fieldTf[tok]++
			ix.fieldLen[idx][f]++
			seen[tok] = true
		}
	}
	// Incremental weighted average of field length, over every field:
	// newAvg = (oldAvg*n + len) / (n+1), with len = 0 for missing or empty
	// fields. Updating all fields — not just the ones present in this
	// document — keeps the average insertion-order-independent and
	// consistent with "missing trailing fields are treated as empty". A
	// field only starts participating once introduced (first document that
	// has it); earlier documents predate it and are not counted.
	n := float64(ix.nDocs - 1)
	for f := 0; f < ix.nFields; f++ {
		ix.avgFieldLen[f] = (ix.avgFieldLen[f]*n + float64(ix.fieldLen[idx][f])) / (n + 1)
	}
	for term := range seen {
		ix.df[term]++
	}
}

func normalizeParams(p Params) Params {
	def := DefaultParams()
	if p.K1 <= 0 {
		p.K1 = def.K1
	}
	if p.B < 0 || p.B > 1 {
		p.B = def.B
	}
	return p
}

func maxFields(docs []Document) int {
	m := 0
	for _, d := range docs {
		if len(d.Fields) > m {
			m = len(d.Fields)
		}
	}
	return m
}
