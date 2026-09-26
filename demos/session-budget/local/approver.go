// approver.go is a demo-only HITL approver for the session-budget
// plugin's `on_exceed: pause` mode. It listens on an HTTP port, prints
// each incoming pause request, prompts the operator for [a]pprove or
// [d]eny, and returns the matching JSON response.
//
// Run:
//
//	go run demos/session-budget/local/approver.go
//	go run demos/session-budget/local/approver.go --auto-approve
//	go run demos/session-budget/local/approver.go --addr 127.0.0.1:7000
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// pauseRequest mirrors the wire type in
// core/plugins/sessionbudget/plugin.go.
type pauseRequest struct {
	SessionID       string `json:"session_id"`
	Reason          string `json:"reason"`
	SpentTokens     int64  `json:"spent_tokens"`
	SpentCalls      int64  `json:"spent_calls"`
	TokenLimit      int64  `json:"token_limit"`
	CallLimit       int64  `json:"call_limit"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
	DurationLimit   int64  `json:"duration_limit,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	autoApprove := flag.Bool("auto-approve", false, "skip the prompt and always approve")
	autoDeny := flag.Bool("auto-deny", false, "skip the prompt and always deny")
	flag.Parse()

	if *autoApprove && *autoDeny {
		fmt.Fprintln(os.Stderr, "approver: --auto-approve and --auto-deny are mutually exclusive")
		os.Exit(2)
	}

	// Serialize prompts so concurrent pause requests queue rather than
	// interleave keystrokes on stdin.
	var promptMu sync.Mutex

	// One long-running reader owns os.Stdin; decide reads answers off
	// this channel. That way a pause whose request context cancels can
	// abandon its prompt without racing another decide on stdin.
	lines := make(chan string)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var pr pauseRequest
		if err := json.Unmarshal(raw, &pr); err != nil {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}

		action := decide(r.Context(), &pr, &promptMu, lines, *autoApprove, *autoDeny)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"action": action})
	})

	fmt.Printf("approver listening on %s (auto-approve=%v, auto-deny=%v)\n",
		*addr, *autoApprove, *autoDeny)
	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "approver:", err)
		os.Exit(1)
	}
}

func decide(ctx context.Context, pr *pauseRequest, mu *sync.Mutex, lines <-chan string, autoApprove, autoDeny bool) string {
	mu.Lock()
	defer mu.Unlock()

	fmt.Println()
	fmt.Println("─── pause request ───")
	// %q on caller-supplied fields so an embedded escape sequence
	// cannot rewrite the calls/tokens lines the operator decides on.
	fmt.Printf("  session: %q\n", pr.SessionID)
	fmt.Printf("  reason:  %q\n", pr.Reason)
	fmt.Printf("  calls:   %d / %d\n", pr.SpentCalls, pr.CallLimit)
	fmt.Printf("  tokens:  %d / %d\n", pr.SpentTokens, pr.TokenLimit)
	if pr.DurationLimit > 0 {
		fmt.Printf("  age:     %ds / %ds\n", pr.DurationSeconds, pr.DurationLimit)
	}

	switch {
	case autoApprove:
		fmt.Println("  → approve (auto)")
		return "approve"
	case autoDeny:
		fmt.Println("  → deny (auto)")
		return "deny"
	}

	// Drain any answer typed for a prior request whose context already
	// canceled — a stale keystroke should not decide the current one.
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				fmt.Println("  (stdin closed — failing closed to deny; use --auto-approve for unattended approvals)")
				return "deny"
			}
			continue
		default:
		}
		break
	}

	for {
		fmt.Print("  [a]pprove / [d]eny (Enter = approve): ")
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Println("  (request canceled by caller — failing closed to deny)")
			return "deny"
		case line, ok := <-lines:
			if !ok {
				fmt.Println("  (stdin closed — failing closed to deny; use --auto-approve for unattended approvals)")
				return "deny"
			}
			switch strings.TrimSpace(strings.ToLower(line)) {
			case "", "a", "approve":
				fmt.Println("  → approve")
				return "approve"
			case "d", "deny":
				fmt.Println("  → deny")
				return "deny"
			default:
				fmt.Println("  (unrecognized — type 'a' to approve or 'd' to deny)")
			}
		}
	}
}
