package llm

import (
	"time"

	"github.com/monsterxx03/tachi/config"
)

// JevProviderName is the service written on every Jev ledger row. Jev is not a
// provider and has no ProviderConfig, so there is no config name to write — and
// a type name would be an implementation detail rather than an identity (see
// RecordingProvider.record). The vendor is what a reader groups these rows by.
const JevProviderName = "typesafe"

// RecordJevUsage writes the ledger row for one Jev evaluation.
//
// Jev is not an llm.Provider, so the decorator that bills providers never sees
// these calls: whichever gate made the call writes the row itself, under
// UsageKindJev, priced through the same resolution a provider's row goes
// through — the jev block's `pricing:` override when it sets one, the built-in
// table's Jev record otherwise (llm.ResolveModelPriceFrom owns that order).
//
// jevCfg is passed as a value read by the CALLER at the moment of the call,
// because the row carries a price snapshot and that snapshot has to be the
// price in force then. usdToCNY is the same kind of reading: Jev's price is
// quoted in USD, so the row's CNY snapshot depends on the rate configured at
// that moment (pass USDToCNYRate(cfg)). input and output are what the response
// reported: Jev bills input only, and the output count is recorded because a
// reader expects a whole row, not because it costs anything.
//
// A nil recorder is a no-op rather than an error — a gate must never fail
// because the ledger is unwritable, and a process without a ledger is a valid
// installation.
func RecordJevUsage(rec *UsageRecorder, jevCfg config.JevConfig, usdToCNY float64, model, sessionID string, input, output int, now time.Time) error {
	if rec == nil {
		return nil
	}
	rp := ResolveModelPriceFrom(jevCfg.Pricing, usdToCNY, JevProviderName, model, now)
	cacheReadPrice, cacheCreationPrice := CacheReadCreationPrice(&rp.Price)

	in, out := int64(input), int64(output)
	var creditRate float64
	if jevCfg.CreditRate != nil {
		creditRate = *jevCfg.CreditRate
	}
	credit := ComputeCredit(in+out, creditRate)

	return rec.Record(UsageRow{
		TS:                 now,
		SessionID:          sessionID,
		Kind:               UsageKindJev,
		Provider:           JevProviderName,
		Model:              model,
		InputTokens:        in,
		OutputTokens:       out,
		InputPrice:         rp.Price.InputPrice,
		OutputPrice:        rp.Price.OutputPrice,
		CacheReadPrice:     cacheReadPrice,
		CacheCreationPrice: cacheCreationPrice,
		Band:               rp.Band,
		// Always a snapshot, 0 included: nil Credit only ever means "written
		// before credits existed", which these rows are not (see UsageRow.Credit).
		Credit: &credit,
	})
}
