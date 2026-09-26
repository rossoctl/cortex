package edit

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReloadStatus is the wire shape of the framework's /reload/status endpoint.
// Only the fields abctl uses are decoded. Keys must match
// core/reloader/status.go exactly.
type ReloadStatus struct {
	LastSuccess   time.Time `json:"last_success"`
	ReloadsOK     int64     `json:"reloads_ok"`
	ReloadsFailed int64     `json:"reloads_failed"`
	LastError     string    `json:"last_error"`
}

// PollResultStatus is a sum type for PollUntilReloaded outcomes.
type PollResultStatus int

const (
	PollUnknown PollResultStatus = iota
	PollSuccess
	PollFailure
	PollTimeout
)

// PollResult is what PollUntilReloaded returns.
type PollResult struct {
	Status    PollResultStatus
	LastError string // populated when Status == PollFailure
}

// baselineFailedSentinel marks the "baseline not yet captured" state for
// the reloads_failed counter. The first successful poll snaps it to the
// current value so we only react to NEW failures after our apply.
const baselineFailedSentinel int64 = -1

// pollInterval is the cadence between /reload/status fetches. 1s balances
// user-visible spinner progress with not hammering the cluster on slow
// reloads.
const pollInterval = 1 * time.Second

// pollMaxBackoff caps the exponential backoff applied to consecutive
// transport errors. Keeps a flapping endpoint from getting hammered
// without making the user wait too long once it recovers.
const pollMaxBackoff = 5 * time.Second

// unreachableThreshold is the number of consecutive transport-layer
// failures (network errors, non-200) after which the poller gives up
// rather than waiting for the full deadline. Picked so a normal kubelet
// hiccup (one or two failed polls) is tolerated, but a port-forward
// drop or framework crash surfaces fast.
const unreachableThreshold = 5

// defaultUnreachableHint is used when a caller has no Target to draw on (a
// test, or a degraded path with no store). Names neither backend's mechanism.
const defaultUnreachableHint = "is the endpoint still up?"

// PollUntilReloaded watches statusURL/reload/status until either:
//   - LastSuccess > applyTime → PollSuccess.
//   - ReloadsFailed exceeds the value at first successful poll → PollFailure
//     with LastError populated.
//   - ctx is done → PollTimeout. (Caller is expected to set a 120s timeout
//     via context.WithTimeout.)
//   - 5 consecutive transport errors → PollFailure with a synthesized
//     "reload status unreachable" LastError, so a port-forward drop or
//     crashed framework surfaces fast instead of silently sitting through
//     the whole deadline.
//
// Backoff: poll at pollInterval, doubled on each transport error up to
// pollMaxBackoff. Reset to pollInterval on the next successful response.
// unreachableHint completes the synthesized "unreachable" error; empty falls
// back to defaultUnreachableHint. It comes from the Store's Target, because
// what to check differs by backend: a cluster edit rides a port-forward that
// can drop, a local one talks straight to a proxy that can have exited.
func PollUntilReloaded(ctx context.Context, statusURL string, applyTime time.Time, unreachableHint string) PollResult {
	url := statusURL + "/reload/status"
	client := &http.Client{Timeout: 2 * time.Second}
	if unreachableHint == "" {
		unreachableHint = defaultUnreachableHint
	}

	baselineFailed := baselineFailedSentinel
	consecErrors := 0
	wait := pollInterval

	for {
		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return PollResult{Status: PollTimeout}
		}
		resp, err := client.Do(req)
		ok := err == nil && resp.StatusCode == 200
		if ok {
			var rs ReloadStatus
			decodeErr := json.NewDecoder(resp.Body).Decode(&rs)
			resp.Body.Close()
			if decodeErr == nil {
				consecErrors = 0
				wait = pollInterval
				if baselineFailed == baselineFailedSentinel {
					baselineFailed = rs.ReloadsFailed
				}
				if rs.LastSuccess.After(applyTime) {
					return PollResult{Status: PollSuccess}
				}
				if rs.ReloadsFailed > baselineFailed {
					return PollResult{Status: PollFailure, LastError: rs.LastError}
				}
			}
		} else {
			if resp != nil {
				resp.Body.Close()
			}
			consecErrors++
			if consecErrors >= unreachableThreshold {
				return PollResult{
					Status:    PollFailure,
					LastError: "reload status endpoint unreachable (" + unreachableHint + ")",
				}
			}
			if wait < pollMaxBackoff {
				wait *= 2
				if wait > pollMaxBackoff {
					wait = pollMaxBackoff
				}
			}
		}

		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		case <-time.After(wait):
		}
	}
}
