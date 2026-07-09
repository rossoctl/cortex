// Package litellm_budgettrack provides an inbound pipeline plugin that tracks
// per-request cost via the x-litellm-response-cost response header and
// enforces a daily spending budget, rejecting requests with HTTP 429
// when the budget is exceeded.
package litellm_budgettrack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

type budgetTrackConfig struct {
	SpendFile string  `json:"spend_file" required:"true" description:"Path to the JSON spend ledger file."`
	MaxBudget float64 `json:"max_budget" required:"true" description:"Daily budget in USD."`
}

type spendLedger struct {
	Date       string  `json:"date"`
	TotalSpend float64 `json:"total_spend"`
	TotalCalls int     `json:"total_calls"`
}

// BudgetTrack enforces a daily spending budget based on x-litellm-response-cost.
type BudgetTrack struct {
	cfg    budgetTrackConfig
	mu     sync.Mutex
	ledger spendLedger
}

// New creates an unconfigured BudgetTrack plugin instance.
func New() *BudgetTrack { return &BudgetTrack{} }

func init() {
	plugins.RegisterPlugin("litellm-budget-track", func() pipeline.Plugin { return New() })
}

func (p *BudgetTrack) Name() string { return "litellm-budget-track" }

func (p *BudgetTrack) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Track x-litellm-response-cost and enforce daily budget limit.",
	}
}

func (p *BudgetTrack) Configure(raw json.RawMessage) error {
	if err := json.Unmarshal(raw, &p.cfg); err != nil {
		return fmt.Errorf("litellm-budget-track config: %w", err)
	}
	if p.cfg.SpendFile == "" {
		return fmt.Errorf("litellm-budget-track: spend_file is required")
	}
	if p.cfg.MaxBudget <= 0 {
		return fmt.Errorf("litellm-budget-track: max_budget must be > 0")
	}
	p.loadLedger()
	return nil
}

// OnRequest checks if the daily budget has been exceeded before allowing the request.
func (p *BudgetTrack) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.resetIfNewDay()
	spend := p.ledger.TotalSpend
	p.mu.Unlock()

	if spend >= p.cfg.MaxBudget {
		return pipeline.DenyStatus(429, "budget.exceeded",
			fmt.Sprintf("Rossocortex ExceededTokenBudget: daily spend $%.4f exceeds budget $%.2f. Reset at midnight UTC.", spend, p.cfg.MaxBudget))
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse reads x-litellm-response-cost and accumulates the spend.
func (p *BudgetTrack) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	costStr := pctx.Headers.Get("X-Litellm-Response-Cost")
	if costStr == "" {
		return pipeline.Action{Type: pipeline.Continue}
	}
	cost, err := strconv.ParseFloat(costStr, 64)
	if err != nil || cost <= 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}

	p.mu.Lock()
	p.resetIfNewDay()
	p.ledger.TotalSpend += cost
	p.ledger.TotalCalls++
	p.saveLedger()
	p.mu.Unlock()

	return pipeline.Action{Type: pipeline.Continue}
}

func (p *BudgetTrack) todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

func (p *BudgetTrack) resetIfNewDay() {
	today := p.todayUTC()
	if p.ledger.Date != today {
		p.ledger = spendLedger{Date: today}
	}
}

func (p *BudgetTrack) loadLedger() {
	data, err := os.ReadFile(p.cfg.SpendFile)
	if err != nil {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	var l spendLedger
	if json.Unmarshal(data, &l) != nil || l.Date != p.todayUTC() {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	p.ledger = l
}

func (p *BudgetTrack) saveLedger() {
	data, _ := json.MarshalIndent(p.ledger, "", "  ")
	_ = os.WriteFile(p.cfg.SpendFile, data, 0644)
}

var (
	_ pipeline.Plugin       = (*BudgetTrack)(nil)
	_ pipeline.Configurable = (*BudgetTrack)(nil)
)
