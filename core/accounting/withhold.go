package accounting

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
)

// The reasons Host.Savings books no saving for a request (Extras.Withheld,
// event.Event.SavingsWithheld, the reason label of llm.savings.withheld). They are the library's
// names: accountant.Withhold decides the gates (accountant design §6.7, docs/cortex-wiring.md
// W6.5), so the ledger and the library's metrics sink withhold the same requests.
const (
	WithheldCheckSum          = accountant.WithheldCheckSum
	WithheldCheckUniqueChange = accountant.WithheldCheckUniqueChange
	WithheldCheckSpans        = accountant.WithheldCheckSpans
	WithheldCheckOther        = accountant.WithheldCheckOther
	WithheldUnpriced          = accountant.WithheldUnpriced
	WithheldSizeUnknown       = accountant.WithheldSizeUnknown
	WithheldImplausibleSize   = accountant.WithheldImplausibleSize
)

// withholdSavings is what Host.Savings books for a request that failed gate why: no saving, no
// UnattributedBreakUSD and no InteractionUSD, as the library's metrics sink books none of them
// (accountant design §6.7). The rest of ex stays: its spends, each priced from its own call.
func withholdSavings(ex Extras, why string) ([]event.Saving, Extras) {
	ex.UnattributedBreakUSD, ex.InteractionUSD, ex.Withheld = 0, 0, why
	return nil, ex
}

// penaltyUnknown reports whether res lists a penalty_unknown op: its figure stands but is an
// estimate (accountant design §6.4).
func penaltyUnknown(res accountant.Result) bool {
	for _, u := range res.Unpriced {
		if u.Reason == accountant.ReasonPenaltyUnknown {
			return true
		}
	}
	return false
}

// tokensBefore is Cortex's measure of the prompt size, in tokens, of the body the client sent,
// the size accountant.Withhold bounds the saving by. The library cannot derive it (design §6.7).
// It is the billed prompt of the body the provider bills (pctx.BilledBody, forwardedBytes —
// the bytes sent, unless a plugin's hook gave a smaller stub-sized copy), scaled by the two
// bodies' sizes in bytes. A forwarded body larger than the client's (an operator that added
// bytes) gives the billed prompt itself, so the bound is never tighter than what the provider
// billed. 0 when the size cannot be known.
func tokensBefore(promptTotal, clientBytes, forwardedBytes int) int {
	if promptTotal <= 0 || forwardedBytes <= 0 {
		return 0
	}
	if clientBytes <= forwardedBytes {
		return promptTotal
	}
	return int(int64(promptTotal) * int64(clientBytes) / int64(forwardedBytes))
}
