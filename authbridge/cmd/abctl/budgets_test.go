package main

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// Every per-call budget in this binary has to fit UNDER the client's header backstop, or the
// backstop silently caps it and the budget is decoration.
//
// THE TRAP IS THAT "WAITING FOR HEADERS" INCLUDES THE SERVER'S WORK. net/http starts that
// clock at the request and stops it at the first response header, and nothing obliges a
// handler to write headers before it computes — /v1/usage does not: it builds the whole
// snapshot, walking up to eight day files for a symbolic window, and writes headers
// afterwards. So a header bound is a ceiling on the server's think time, which for this
// command is exactly the slow read costFetchTimeout is generous for.
//
// This is the second time that relationship was got wrong. The first was a fixed 10s
// http.Client.Timeout; removing it and replacing it with a 10s header bound reinstated the
// same 10s cap by a different mechanism, and no test noticed because the behavioural tests
// shorten their own seams and sleep for milliseconds — 40x under the real bound. A comparison
// of the two constants cannot miss it.
//
// A TABLE, so adding a budget means adding a line here. The alternative is remembering, and
// the evidence is that remembering did not work.
func TestCallerBudgets_FitUnderTheHeaderBackstop(t *testing.T) {
	for _, b := range []struct {
		name   string
		budget time.Duration
	}{
		// The only budget in this binary that exceeds apiclient's own deadline-less default,
		// and the one the defect bit.
		{name: "abctl cost (costFetchTimeout)", budget: costFetchTimeout},
		{name: "abctl pipeline get (pipelineFetchTimeout)", budget: pipelineFetchTimeout},
	} {
		t.Run(b.name, func(t *testing.T) {
			if b.budget >= apiclient.HeaderTimeout {
				t.Errorf("%s budgets %v against a %v header backstop: the backstop is a ceiling "+
					"on the server's think time, so this budget cannot be reached and the reason "+
					"it is generous — a slow ledger scan — is exactly what gets capped",
					b.name, b.budget, apiclient.HeaderTimeout)
			}
			// And not merely under it: a budget within a factor of two of the backstop is a
			// budget one slow mount away from being capped. Stated as a rule rather than left
			// for the next person to rediscover at 10s.
			if b.budget*2 >= apiclient.HeaderTimeout {
				t.Errorf("%s budgets %v against a %v backstop — under it, but with less than 2x "+
					"headroom; raise apiclient.HeaderTimeout", b.name, b.budget, apiclient.HeaderTimeout)
			}
		})
	}
}
