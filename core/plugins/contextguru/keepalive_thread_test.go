package contextguru

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	cgcomponents "github.com/rossoctl/context-guru/components"
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/pipeline"
)

// agentPctx is a finished request from the Claude Code agent named id: a subagent (role
// subagent) or an agent-team teammate (role main, header set all the same).
func agentPctx(role pipeline.AgentRole, id string) *pipeline.Context {
	p := finishedPctx(200, role)
	p.Headers.Set(pipeline.ClaudeCodeAgentIDHeader, id)
	return p
}

func TestRecordForKeysAnAgentByItsOwnThread(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy(cgconfig.CacheConfig{})
	for _, tc := range []struct {
		name string
		role pipeline.AgentRole
	}{{"subagent", pipeline.AgentRoleSubagent}, {"teammate", pipeline.AgentRoleMain}} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := recordFor(agentPctx(tc.role, "ab12"), "s1", pol, nil, logEmitter{})
			if !ok {
				t.Fatal("a request naming its agent was not recorded")
			}
			if r.Key != (keepalive.Key{Session: "s1", Thread: "a:ab12"}) {
				t.Fatalf("key = %+v, want the agent's own thread", r.Key)
			}
			// The core's own test of the thread id: its subagent floors and ping plans apply.
			if !keepalive.IsSubagentThread(r.Key.Thread) {
				t.Fatalf("thread %q is not a subagent thread to the keeper", r.Key.Thread)
			}
		})
	}
}

func TestRecordForSkipsASubagentThatNamesNoAgent(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy(cgconfig.CacheConfig{})
	if _, ok := recordFor(finishedPctx(200, pipeline.AgentRoleSubagent), "s1", pol, nil, logEmitter{}); ok {
		t.Fatal("a subagent with no agent id was recorded, which would replace the main thread's entry")
	}
}

// A subagent's turns keep an entry of their own beside the main thread's, and its requests leave
// the main thread's entry alone.
func TestASubagentIsKeptWarmBesideTheMainThread(t *testing.T) {
	p := keepAlivePlugin(t)
	finishTurns(t, p, 2)
	for i := 0; i < 2; i++ { // turn 0 of a thread is counted but never tracked
		sub := agentPctx(pipeline.AgentRoleSubagent, "ab12")
		invokeReq(p, sub)
		p.OnFinish(t.Context(), sub)
	}
	if n := p.keeper.Stats().Live; n != 2 {
		t.Fatalf("live entries = %d, want the main thread's and the subagent's", n)
	}
	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 2 {
		t.Fatalf("sweep sent %d pings, want one per thread", n)
	}
}

// A subagent's request that is still in flight pauses its own thread only: the main thread is
// pinged while it runs.
func TestAnInFlightSubagentDoesNotPauseTheMainThread(t *testing.T) {
	p := keepAlivePlugin(t)
	finishTurns(t, p, 2)
	invokeReq(p, agentPctx(pipeline.AgentRoleSubagent, "ab12")) // arrives, never finishes
	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 1 {
		t.Fatalf("sweep sent %d pings, want the main thread's 1", n)
	}
}

// A ping is accounted under the thread the accountant bills the agent's next request on
// (accounting.ThreadOf), so the ping's facts reach that request.
func TestAPingIsAccountedOnTheRequestsThread(t *testing.T) {
	for _, tc := range []struct {
		name string
		pctx *pipeline.Context
		want string
	}{
		{"main", finishedPctx(200, pipeline.AgentRoleMain), "main"},
		{"subagent", agentPctx(pipeline.AgentRoleSubagent, "ab12"), "ab12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em := New().pingEmitter(tc.pctx, "s1").(pingEmitter)
			if em.thread != tc.want {
				t.Fatalf("ping thread = %q, want %q", em.thread, tc.want)
			}
			if got := pingFromReport(cgcomponents.KeepAliveReport{Thread: "a:ab12"}, "h", em.thread).ThreadID; got != tc.want {
				t.Fatalf("Ping.ThreadID = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every `default` tag on keepAliveConfig is the value its zero takes in policy(). The tags are
// the schema users read; the policy takes the engine's shipped defaults, which move with the pin.
func TestKeepAliveDefaultTagsMatchThePolicy(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy(cgconfig.CacheConfig{})
	// Enabled's default is Configure's, not policy()'s: a config with no keep_alive block.
	enabled := 0.0
	if configured(t, `{}`).cfg.KeepAlive.Enabled {
		enabled = 1
	}
	actual := map[string]float64{
		"Enabled":         enabled,
		"IdleSeconds":     pol.Idle.Seconds(),
		"MaxPings":        float64(pol.MaxPings),
		"MaxUSDPerPing":   pol.MaxUSDPerPing,
		"MinPrefixTokens": float64(pol.MinPrefixTokens),
	}
	typ := reflect.TypeOf(keepAliveConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, ok := f.Tag.Lookup("default")
		if !ok {
			continue
		}
		want, known := actual[f.Name]
		if !known {
			t.Errorf("%s has a default tag but no policy value to check it against", f.Name)
			continue
		}
		if b, err := strconv.ParseBool(tag); err == nil {
			tag = map[bool]string{true: "1", false: "0"}[b]
		}
		got, err := strconv.ParseFloat(tag, 64)
		if err != nil || got != want {
			t.Errorf("%s: default tag %q, policy default %v", f.Name, tag, want)
		}
	}
}
