// Package accounting is Cortex's own host for github.com/rossoctl/accountant: it computes
// context-savings figures with the same rules the context-guru proxy uses, following the host
// obligations in accountant/docs/cortex-wiring.md W6.
//
// This file's package holds the leaf host code with no caller yet (design §5.2, task 1 of the
// accountant integration plan):
//
//   - wire: splits an inference request body into the sections and breakpoints accountant.NewBody
//     wants (W6.3).
//   - facts: classifies the destination provider and gives the cache facts (accountant.EntryFacts)
//     for a provider and model (W6, cached-prefix.md).
//   - skip.go: SkipReason says when not to call Compute, and why (W6.1).
//   - thread.go: ThreadOf gives the accountant.Thread for a request (W5.2).
//
// core/accounting imports only the standard library, github.com/rossoctl/accountant, OTel, and
// github.com/rossoctl/cortex/core/... packages — never github.com/rossoctl/context-guru/...
// (global-constraints.md; boundary_test.go enforces it).
package accounting
