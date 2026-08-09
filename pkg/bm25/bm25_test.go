package bm25

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doc builds a single-field document (the common case) from tokens.
func doc(tokens ...string) Document {
	return Document{Fields: []Field{{Tokens: tokens}}}
}

// docs builds a multi-field document: field i gets tokens[i] with boost
// boosts[i] (0 = default).
func fdoc(boosts []float64, tokens ...[]string) Document {
	d := Document{}
	for i, toks := range tokens {
		b := 0.0
		if i < len(boosts) {
			b = boosts[i]
		}
		d.Fields = append(d.Fields, Field{Boost: b, Tokens: toks})
	}
	return d
}

func TestNew_EmptyCorpus(t *testing.T) {
	ix := New(nil, DefaultParams())
	assert.Equal(t, 0, ix.Len())
	assert.Empty(t, ix.Scores([]string{"x"}))
	assert.Nil(t, ix.TopN([]string{"x"}, 5))
	assert.Equal(t, 0.0, ix.Score([]string{"x"}, 0))
	assert.Equal(t, 0.0, ix.IDF("x"))
}

func TestScore_NoMatch(t *testing.T) {
	ix := New([]Document{doc("hello", "world")}, DefaultParams())
	assert.Equal(t, 0.0, ix.Score([]string{"absent"}, 0))
	assert.Equal(t, 0.0, ix.Score([]string{""}, 0))
	assert.Equal(t, 0.0, ix.Score(nil, 0))
	assert.Equal(t, 0.0, ix.Score([]string{"hello"}, 42)) // out of range
}

func TestScore_TermFrequency(t *testing.T) {
	// Doc A mentions "query" twice, doc B once — A must score higher, and
	// A's score must be strictly greater than B's (saturated but still
	// monotonic in tf for k1=1.2).
	ix := New([]Document{
		doc("query", "query", "database"),
		doc("query", "database"),
	}, DefaultParams())

	sa := ix.Score([]string{"query"}, 0)
	sb := ix.Score([]string{"query"}, 1)
	assert.Greater(t, sa, 0.0)
	assert.Greater(t, sa, sb)
	assert.Greater(t, sb, 0.0)
}

func TestScore_IDF_RareTermDominates(t *testing.T) {
	// "candlestick" appears in only one of three docs — its IDF must make
	// that doc outrank a doc matched only by the common term "data".
	ix := New([]Document{
		doc("data", "data", "data", "data", "data", "data"),
		doc("data", "data", "data", "data", "data", "data"),
		doc("candlestick", "data"),
	}, DefaultParams())

	sRare := ix.Score([]string{"candlestick"}, 2)
	sCommon := ix.Score([]string{"data"}, 0)
	assert.Greater(t, sRare, 0.0)
	assert.Greater(t, sRare, sCommon, "rare term should outrank high-tf common term")

	// IDF of a corpus-wide term is smaller than IDF of a rare term.
	idfRare := ix.IDF("candlestick")
	idfCommon := ix.IDF("data")
	assert.Greater(t, idfRare, idfCommon)
}

func TestScore_LengthNormalization(t *testing.T) {
	// Two docs with identical relevant content; the padded one must score
	// lower (b > 0 penalizes length).
	ix := New([]Document{
		doc("query", "query"),
		doc("query", "query", "filler", "filler", "filler", "filler", "filler", "filler"),
	}, DefaultParams())

	sShort := ix.Score([]string{"query"}, 0)
	sLong := ix.Score([]string{"query"}, 1)
	assert.Greater(t, sShort, sLong, "length normalization must penalize the padded doc")

	// With b=0 the length penalty disappears and both docs tie.
	ixNoNorm := New([]Document{
		doc("query", "query"),
		doc("query", "query", "filler", "filler", "filler", "filler", "filler", "filler"),
	}, Params{K1: 1.2, B: 0})
	assert.InDelta(t, ixNoNorm.Score([]string{"query"}, 0), ixNoNorm.Score([]string{"query"}, 1), 1e-9)
}

func TestScore_FieldBoost(t *testing.T) {
	// Field 0 (name) boosted 10x, field 1 (description) default.
	// Doc A matches in the boosted field, doc B in the plain field — A must
	// outrank B even though B's description mentions the term more.
	ix := New([]Document{
		fdoc([]float64{10, 1}, []string{"query"}, []string{"unrelated"}),
		fdoc([]float64{10, 1}, []string{"other"}, []string{"query", "query", "query", "query"}),
	}, DefaultParams())

	sBoosted := ix.Score([]string{"query"}, 0)
	sPlain := ix.Score([]string{"query"}, 1)
	assert.Greater(t, sBoosted, 0.0)
	assert.Greater(t, sPlain, 0.0)
	assert.Greater(t, sBoosted, sPlain, "boosted field match must dominate")

	// Equal boost everywhere → symmetric behavior: doc B wins on tf.
	ixFlat := New([]Document{
		fdoc(nil, []string{"query"}, []string{"unrelated"}),
		fdoc(nil, []string{"other"}, []string{"query", "query", "query", "query"}),
	}, DefaultParams())
	assert.Greater(t, ixFlat.Score([]string{"query"}, 1), ixFlat.Score([]string{"query"}, 0))
}

func TestScore_ZeroBoostUsesDefault(t *testing.T) {
	// Boost 0 must behave exactly like the default 1.0.
	ixDefault := New([]Document{
		fdoc([]float64{0}, []string{"query"}),
	}, DefaultParams())
	ixExplicit := New([]Document{
		fdoc([]float64{1}, []string{"query"}),
	}, DefaultParams())
	assert.InDelta(t, ixDefault.Score([]string{"query"}, 0), ixExplicit.Score([]string{"query"}, 0), 1e-9)
}

func TestTopN_OrderAndLimit(t *testing.T) {
	ix := New([]Document{
		doc("query", "database"),
		doc("query", "query", "query"), // highest
		doc("unrelated"),
	}, DefaultParams())

	top := ix.TopN([]string{"query"}, 2)
	require.Len(t, top, 2)
	assert.Equal(t, 1, top[0].Doc)
	assert.Greater(t, top[0].Score, top[1].Score)
	assert.Equal(t, 0, top[1].Doc)

	// n > matches returns all matches, n <= 0 returns nil.
	assert.Len(t, ix.TopN([]string{"query"}, 10), 2)
	assert.Nil(t, ix.TopN([]string{"query"}, 0))
	assert.Nil(t, ix.TopN([]string{"query"}, -1))
}

func TestTopN_DeterministicTieBreak(t *testing.T) {
	// Two docs score identically → document order breaks the tie.
	ix := New([]Document{
		doc("query"),
		doc("query"),
	}, DefaultParams())
	top := ix.TopN([]string{"query"}, 2)
	require.Len(t, top, 2)
	assert.Equal(t, 0, top[0].Doc)
	assert.Equal(t, 1, top[1].Doc)
	assert.InDelta(t, top[0].Score, top[1].Score, 1e-9)
}

func TestTopN_ZeroScoresExcluded(t *testing.T) {
	ix := New([]Document{
		doc("query"),
		doc("unrelated"),
	}, DefaultParams())
	top := ix.TopN([]string{"query"}, 5)
	require.Len(t, top, 1)
	assert.Equal(t, 0, top[0].Doc)
}

func TestAdd_IncrementalMatchesBulk(t *testing.T) {
	bulk := New([]Document{
		doc("query", "database"),
		doc("other"),
	}, DefaultParams())

	inc := New(nil, DefaultParams())
	inc.Add(doc("query", "database"))
	inc.Add(doc("other"))

	assert.Equal(t, bulk.Len(), inc.Len())
	assert.InDelta(t, bulk.Score([]string{"query"}, 0), inc.Score([]string{"query"}, 0), 1e-9)
	assert.InDelta(t, bulk.Score([]string{"other"}, 1), inc.Score([]string{"other"}, 1), 1e-9)
	assert.InDelta(t, bulk.IDF("query"), inc.IDF("query"), 1e-9)
	assert.InDelta(t, bulk.IDF("other"), inc.IDF("other"), 1e-9)

	// After a further Add, df and avg update: "fresh" is now in 1 of 3 docs.
	inc.Add(doc("fresh"))
	assert.Equal(t, 3, inc.Len())
	assert.InDelta(t, inc.IDF("fresh"), inc.IDF("query"), 1e-9, "both terms appear in exactly 1 of 3 docs")
}

func TestAdd_AfterBulkAvgUpdated(t *testing.T) {
	ix := New([]Document{
		doc("a", "b"),
		doc("c"),
	}, DefaultParams())
	ix.Add(doc("d", "e", "f"))

	// IDF of "d" (1 of 3 docs) must exceed IDF of "a" (1 of 3 docs) — equal
	// corpus-wide; instead check both are positive and distinct query terms
	// on the new doc score.
	assert.Greater(t, ix.Score([]string{"d"}, 2), 0.0)
	assert.Greater(t, ix.Score([]string{"a"}, 0), 0.0)
}

// TestAdd_ExpandsFieldCount pins the fix where adding a multi-field document
// to an index that started empty must grow the field count (previously the
// new fields were silently dropped, scoring 0).
func TestAdd_ExpandsFieldCount(t *testing.T) {
	ix := New(nil, DefaultParams())
	ix.Add(fdoc([]float64{5}, []string{"name-term"}, []string{"desc-term"}))

	assert.Equal(t, 1, ix.Len())
	assert.Greater(t, ix.Score([]string{"name-term"}, 0), 0.0, "field 0 indexed after grow")
	assert.Greater(t, ix.Score([]string{"desc-term"}, 0), 0.0, "field 1 indexed after grow")

	// A second, single-field doc must still score in its only field, and the
	// original doc's scores are unchanged after the grow.
	ix.Add(doc("plain"))
	assert.Greater(t, ix.Score([]string{"plain"}, 1), 0.0)
	assert.Greater(t, ix.Score([]string{"name-term"}, 0), 0.0, "field 0 score unchanged after grow")
	assert.Greater(t, ix.Score([]string{"desc-term"}, 0), 0.0, "field 1 score unchanged after grow")
	// The new doc lacks field 1, so it must dilute that field's average
	// length (length 1 over 2 docs → 0.5).
	assert.InDelta(t, 0.5, ix.avgFieldLen[1], 1e-9)
}

// TestAdd_OrderIndependence pins the fix where avgFieldLen depended on
// insertion order: a document missing a field must still dilute that field's
// average length (contributing 0), so the same corpus scores identically
// regardless of Add order or bulk New.
func TestAdd_OrderIndependence(t *testing.T) {
	withField1 := Document{Fields: []Field{
		{Tokens: []string{"b"}},
		{Tokens: []string{"c"}},
	}}
	withoutField1 := Document{Fields: []Field{{Tokens: []string{"a"}}}}

	build := func(ordered []Document) *Index {
		ix := New(nil, DefaultParams())
		for _, d := range ordered {
			ix.Add(d)
		}
		return ix
	}

	first := build([]Document{withoutField1, withField1})
	second := build([]Document{withField1, withoutField1})
	bulk := New([]Document{withoutField1, withField1}, DefaultParams())

	// field 1 is introduced by the second document (length 1); the first
	// document predates it, so avg = 1/2. Missing fields AFTER introduction
	// contribute 0 (dilution, see TestAdd_ExpandsFieldCount).
	assert.InDelta(t, 0.5, first.avgFieldLen[1], 1e-9)
	assert.InDelta(t, first.avgFieldLen[1], second.avgFieldLen[1], 1e-9)

	for _, q := range [][]string{{"a"}, {"b"}, {"c"}} {
		assert.InDelta(t, first.Score(q, 1), second.Score(q, 0), 1e-9, "Add order must not change scores")
		assert.InDelta(t, first.Score(q, 1), bulk.Score(q, 1), 1e-9, "incremental Add must match bulk New")
	}
	assert.InDelta(t, first.IDF("c"), bulk.IDF("c"), 1e-9)
}

func TestScore_DuplicateQueryTerms(t *testing.T) {
	// Duplicate query terms are not deduplicated — each occurrence adds to
	// the score, so callers should dedup (Lucene BooleanQuery semantics).
	// This test pins that behavior so it can't silently change.
	ix := New([]Document{doc("query")}, DefaultParams())
	s1 := ix.Score([]string{"query"}, 0)
	s2 := ix.Score([]string{"query", "query"}, 0)
	assert.InDelta(t, 2*s1, s2, 1e-9)
}

func TestScores_OrderedByDoc(t *testing.T) {
	ix := New([]Document{
		doc("query"),
		doc("other"),
	}, DefaultParams())
	scores := ix.Scores([]string{"query"})
	require.Len(t, scores, 2)
	assert.Greater(t, scores[0], 0.0)
	assert.Equal(t, 0.0, scores[1])
}

func TestDefaultParams_Values(t *testing.T) {
	p := DefaultParams()
	assert.InDelta(t, 1.2, p.K1, 1e-9)
	assert.InDelta(t, 0.75, p.B, 1e-9)
}

func TestZeroParams_FallBackToDefaults(t *testing.T) {
	// Params zero value must behave like DefaultParams (both indices score
	// identically).
	a := New([]Document{doc("query", "query", "query", "query")}, Params{})
	b := New([]Document{doc("query", "query", "query", "query")}, DefaultParams())
	assert.InDelta(t, a.Score([]string{"query"}, 0), b.Score([]string{"query"}, 0), 1e-9)
}

func TestScore_EmptyFieldNoPanic(t *testing.T) {
	// Document with no fields at all must not panic and score 0.
	ix := New([]Document{
		{},
		doc("query"),
	}, DefaultParams())
	assert.Equal(t, 0.0, ix.Score([]string{"query"}, 0))
	assert.Greater(t, ix.Score([]string{"query"}, 1), 0.0)
}

func TestScore_MultiTermQueryAggregates(t *testing.T) {
	// Query ["query", "database"] on a doc containing both must score higher
	// than on a doc containing only one of them.
	ix := New([]Document{
		doc("query", "database"),
		doc("query"),
		doc("database"),
	}, DefaultParams())

	sBoth := ix.Score([]string{"query", "database"}, 0)
	sQ := ix.Score([]string{"query", "database"}, 1)
	sD := ix.Score([]string{"query", "database"}, 2)
	assert.Greater(t, sBoth, sQ)
	assert.Greater(t, sBoth, sD)
}

func TestIDF_AllDocsTermLowButPositive(t *testing.T) {
	// A term in every doc gets a small but non-negative IDF (Lucene variant).
	ix := New([]Document{
		doc("x", "common"),
		doc("x", "common"),
	}, DefaultParams())
	idf := ix.IDF("common")
	assert.GreaterOrEqual(t, idf, 0.0)
	assert.Less(t, idf, 1.0, "corpus-wide term should have near-zero IDF")
	assert.Greater(t, ix.IDF("x"), 0.0)
}

// TestScore_MatchesReference ensures the formula matches the canonical Okapi
// BM25 definition on a hand-computed case:
//
// Corpus: doc0 = [a, b], doc1 = [a]
// query:  [a]
//
// df(a) = 2, N = 2 → idf(a) = ln(1 + (2-2+0.5)/(2+0.5)) = ln(1 + 0.2) = 0.1823
// avgdl  = (2 + 1)/2 = 1.5
//
// doc0: tf=1, len=2 → tf·(k1+1)/(tf + k1·(1-b+b·len/avg))
//
//	= 2.2 / (1 + 1.2·(0.25 + 0.75·2/1.5)) = 2.2 / (1 + 1.2·1.25) = 2.2/2.5 = 0.88
//
// doc1: tf=1, len=1 → 2.2 / (1 + 1.2·(0.25 + 0.75·1/1.5)) = 2.2/(1+1.2·0.75) = 2.2/1.9 = 1.1579
func TestScore_MatchesReference(t *testing.T) {
	ix := New([]Document{
		doc("a", "b"),
		doc("a"),
	}, DefaultParams())

	want0 := 0.1823 * 0.88
	want1 := 0.1823 * 1.1578947368421053
	assert.InDelta(t, want0, ix.Score([]string{"a"}, 0), 1e-3)
	assert.InDelta(t, want1, ix.Score([]string{"a"}, 1), 1e-3)
	assert.InDelta(t, math.Log(1.2), ix.IDF("a"), 1e-9)
}
