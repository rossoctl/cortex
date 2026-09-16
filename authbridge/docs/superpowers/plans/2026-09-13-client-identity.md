# Client Identity Implementation Plan (commit 6)

<!-- Commit references in this document point into TWO pull requests, not one. The work was
     reviewed as a single branch and then split: authlib and the proxy binary into the
     aggregate/ledger PR, the abctl surfaces into the PR stacked on it, and these documents
     into a third. Every SHA below was repointed after that split and verified to resolve.
     Four commits were split in half and are cited as `<one-half>` / `<other-half>`, labelled
     (core) and (abctl), so either PR can be reached from here.

     THREE SHAs ARE DELIBERATELY UNREACHABLE: cbe34bbc, c5b1f596 and 4765644f appear in text
     explaining that an earlier banner pointed at them wrongly. They are commits from an
     abandoned branch, and rewriting them would delete the correction they exist to record.

     "VERIFIED TO RESOLVE" IS NOW A CHECKABLE CLAIM, AND IT USED TO BE FALSE. Four
     "SUPERSEDED by" citations in the ledger plan still pointed at the abandoned branch,
     so they resolved for nobody but the author while this banner asserted the opposite —
     the exact failure the paragraph above was written about, one screen below it. They now
     point at their equivalents on the aggregate/ledger PR, each confirmed three ways:
     reachable from that branch, a unique subject match, and an IDENTICAL patch-id, since
     the split cherry-picked them rather than rewriting them.

     Re-check the whole set after any rebase, rather than trusting this paragraph:

       grep -rhoE '\b[0-9a-f]{8}\b' authbridge/docs/superpowers/{plans,specs}/2026-09-13-*.md |
         sort -u | while read s; do git merge-base --is-ancestor $s <core-branch> 2>/dev/null ||
         git merge-base --is-ancestor $s <abctl-branch> 2>/dev/null || echo "UNREACHABLE $s"; done

     Expect exactly the three named above. Anything else is a citation a reviewer cannot
     follow. Note the pattern also matches hex-looking prose (0b11001, 218100000); those are
     figures, not SHAs. -->

> **STATUS: implemented.** Landed as `6870ad3b`: `pipeline.EventClient`,
> `usage.GroupAgent` with its `byAgent` accumulator, and the cost ledger's `agent`
> column populated and part of the row key. An earlier revision of this banner said
> none of it existed; that was true when written and stopped being true one commit
> later, which is the same class of defect as a banner that overstates.
>
> **The design this plan replaced mid-flight is the one that shipped**, and Task 1's
> "This replaces the plan's original design" note is where it is argued: a memoized
> `Context.ClientInfo()` accessor, derived on demand from the headers `Context`
> already holds, rather than a `Context.Client` field each listener assigns at request
> entry. So no listener gained request-entry wiring; the event-construction sites gained
> one line each.
>
> **COVERAGE GAP, deliberate and not recorded anywhere else.** Task 2 names
> `forwardproxy` and `extproc` only, and only those were wired — ten
> `SessionEvent{...}` sites, each mutation-checked. `authlib/listener/reverseproxy`
> has **four more event sites and none of them was touched**, so every event from the
> INBOUND path carries a nil `Client`, which `Label()` renders as the reserved
> `"unknown"` bucket. Consequences a reader has to know:
>
> - `group=agent` attributes outbound traffic and silently pools all inbound traffic
>   under one row. That row is not an agent and must not be read as one.
> - A deployment whose traffic is mostly inbound gets a breakdown that looks empty
>   rather than one that says it cannot answer.
> - The fix is four one-line additions of the same `Client: pctx.ClientInfo()` — the
>   accessor is nil-safe and needs nothing from the listener — plus the mutation check
>   Task 2 Step 5 prescribes. It is small; it is just not done.
>
> Nothing about the parsing, the caps or the wire mapping is inbound-specific, so this
> is coverage rather than a design limit.
>
> **Line numbers drift.** Every `file.go:NN` below was accurate when written and many
> have moved. Read them as "roughly here" and find the symbol by name — Task 2 already
> tells you to locate the event sites by grep for exactly this reason.
>
> ---
>
> **SECOND SWEEP, 2026-09-15. The coverage gap above is still open, verified:**
> `authlib/listener/reverseproxy/server.go` still has four `SessionEvent{` constructions
> and not one `Client:` or `ClientInfo()` in the package. Every inbound event still carries
> a nil `Client` and pools under the reserved bucket. Nothing here has been quietly closed.
>
> Three things this plan says about the design are no longer accurate:
>
> - **Only ONE client is recognised.** The `EventClient` sketch in Task 1 comments its
>   `Name` field as `"claude-code" | "opencode" | "codex" | "" when unrecognised`, which
>   reads as three implemented parsers. `knownClients` holds exactly `claude-cli` →
>   `claude-code`. OpenCode and Codex are deliberately absent, and the code says why: a
>   wrong guess files one agent silently under another's name, where an unrecognised agent
>   still reports under its `Raw` value and can be identified from the breakdown. That is
>   the `Raw`-as-well-as-`Name` argument in Task 1 paying off, not a gap.
> - **The absent-client question Task 3 Step 1 leaves open was decided, and then made a
>   compiler problem.** The ledger stores `""` and the aggregator keys on `Label()`; both
>   reach the same bucket through `pipeline.UnknownClientLabel`, exported by `c3eeb22f` (abctl) / `8e4df70d` (core) for
>   exactly the reason Step 1 worries about — it was a bare literal in two packages,
>   agreeing only by a comment saying it must, and two spellings would surface as two rows
>   each holding half the unattributed spend. `costledger.labelFor`'s `GroupAgent` arm is
>   the one grouping axis that *labels* an absent value rather than dropping the row.
> - **`Label()`'s doc records an undefended collision** the plan does not anticipate: a
>   caller sending literally `User-Agent: unknown` lands in the reserved bucket. Not
>   defended, and the reasoning is worth keeping — the axis is spoofable by construction,
>   so the same caller could claim `claude-cli/2.1.14` and land in a real agent's row
>   instead. Reserving the word would buy nothing.
>
> Two internal contradictions have been fixed inline below: Task 1 Step 4 still told you to
> add a `Client` field to `pipeline.Context`, and Task 2 Step 5 still named `pctx.Client` as
> the line to delete. Both are the design this plan's own Task 1 says it replaced.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "which agent spent this" answerable — capture the calling coding agent from the request's User-Agent, carry it on the session event, and break cost down by it.

**Architecture:** Three tasks, one commit. One new fact on `pipeline.Context`, populated where the request enters, carried onto `SessionEvent` and through its hand-maintained JSON mapping. A `byAgent` accumulator and `GroupAgent` in the usage aggregator. The cost ledger's `agent` column, which shipped empty, starts being populated.

**Tech Stack:** Go 1.x, standard library only.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-13-cost-first-class-design.md`

## Global Constraints

- **Module root is `authbridge/`.** Go commands run from `authbridge/`, relative to the repo root; `git` from the repo root itself.
- **`git commit -s` mandatory.** Trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`.
- **A User-Agent is client-controlled. This is a DISPLAY axis, not a security boundary.** Say so where the field is defined, so nobody later builds authorisation on it. Never use it for a policy decision.
- **Additive on the wire.** Old events decode with an empty client; old consumers ignore the new field.
- **Bounded cardinality.** The value comes off a request header, so an attacker controls it. It must be length-capped before it is retained, and the label maps' existing `maxLabelsPerBucket` bound must cover it.
- **`presentKinds`-style honesty:** an absent User-Agent is "unknown", not a blank row. Distinguish "no UA sent" from "UA sent but unrecognised" — the raw value is kept for the second case.
- **Large test files in SEVERAL SMALL tool calls**, never one big `Write`. A single oversized write exceeds this environment's stream watchdog and kills the agent deterministically; it already killed one dispatch on this branch.
- Do NOT run `make lint`. Do NOT `gofmt -w .` at the module root.
- Known pre-existing failure on this machine: `cmd/abctl`'s `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` — a TEST-ISOLATION BUG: the fixture is deliberately bundle-less, but the machine's own `~/.cortex/ca/bundle.crt` leaks through; fails on `main` too.
- Comment register: long comments explaining *why*, naming the bug the code prevents.

## The hazard this commit has to survive

`SessionEvent`'s JSON form is **hand-maintained in four places**: the struct itself, `sessionEventWire`, the `MarshalJSON` field list (`authlib/pipeline/session.go:313-334`) and the `UnmarshalJSON` field list (`:339-365`). A field added to the struct but missed in the wire mapping compiles, passes every unit test that stays in-process, and silently never reaches abctl — which reads events out of process.

This is exactly the class of bug that has already cost this branch two fix rounds (a stale doc, and a test pinned to a name rather than a behaviour). Task 1 therefore adds a **reflection-based drift guard** as well as the field, so the next person to add one cannot lose it the same way.

---

## File Structure

| file | responsibility | task |
|---|---|---|
| `authlib/pipeline/client.go` | **new** — `EventClient` and the User-Agent parser | 1 |
| `authlib/pipeline/client_test.go` | **new** — parser table, cap, unknown-vs-absent | 1 |
| `authlib/pipeline/session.go` | `SessionEvent.Client`, wire mapping, drift guard | 1 |
| `authlib/pipeline/session_test.go` | round-trip + the reflection guard | 1 |
| `authlib/pipeline/context.go` | `Context.ClientInfo()` + memo field | 1 |
| `authlib/listener/forwardproxy/*.go`, `extproc/server.go` | populate on each event (**not** at request entry — see Task 1's redesign). `reverseproxy` is missing from this row and stayed unwired | 2 |
| `authlib/usage/usage.go`, `snapshot.go` | `byAgent`, `GroupAgent` | 3 |
| `authlib/costledger/writer.go` | populate the `agent` column | 3 |
| `authlib/sessionapi/usage.go` | disclosure block + parameter list | 3 |

---

## Task 1: The fact, on the event, with a drift guard

**Interfaces produced:**
```go
// authlib/pipeline/client.go
type EventClient struct {
    Name    string `json:"name,omitempty"`    // "claude-code" | "opencode" | "codex" | "" when unrecognised
    Version string `json:"version,omitempty"`
    Raw     string `json:"raw,omitempty"`     // the UA verbatim, so an unrecognised client is still nameable
}
func ParseUserAgent(ua string) *EventClient   // nil when ua == ""
func (c *EventClient) Label() string          // "claude-code/2.1.14", or "unknown" when nil
```
`SessionEvent.Client *EventClient`, and on `Context`:

```go
// ClientInfo returns the calling agent, parsed from this request's User-Agent
// and memoized.
//
// Memoized rather than parsed per event because a turn produces at least a
// request and a response event and the parse allocates; and derived here rather
// than set by each listener because Context already carries the request headers,
// so a listener-populated field would be a second source of the same truth.
func (c *Context) ClientInfo() *EventClient
```

**This replaces the plan's original design**, which added a `Context.Client` field for the listeners to populate at request entry. That was written believing `Context` had no request headers — it does: `Headers http.Header` at `context.go:121`, the request-side counterpart to `ResponseHeaders`. Deriving it on demand means **no listener request-entry code changes at all**; only the event-construction sites gain `Client: pctx.ClientInfo()`. Task 2 shrinks accordingly and its riskiest half disappears.

Keep the memo field unexported (`client *EventClient`, plus a `clientOnce` or a parsed flag — a nil result is a valid memoized answer, so a bare nil check would re-parse every call for any request with no User-Agent).

**Why `Raw` as well as `Name`:** #952 needs to report per agent and to distinguish an honest zero from missing data. An unrecognised agent that we can still *name* is data; one we cannot is not. Keeping the raw value means a new coding agent shows up in the breakdown the day someone runs it, instead of after we ship a parser update — and it is what tells the OpenCode/Codex/Bob detection work what strings to expect.

- [ ] **Step 1: Write the failing parser test**

`authlib/pipeline/client_test.go`, in two small calls:

```go
func TestParseUserAgent(t *testing.T) {
	for _, tc := range []struct {
		name, ua          string
		wantName, wantVer string
		wantNil           bool
	}{
		{"absent is nil", "", "", "", true},
		{"claude code", "claude-cli/2.1.14 (external, cli)", "claude-code", "2.1.14", false},
		{"claude code bare", "claude-cli/2.1.14", "claude-code", "2.1.14", false},
		{"unrecognised keeps raw only", "SomeNewAgent/9.9", "", "", false},
		{"curl is not a coding agent", "curl/8.4.0", "", "", false},
		{"whitespace only is nil", "   ", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("ParseUserAgent(%q) = %+v, want nil", tc.ua, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if got.Version != tc.wantVer {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVer)
			}
			// Raw is always kept: an unrecognised agent we can still NAME is data.
			if got.Raw == "" {
				t.Error("Raw is empty; the verbatim UA must be kept")
			}
		})
	}
}

func TestParseUserAgent_CapsTheRetainedValue(t *testing.T) {
	// The header is caller-controlled and the value is retained in bucket label
	// maps for a full ring lap. Without a cap a caller parks arbitrary bytes in
	// memory from off-host, on the synchronous session-append path — the same
	// reasoning as usage.maxLabelLen.
	long := strings.Repeat("A", 10_000)
	got := ParseUserAgent(long)
	if got == nil {
		t.Fatal("ParseUserAgent returned nil for a long UA")
	}
	if len(got.Raw) > maxClientLen {
		t.Errorf("Raw retained %d bytes, want <= %d", len(got.Raw), maxClientLen)
	}
}

func TestEventClient_Label(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *EventClient
		want string
	}{
		{"nil is unknown", nil, "unknown"},
		{"recognised", &EventClient{Name: "claude-code", Version: "2.1.14"}, "claude-code/2.1.14"},
		{"no version", &EventClient{Name: "claude-code"}, "claude-code"},
		{"unrecognised falls back to raw", &EventClient{Raw: "SomeNewAgent/9.9"}, "SomeNewAgent/9.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Label(); got != tc.want {
				t.Errorf("Label() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

`Label()` must be safe on a nil receiver — the aggregator and ledger call it on events that carry no client, and a nil check at every call site is how one gets forgotten.

- [ ] **Step 2: Write the drift guard**

Append to `authlib/pipeline/session_test.go`:

```go
// TestSessionEventWireCoversEveryField is a drift guard, not a behaviour test.
//
// SessionEvent's JSON form is hand-maintained in four places: the struct,
// sessionEventWire, MarshalJSON's field list and UnmarshalJSON's. A field added to
// the struct but missed in the wire mapping compiles, passes every in-process
// test, and silently never reaches abctl — which decodes these out of process.
//
// Comparing field COUNTS rather than names on purpose: the two structs
// deliberately differ in spelling (Duration vs DurationMs), so a name check would
// need an exception list that itself goes stale. A count mismatch is the signal
// that someone added to one and not the other.
func TestSessionEventWireCoversEveryField(t *testing.T) {
	got := reflect.TypeOf(SessionEvent{}).NumField()
	want := reflect.TypeOf(sessionEventWire{}).NumField()
	if got != want {
		t.Fatalf("SessionEvent has %d fields, sessionEventWire has %d.\n"+
			"Add the new field to sessionEventWire AND to both MarshalJSON and "+
			"UnmarshalJSON, or abctl will never see it.", got, want)
	}
}

func TestSessionEvent_ClientRoundTrips(t *testing.T) {
	in := SessionEvent{
		At:     time.Now().UTC().Truncate(time.Millisecond),
		Phase:  SessionResponse,
		Client: &EventClient{Name: "claude-code", Version: "2.1.14", Raw: "claude-cli/2.1.14"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SessionEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Client == nil {
		t.Fatal("Client was lost in the round trip — check sessionEventWire and both mappings")
	}
	if out.Client.Name != "claude-code" || out.Client.Version != "2.1.14" {
		t.Errorf("Client = %+v, want name/version preserved", out.Client)
	}
}

func TestSessionEvent_AbsentClientRoundTripsAsNil(t *testing.T) {
	// Old events, and any traffic with no User-Agent. Must stay nil rather than
	// decoding to an empty struct, so "unknown" and "named but empty" differ.
	raw, err := json.Marshal(SessionEvent{At: time.Now(), Phase: SessionRequest})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`"client"`)) {
		t.Errorf("absent client serialized a key: %s", raw)
	}
	var out SessionEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Client != nil {
		t.Errorf("Client = %+v, want nil", out.Client)
	}
}
```

- [ ] **Step 3: Run both to verify they fail**

Run: `go test ./authlib/pipeline/ -run 'TestParseUserAgent|TestEventClient|TestSessionEvent_Client|TestSessionEventWireCovers' -v 2>&1 | tail -20`
Expected: FAIL — `undefined: ParseUserAgent`. The drift guard should **pass** before the change and fail after the struct field is added but before the wire mapping is — check that intermediate state deliberately and report it, because that is the proof the guard works.

- [ ] **Step 4: Implement**

Create `client.go` with `maxClientLen = 128` (long enough for any real UA prefix; the cap's reasoning mirrors `usage.maxLabelLen`). Recognise `claude-cli/` → `claude-code`. Keep the recognised set small and add a comment that `toolscan/known.go` already hardcodes Claude Code's built-in tool names, so Claude Code is the only agent with full support today and the others arrive with their own detection work.

Add `Client *EventClient` to `SessionEvent` and `sessionEventWire`, and to both mapping functions. ~~Add `Client *EventClient` to `pipeline.Context`.~~ **No — that last sentence is the design this task's own redesign note replaced, left standing when the rest of the section was rewritten.** `Context` gets the unexported memo pair from Task 2 Step 3 (`client *EventClient`, `clientParsed bool`) and the `ClientInfo()` accessor. An exported field would be a second source of a truth `Context.Headers` already holds, and ten-odd event sites would each have to remember to fill it.

- [ ] **Step 5: GREEN, then the whole package**

Run: `go test ./authlib/pipeline/ 2>&1 | tail -5`

- [ ] **Step 6: Do not commit yet.** Task 3 commits.

---

## Task 2: Populate it at request entry

**Files:** `authlib/listener/forwardproxy/server.go`, `transparent.go`, `authlib/listener/extproc/server.go`

**What this task is, after the redesign above:** add `Client: pctx.ClientInfo()` at each `pipeline.SessionEvent{...}` construction site. There are 6+ across the two listeners — `extproc/server.go:240,279,333,373,402,450`, `forwardproxy/server.go:352,745,1054`, `transparent.go:190` — and they already read `pctx.Host` the same way, so each is a one-line addition alongside an existing field.

**Ten sites were wired, and `reverseproxy`'s four were not.** The list above names two listeners and the grep in the reference section names the same three files, so a third listener's event sites were never in scope — `authlib/listener/reverseproxy/server.go` has four `SessionEvent` constructions and none carries a `Client`. Every INBOUND event therefore has a nil client, which `Label()` renders as the reserved `"unknown"` bucket, so `group=agent` pools all inbound traffic into one row that is not an agent. Left as a gap rather than quietly closed here because it needs the same mutation check Step 5 prescribes; the change itself is four lines. Note the shape of the mistake: the enumeration was correct about the sites it listed and the mutation check proved each of them, and neither of those can detect a listener that was never enumerated. Grep the whole `authlib/listener/` tree, not the files a plan names.

Two sites build events from an `*http.Request` at request entry rather than from a `pctx` (`forwardproxy/server.go:265,1089`). Check whether those construct a `SessionEvent` at all or only a `Context`; if they do build an event and have no `pctx` in scope, parse from `r.Header.Get("User-Agent")` there via the same exported `ParseUserAgent`, and note it — do not invent a second parser.

**No request-entry wiring is needed** and no listener needs a new assignment: `ClientInfo()` reads headers the `Context` already holds. If you find yourself adding a field for a listener to populate, stop — that is the design this plan replaced.

- [ ] **Step 1: Write the failing test**

Two layers of test, because they catch different failures.

**Layer 1 — `ClientInfo()` itself, in `authlib/pipeline/client_test.go`:**

```go
func TestContextClientInfo_ParsesFromTheRequestHeaders(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14 (external, cli)")

	got := c.ClientInfo()

	if got == nil {
		t.Fatal("ClientInfo() = nil for a request carrying a User-Agent")
	}
	if got.Name != "claude-code" || got.Version != "2.1.14" {
		t.Errorf("ClientInfo() = %+v, want claude-code/2.1.14", got)
	}
}

func TestContextClientInfo_NilWhenNoUserAgent(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_NilHeadersDoesNotPanic(t *testing.T) {
	// A Context built by a listener that never set Headers. This runs on the
	// request hot path; a nil map read is fine but a nil *Context is not, and the
	// event sites call this unconditionally.
	c := &Context{}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_MemoizesIncludingTheNilAnswer(t *testing.T) {
	// The subtle one. A nil result is a VALID memoized answer, so a bare nil check
	// as the memo guard would re-parse on every call for exactly the requests that
	// have nothing to parse — and every turn calls this at least twice, once per
	// event.
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")

	first := c.ClientInfo()
	// Mutating the header after the first call must not change the answer; if it
	// does, the value is being re-derived rather than memoized.
	c.Headers.Set("User-Agent", "something-else/9.9")
	second := c.ClientInfo()

	if first != second {
		t.Errorf("ClientInfo() returned different pointers across calls: %p then %p", first, second)
	}
	if second.Name != "claude-code" {
		t.Errorf("second call re-parsed the mutated header: %+v", second)
	}
}

func TestContextClientInfo_MemoizesTheNilCase(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	_ = c.ClientInfo() // nil
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v after a memoized nil; want the nil to stick", got)
	}
}
```

That last pair is the whole reason the memo needs a "parsed" flag rather than `if c.client == nil`.

**Layer 2 — the event sites actually carry it.** Prefer the listeners' existing test style; if they already spin a server and capture recorded events, extend that. The assertion that matters: a request carrying `User-Agent: claude-cli/2.1.14` produces session events — **both request and response phases** — whose `Client.Name` is `claude-code`, and a request with no User-Agent produces events with a nil `Client`.

If reaching the event sites needs more scaffolding than the change warrants, cover it at the `pipeline.Context` boundary instead — but then **report that the listener wiring is untested** and enumerate every site you edited, so the reviewer checks them by inspection rather than assuming. Do not quietly settle for layer 1: an unwired event site is exactly the defect Step 5 exists to catch, and on this branch an unwired call site has already shipped once.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./authlib/pipeline/ -run TestContextClientInfo -v 2>&1 | tail -20`
Expected: FAIL — `c.ClientInfo undefined`.

- [ ] **Step 3: Implement**

On `Context`, an unexported memo plus the accessor:

```go
	// client and clientParsed memoize ClientInfo's answer.
	//
	// A separate flag rather than a nil check on client, because nil IS a valid
	// answer — a request with no User-Agent — and a nil-guard memo would re-parse
	// on every call for precisely those requests. Each turn calls this at least
	// twice, once per session event.
	client       *EventClient
	clientParsed bool
```

```go
// ClientInfo returns the calling agent, parsed from this request's User-Agent and
// memoized.
//
// Derived here rather than assigned by each listener because Context already
// carries the request headers, so a listener-populated field would be a second
// source of the same truth — and there are ten-odd event construction sites that
// would each have to remember to set it.
//
// Nil means no User-Agent was sent. Callers do not nil-check: EventClient.Label()
// is nil-safe and answers "unknown".
func (c *Context) ClientInfo() *EventClient {
	if c.clientParsed {
		return c.client
	}
	c.clientParsed = true
	if c.Headers != nil {
		c.client = ParseUserAgent(c.Headers.Get("User-Agent"))
	}
	return c.client
}
```

Not goroutine-safe, and that is correct: a `Context` belongs to one request and the pipeline runs its phases sequentially. Say so in the comment, because the surrounding type has fields that other goroutines do read.

Then add `Client: pctx.ClientInfo()` at each `pipeline.SessionEvent{…}` construction site. Locate them by grep, not by the line numbers in this plan — `grep -n 'SessionEvent{' authlib/listener/*/*.go authlib/pipeline/*.go`. Expect roughly ten across `extproc/server.go`, `forwardproxy/server.go` and `transparent.go`.

That grep reaches `reverseproxy/server.go` too, and its **four sites were left unwired** — see the banner and Task 2's opening for the consequence. "Roughly ten across these three files" is the sentence that made a fourth file invisible: the number came from the files, so counting them back could never disagree with it.

For the one or two sites that build an event from an `*http.Request` with no `pctx` in scope, call `ParseUserAgent(r.Header.Get("User-Agent"))` directly — the same exported parser, never a second implementation — and note which sites those were.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./authlib/pipeline/ ./authlib/listener/... 2>&1 | tail -10`

- [ ] **Step 5: Mutation-check the wiring.** For each event site you edited, delete the `Client: pctx.ClientInfo()` line — `pctx.Client`, as an earlier revision of this step wrote it, is the replaced design and no such field exists — confirm a test fails, restore, and confirm `git diff` shows the file byte-identical. If deleting a site fails nothing, that site is untested — say which in your report rather than leaving it silent. This is the same discipline that caught an unwired `settleCost` call earlier on this branch.

- [ ] **Step 6: Do not commit yet.**

---

## Task 3: Group by it, and fill the ledger's column

**Files:** `authlib/usage/usage.go`, `authlib/usage/snapshot.go`, `authlib/costledger/writer.go`, `authlib/sessionapi/usage.go`

- [ ] **Step 1: Write the failing tests**

`authlib/usage/snapshot_test.go` — reuse the file's existing `inferenceEvent` helper and `mergeSeries`:

```go
func TestSnapshot_GroupAgentBreaksDownByClient(t *testing.T) {
	a := New()
	e1 := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e1.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	e2 := inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001)
	e2.Client = &pipeline.EventClient{Name: "opencode", Version: "0.4.2"}
	a.Record("s1", e1)
	a.Record("s1", e2)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if got := series["claude-code/2.1.14"].InputTokens; got != 10 {
		t.Errorf("claude-code InputTokens = %d, want 10", got)
	}
	if got := series["opencode/0.4.2"].InputTokens; got != 20 {
		t.Errorf("opencode InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupAgentFoldsAnAbsentClientUnderUnknown(t *testing.T) {
	// "unknown" rather than "", because a blank row reads as a bug rather than as
	// unattributed traffic — and the savings work requires missing data and an
	// honest zero to be distinguishable.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Client = nil
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if _, blank := series[""]; blank {
		t.Error(`series has an "" key`)
	}
	if got := series["unknown"].InputTokens; got != 10 {
		t.Errorf("unknown InputTokens = %d, want 10", got)
	}
}

func TestSnapshot_GroupAgentCoversNonInferenceTrafficToo(t *testing.T) {
	// byAgent has no inference guard, unlike byMethod. So this axis counts MCP and
	// tool traffic as well, and its denominator differs from group=model's — the
	// same hazard group=endpoint has, and it must be documented the same way.
	a := New()
	e := &pipeline.SessionEvent{
		At: time.Now(), Phase: pipeline.SessionResponse, StatusCode: 200, Host: "tool",
		Client: &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"},
	}
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupAgent).Buckets)

	if got := series["claude-code/2.1.14"].Requests; got != 1 {
		t.Errorf("Requests = %d, want 1 — non-inference traffic is attributed too", got)
	}
}

func TestParseGroup_AcceptsAgent(t *testing.T) {
	got, err := ParseGroup("agent")
	if err != nil {
		t.Fatalf("ParseGroup(\"agent\"): %v", err)
	}
	if got != GroupAgent {
		t.Errorf("= %q, want %q", got, GroupAgent)
	}
}
```

`authlib/costledger/writer_test.go` — reuse `costedEvent` and `readAllRows`:

```go
func TestWriter_RecordsTheAgentLabel(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	w.Record("s1", e)

	now = at.Add(time.Minute)
	w.Flush()

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Agent != "claude-code/2.1.14" {
		t.Errorf("Agent = %q, want claude-code/2.1.14", rows[0].Agent)
	}
}

func TestWriter_TwoAgentsInOneMinuteAreTwoRows(t *testing.T) {
	// The agent is part of the composite key, so two agents on the same endpoint
	// and model must not be merged into one row.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	a := costedEvent(t, "gw", "m", 0.25, 100, 50)
	a.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	b := costedEvent(t, "gw", "m", 0.25, 100, 50)
	b.Client = &pipeline.EventClient{Name: "opencode", Version: "0.4.2"}
	w.Record("s1", a)
	w.Record("s1", b)

	now = at.Add(time.Minute)
	w.Flush()

	if rows := readAllRows(t, dir); len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per agent", len(rows))
	}
}

func TestWriter_NoClientStillWritesTheRow(t *testing.T) {
	// Dropping unattributed traffic would make the ledger's totals disagree with
	// /v1/usage's, which is worse than an empty column.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = nil
	w.Record("s1", e)

	now = at.Add(time.Minute)
	w.Flush()

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want the cost still recorded", rows[0].CostMicros)
	}
}
```

**Decide and state which convention the ledger uses for an absent client** — `""` or `"unknown"`. The aggregator uses `"unknown"` because it is a display label; the ledger's `agent` field is `omitempty`, so `""` keeps the file smaller and lets a reader distinguish "no agent recorded" from an agent literally called unknown. Whichever you pick, make the two consistent in *meaning* and write down why they differ in *representation* if they do. A reader joining the two sources must not be surprised.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./authlib/usage/ ./authlib/costledger/ -run 'GroupAgent|ParseGroup_AcceptsAgent|Agent' -v 2>&1 | tail -20`
Expected: FAIL — `undefined: GroupAgent`, `rows[0].Agent` empty.

- [ ] **Step 3: Implement**

- `bucket` gains `byAgent map[string]Counts`, with a comment noting that unlike `byMethod` it has **no inference guard**, so its denominator differs from `group=model`'s — the same property `byEndpoint` has.
- `foldInto` adds `addLabel(&b.byAgent, truncateLabel(e.Client.Label()), one)` unconditionally, since `Label()` is nil-safe and returns `"unknown"`.
- `GroupAgent Group = "agent"`, a `ParseGroup` case, a `series` arm, and the error string extended to name it.
- `costledger.Writer.Record` sets the `Agent` field, replacing the empty-column placeholder **and** the comment promising a later change would populate it — that comment becoming false is the defect class this branch has paid for four times.
- `costledger.labelFor` gains its `usage.GroupAgent` arm, which the ledger plan deliberately left out because the constant did not exist yet.
- `sessionapi/usage.go`: add `agent` to the `group` parameter list, and a disclosure bullet. `group=agent` exposes which coding agents and which **versions** run on the operator's workstation — fingerprinting-adjacent, and the most personal of the groupings. Say that plainly; it is also the axis most likely to be objected to, and the block exists so the objection is possible.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./authlib/usage/ ./authlib/costledger/ ./authlib/sessionapi/ 2>&1 | tail -10`

- [ ] **Step 5:** Full suite, vet, lint, `gofmt -l` on touched dirs.

- [ ] **Step 6: Commit — commit 6 of the PR.** The message must state that a User-Agent is client-controlled and this is a display axis rather than a security boundary; that `Raw` is kept so an unrecognised agent is still nameable; that the drift guard exists because the wire mapping is hand-maintained in four places; and that the ledger's `agent` column, which shipped empty, is now populated.

---

## Self-Review

**Spec coverage:** `EventClient{Name, Version, Raw}` on the event (Task 1) ✓; populated from the request's User-Agent (Task 2) — **outbound only**, see below; documented as a display axis, not a security boundary (Tasks 1, 3) ✓; `GroupAgent` (Task 3) ✓; ledger `agent` column populated (Task 3) ✓; unrecognised agents still nameable (Task 1, via `Raw`) ✓; absent vs unrecognised distinguishable (Task 1) ✓.

**The one partial tick.** `forwardproxy` and `extproc` carry the client; `reverseproxy` does not, so every inbound event has a nil `Client` and pools under `"unknown"`. Ticked as done because every site the plan enumerated was wired and mutation-checked — which is exactly why the tick is worth keeping visible rather than editing to `✓`: coverage of an enumeration proves nothing about the enumeration.

**Beyond the spec, and why:** the reflection drift guard. The spec says only "the listener gains one fact"; it does not say that adding a field to `SessionEvent` has four edit sites and fails silently if you miss one. Adding the field without the guard would leave the next person the same trap.

**Type consistency:** `EventClient` is a pointer on both `Context` and `SessionEvent`, nil meaning "no User-Agent sent". `Label()` is nil-safe and returns `"unknown"` — every consumer (aggregator, ledger) calls it without a nil check, which is the point.

**Risk:** Task 2 touches ~10 construction sites across two listeners. The mutation check in Task 2 Step 5 is what turns "I edited them all" from a claim into evidence.

It did: all ten were mutation-checked, and deleting the `Client` line at any one of them fails a named test. It also proved nothing about the third listener, whose four sites this plan never names. Both halves of that sentence are the finding.
