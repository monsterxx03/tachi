package llm

import (
	"reflect"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// testSource is the schedule's provenance label. Resolution ignores it (it
// only attributes warnings), so tests pass a constant rather than pretending
// to be a provider.
const testSource = "test"

// ResolveModelPriceFrom is the entry point for a caller whose pricing does not
// hang off a ProviderConfig entry — a ledger row for a call that went through
// something other than an llm.Provider. These tests pin what makes it usable
// there, and the equivalence that keeps the provider-shaped entry point
// honest.
//
// The resolution matrix itself (bands, days, timezones, inheritance) is
// covered by the TestResolveModelPriceAt_* cases, which all reach this function
// by delegation — so a behaviour change here shows up there too.

// The whole point of the raw entry point: overrides belonging to no provider
// still resolve, so a caller holding nothing but a pricing block can price a
// call.
func TestResolveModelPriceFromAppliesAScheduleWithNoProviderBehindIt(t *testing.T) {
	schedule := &config.PricingConfig{
		InputPrice:  f64(2.5),
		OutputPrice: f64(0),
	}

	got := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, testSource, "not-in-the-builtin-table", atHour(10))
	if got.Price.InputPrice != 2.5 {
		t.Errorf("InputPrice = %v, want 2.5", got.Price.InputPrice)
	}
	if !got.HasPrice() {
		t.Error("HasPrice = false, want true — an input price is set")
	}
}

// A priced row whose OUTPUT side is explicitly free must not read as unpriced.
// UsageRow.Unpriced needs both fields zero, which is exactly why a
// billed-on-input-only service still produces rows the ledger counts as priced.
func TestResolveModelPriceFromFreeOutputIsStillPriced(t *testing.T) {
	got := ResolveModelPriceFrom(&config.PricingConfig{InputPrice: f64(1.0), OutputPrice: f64(0)}, DefaultUSDToCNY, testSource, "m", atHour(10))

	row := &UsageRow{
		InputTokens:  1000,
		OutputTokens: 500,
		InputPrice:   got.Price.InputPrice,
		OutputPrice:  got.Price.OutputPrice,
	}
	if row.Unpriced() {
		t.Error("Unpriced = true, want false — only the input side is billed, the output side is free")
	}
	if cost := row.Cost(); cost <= 0 {
		t.Errorf("Cost = %v, want the input tokens billed", cost)
	}
}

// A nil schedule is the "built-in only" case: known models resolve, unknown
// ones come back unpriced rather than guessing.
func TestResolveModelPriceFromNilScheduleUsesTheBuiltinTable(t *testing.T) {
	t.Run("a model in the built-in table", func(t *testing.T) {
		got := ResolveModelPriceFrom(nil, DefaultUSDToCNY, testSource, "deepseek-chat", atHour(10))
		if !got.HasPrice() {
			t.Errorf("HasPrice = false for a built-in model: %+v", got)
		}
	})

	t.Run("a model nobody has priced", func(t *testing.T) {
		got := ResolveModelPriceFrom(nil, DefaultUSDToCNY, testSource, "not-in-the-builtin-table", atHour(10))
		if got.HasPrice() {
			t.Errorf("HasPrice = true, want an unpriced resolution: %+v", got)
		}
	})
}

// The bands-only path is the one that must not silently drop input: an unknown
// model with bands and no flat prices still gets its bands, with the inherited
// fields left at 0.
func TestResolveModelPriceFromBandsSurviveAnUnknownModel(t *testing.T) {
	schedule := &config.PricingConfig{
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(7.0)},
		},
	}

	in := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, testSource, "not-in-the-builtin-table", atHour(10))
	if in.Price.InputPrice != 7.0 || in.Band != "peak" {
		t.Errorf("in-band = %+v (band %q), want 7.0 + peak", in.Price, in.Band)
	}
	out := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, testSource, "not-in-the-builtin-table", atHour(13))
	if out.Price.InputPrice != 0 || out.Band != "" {
		t.Errorf("off-band = %+v (band %q), want the zero flat price", out.Price, out.Band)
	}
}

// Resolution is applied to a COPY of the model's price entry, so the same
// question asked twice must give the same answer. A regression here —
// appending bands onto shared state instead of onto a copy — would show up
// only as a price that drifts the longer a process runs.
func TestResolveModelPriceFromIsRepeatable(t *testing.T) {
	schedule := &config.PricingConfig{
		InputPrice: f64(1.0),
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(7.0)},
		},
	}

	// A built-in model is the interesting case: it arrives with its own bands
	// and location, which the schedule's bands must replace wholesale without
	// touching the table entry they came from.
	first := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, testSource, "deepseek-chat", atHour(10))
	for i := 2; i <= 4; i++ {
		again := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, testSource, "deepseek-chat", atHour(10))
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("call %d = %+v, want %+v — resolution is accumulating state", i, again, first)
		}
	}

	// The table entry itself must be untouched: ask it directly after the
	// resolutions above and compare against a fresh lookup of another model's
	// untouched sibling — a mutated entry would carry the schedule's 7.0.
	if entry := GetBuiltinModelPriceAt("deepseek-chat", atHour(10), DefaultUSDToCNY); entry != nil {
		for _, b := range entry.Bands {
			if b.InputPrice == 7.0 {
				t.Errorf("built-in entry carries the schedule's band — the shared table was mutated: %+v", b)
			}
		}
	}
}

// ResolveModelPriceAt must stay nothing but "look up the provider's schedule,
// then resolve it". If the two ever diverge, a provider-priced row and a
// directly-priced row would disagree about the same schedule — the kind of
// drift that stays invisible until a bill is wrong.
func TestResolveModelPriceAtIsResolveModelPriceFromOfTheProviderSchedule(t *testing.T) {
	schedule := &config.PricingConfig{
		InputPrice:  f64(1.0),
		OutputPrice: f64(2.0),
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(3.0)},
		},
	}
	cfg := cfgWithPricing(schedule)

	for _, tc := range []struct {
		name string
		t    time.Time
	}{
		{name: "in band", t: atHour(10)},
		{name: "out of band", t: atHour(13)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := ResolveModelPriceFrom(schedule, DefaultUSDToCNY, "custom", "some-model", tc.t)
			got := ResolveModelPriceAt(cfg, "custom", "some-model", tc.t)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolveModelPriceAt = %+v, want %+v (the provider's own schedule, resolved once)", got, want)
			}
		})
	}

	t.Run("unknown provider falls back to a nil schedule", func(t *testing.T) {
		want := ResolveModelPriceFrom(nil, DefaultUSDToCNY, testSource, "some-model", atHour(10))
		got := ResolveModelPriceAt(cfg, "no-such-provider", "some-model", atHour(10))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveModelPriceAt = %+v, want the built-in-only resolution %+v", got, want)
		}
	})

	t.Run("nil config", func(t *testing.T) {
		want := ResolveModelPriceFrom(nil, DefaultUSDToCNY, testSource, "some-model", atHour(10))
		got := ResolveModelPriceAt(nil, "custom", "some-model", atHour(10))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveModelPriceAt(nil cfg) = %+v, want the built-in-only resolution %+v", got, want)
		}
	})
}
