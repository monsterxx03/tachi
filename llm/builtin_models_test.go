package llm

import (
	"fmt"
	"testing"
)

// TestBuiltinModelOrder pins the registry's ordering contract: every record's
// match terms (combined with its require conditions) must resolve back to
// that exact record via lookup. If a family record were wrongly placed before
// its variant, lookup would hit the family first and this test fails — so
// adding a variant always means placing it before its family's record.
//
// It also guards the "记录自包含" rule indirectly: a record whose match term
// is shadowed (e.g. a variant that forgets to carry context) would still
// resolve to itself, but the per-query tests (ModelContextWindow /
// ModelSupportsVision / GetBuiltinModelPriceAt) pin the field values.
func TestBuiltinModelOrder(t *testing.T) {
	for i := range builtinModels {
		b := &builtinModels[i]
		for _, s := range b.match {
			// Construct a name this record must match: the term itself, plus
			// the require conditions appended (e.g. qwen + vl → "qwen-vl").
			name := s
			for _, r := range b.require {
				name += "-" + r
			}
			if got := lookup(name); got != b {
				t.Errorf("lookup(%q) = %v, want record #%d %v; 变体记录必须排在家族记录之前",
					name, describe(got), i, describe(b))
			}
		}
	}
}

func describe(b *builtinModel) string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{match:%v require:%v context:%d vision:%v prices:%d}",
		b.match, b.require, b.context, b.vision, len(b.prices))
}
