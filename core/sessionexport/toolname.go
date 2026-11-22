package sessionexport

// Tool-name classification and the keyed hash, which together are the privacy gate on a
// toolCall record's `tool` field (spec T4, T10, T10a, T11, T11a, T12).
//
// THE BRANCH IS ON "IS THIS PROVABLY A BUILT-IN", NEVER ON "IS THIS PROVABLY MCP" (T10a). A
// built-in name is a closed vocabulary this proxy can enumerate, so it can be exported verbatim.
// Everything else — an MCP name, or a name whose source cannot be determined — is hashed. The
// asymmetry is the whole point: a hashed built-in costs a consumer one unreadable label, where a
// cleartext MCP name costs a customer's identity. `mcp__acme-billing-prod__refund` carries a
// vendor relationship, an internal system name or a project codename.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ToolSource is the closed vocabulary for a toolCall record's `toolSource` (T4). Exported by
// NAMING ITS VALUES, never by sanitising what arrived — the rule Record.AgentRole already
// follows, for the reason its comment gives: this package's input is a DECODED archive event,
// where the field is whatever the JSON said.
type ToolSource string

const (
	// ToolBuiltin is a tool from a client's own closed vocabulary. ONLY THIS SOURCE EXPORTS ITS
	// NAME IN CLEAR (T10).
	ToolBuiltin ToolSource = "builtin"
	// ToolMCP is an operator-defined, namespaced MCP tool. Hashed (T11).
	ToolMCP ToolSource = "mcp"
	// ToolUnknown is a name this package cannot place. Hashed, like an MCP name (T10a) — the
	// safe default for a name that might be operator-defined is to treat it as if it were.
	ToolUnknown ToolSource = "unknown"
)

// mcpPrefix is the namespace separator Claude Code puts on an MCP tool: `mcp__<server>__<tool>`.
const mcpPrefix = "mcp__"

// builtinTools is the built-in vocabulary, PER CLIENT (T10): "one agent's `Bash` and `Read` are
// another's `bash` and `read`", so there is no single list to normalise against and normalising
// would lose which spelling the client actually sent. Keyed by the canonical client name
// model.AgentName folds a User-Agent to — the same key Record.Client carries.
//
// WITHIN A SOURCE THE NAME IS NEVER REWRITTEN: no case folding, no canonical spelling. A lookup
// miss is ToolUnknown, which hashes, so a built-in this map does not list is unreadable rather
// than leaked — the failure direction T10a asks for.
//
// INCOMPLETE BY CONSTRUCTION, and that is survivable rather than a bug to fix with a wildcard. A
// client adds tools faster than this list is updated, and the cost of a miss is a hashed label on
// a built-in. The cost of the opposite default — treating an unlisted name as a built-in — is a
// customer's MCP server name in cleartext. Add names as they are observed; never add a pattern.
var builtinTools = map[string]map[string]bool{
	// Claude Code's own tools, as its manifests send them.
	"claude-code": set(
		"Agent", "AskUserQuestion", "Bash", "BashOutput", "Edit", "ExitPlanMode", "Glob",
		"Grep", "KillBash", "KillShell", "ListMcpResources", "MultiEdit", "NotebookEdit",
		"NotebookRead", "Read", "ReadMcpResource", "SlashCommand", "Skill", "Task",
		"TaskOutput", "TaskStop", "TodoWrite", "WebFetch", "WebSearch", "Write",
		// Workflow and subagent control, which end a run by design (toolpair's endsRun).
		"StructuredOutput", "SubagentHandback", "end_subtask",
		// Scheduling and messaging, observed on this proxy's own traffic.
		"CronCreate", "CronDelete", "CronList", "ListAgents", "ScheduleWakeup",
		"SendMessage", "ReportFindings", "Workflow", "EnterPlanMode", "EnterWorktree",
		"ExitWorktree",
	),
	// Codex's tools are lowercase, which is exactly the per-client divergence T10 names.
	"codex": set(
		"apply_patch", "bash", "container_exec", "exec_command", "local_shell",
		"read_file", "shell", "update_plan", "view_image", "web_search", "write_file",
	),
	// Bob's subtask vocabulary.
	"bob": set("end_subtask", "start_subtask", "update_plan"),
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// classifyTool decides a tool name's source. client is the canonical client name (Record.Client's
// name half); an empty or unrecognised client cannot prove a built-in, so its names are unknown.
//
// ORDER MATTERS: the MCP prefix is checked FIRST. A client that somehow offered a built-in named
// `mcp__…` would still be treated as MCP, because the prefix is the operator-defined namespace
// and mislabelling it builtin is the one error with a privacy cost.
func classifyTool(name, client string) ToolSource {
	switch {
	case name == "":
		return ToolUnknown
	case strings.HasPrefix(name, mcpPrefix):
		return ToolMCP
	case builtinTools[client][name]:
		return ToolBuiltin
	default:
		return ToolUnknown
	}
}

// hashPrefix types the digest on the wire, so a consumer can tell a hashed label from a cleartext
// one without guessing from its shape (T11).
const hashPrefix = "hmac-sha256:"

// hashedToolLen is how many hex characters of the digest a label carries. 32 hex chars = 128 bits,
// which is far past collision risk for the number of distinct tool names any install sees, and it
// keeps a label short enough to read in a terminal. The full 64 would survive T12's 96-byte cut
// too, so this is legibility rather than necessity.
const hashedToolLen = 32

// ToolNameKey is the install-local HMAC key for hashing tool names. Its zero value hashes
// nothing: a Keyer with no key returns an empty label rather than an unkeyed digest, because an
// unkeyed digest of a namespaced name is REVERSIBLE by trying candidate vendor names — which is
// the whole reason T11 requires a key.
type ToolNameKey struct {
	key []byte
}

// toolKeyFile is the key's name beside the install's other state.
const toolKeyFile = "export-toolname-key"

// keyOnce guards the process-local cache. The FILE is the cross-process authority; this only
// avoids re-reading it per request.
var (
	keyOnce   sync.Mutex
	keyCached map[string]ToolNameKey
)

// LoadToolNameKey returns the install-local tool-name key under stateDir, creating it on first
// use. 32 bytes from crypto/rand, written 0600.
//
// THE RACE THE SPEC FLAGS IS CLOSED BY THE FILESYSTEM, not by a lock (T11a). The spec borrows
// cmd/cortex's writeGatewayFiles read-or-create, but that runs ONCE AT BOOT: two concurrent
// `?include=toolCalls` requests on a fresh install could each mint a key and the later atomic
// write would win, re-labelling the earlier export's MCP tools after it had already streamed. So
// the create is O_CREATE|O_EXCL and EEXIST means "someone else won — re-read and use theirs".
// Only one writer can ever observe a successful create, so only one key can ever be in force.
//
// IT DOES NOT ROTATE. Rotation would silently re-label every MCP tool from that point, splitting
// one tool's history into two series a consumer cannot rejoin. Anyone who needs new labels
// deletes the file and knows the old exports no longer compare; a lost key is the same event, and
// the honest answer is that the old labels are unrecoverable — which is the property that made a
// keyed hash safe to ship.
func LoadToolNameKey(stateDir string) (ToolNameKey, error) {
	if stateDir == "" {
		return ToolNameKey{}, errors.New("sessionexport: no state dir for the tool-name key")
	}
	keyOnce.Lock()
	defer keyOnce.Unlock()
	if k, ok := keyCached[stateDir]; ok {
		return k, nil
	}
	path := filepath.Join(stateDir, toolKeyFile)
	k, err := readOrCreateKey(path)
	if err != nil {
		return ToolNameKey{}, err
	}
	if keyCached == nil {
		keyCached = map[string]ToolNameKey{}
	}
	keyCached[stateDir] = k
	return k, nil
}

// toolKeyLen is the key's size: 32 bytes, as T11a specifies.
const toolKeyLen = 32

func readOrCreateKey(path string) (ToolNameKey, error) {
	// The ordinary path first: a key already exists.
	if b, err := os.ReadFile(path); err == nil {
		if len(b) < toolKeyLen {
			// A short key is a truncated write from an older crash, not a key. Refusing is
			// better than hashing under a weak one: the labels would be wrong AND weak.
			return ToolNameKey{}, errors.New("sessionexport: tool-name key is too short; delete it to mint a new one")
		}
		return ToolNameKey{key: b[:toolKeyLen]}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ToolNameKey{}, err
	}

	fresh := make([]byte, toolKeyLen)
	if _, err := rand.Read(fresh); err != nil {
		return ToolNameKey{}, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ToolNameKey{}, err
	}

	// WRITE TO A TEMPORARY FILE, THEN PUBLISH BY LINKING IT INTO PLACE.
	//
	// O_CREATE|O_EXCL on the real path closes the two-key race — only one process can observe a
	// successful create — but it PUBLISHES AN EMPTY FILE and fills it afterwards. Two things go
	// wrong in that window, and both are worse than the race it fixes:
	//
	//   - A concurrent reader sees a 0-byte key, finds it shorter than toolKeyLen, and refuses.
	//   - A crash or a failed write between create and sync LEAVES that short file. The refusal
	//     is then permanent: every later opted-in export fails until an operator deletes it,
	//     even after whatever broke the write recovered.
	//
	// So the authoritative path only ever appears complete. os.Link fails with EEXIST rather
	// than replacing, which keeps the no-replace guarantee the race needs: a key already in
	// force is never overwritten by a second one.
	tmp, err := os.CreateTemp(dir, toolKeyFile+".tmp-*")
	if err != nil {
		return ToolNameKey{}, err
	}
	tmpName := tmp.Name()
	// Unpublished temporary files never survive a failure, whichever branch returns.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return ToolNameKey{}, err
	}
	if _, err := tmp.Write(fresh); err != nil {
		return ToolNameKey{}, err
	}
	// Synced BEFORE publishing, so the bytes are durable by the time any reader can find them.
	if err := tmp.Sync(); err != nil {
		return ToolNameKey{}, err
	}
	if err := tmp.Close(); err != nil {
		return ToolNameKey{}, err
	}

	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return ToolNameKey{}, err
		}
		// Someone else published first. Theirs is in force — a second key would re-label an
		// export already streaming under the first — and it is complete, because that is the
		// only state this path ever reaches.
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return ToolNameKey{}, rerr
		}
		if len(b) < toolKeyLen {
			return ToolNameKey{}, errors.New("sessionexport: tool-name key is too short; delete it to mint a new one")
		}
		return ToolNameKey{key: b[:toolKeyLen]}, nil
	}
	return ToolNameKey{key: fresh}, nil
}

// label is the exported form of a tool name under source.
//
// HASHING HAPPENS BEFORE T12'S CUT, so the cut never splits a digest: the hash is computed over
// the WHOLE name and the result is already short enough to pass exportLabel unchanged. A built-in
// name goes through exportLabel for the bound every wire-sourced string in a record passes.
//
// A KEYLESS Keyer RETURNS EMPTY for a name that must be hashed, never the name and never an
// unkeyed digest. An absent `tool` field reads as "not stated", which is a real answer; a
// cleartext MCP name is not.
func (k ToolNameKey) label(name string, source ToolSource) string {
	if source == ToolBuiltin {
		return exportLabel(name)
	}
	if len(k.key) == 0 || name == "" {
		return ""
	}
	mac := hmac.New(sha256.New, k.key)
	mac.Write([]byte(name))
	sum := hex.EncodeToString(mac.Sum(nil))
	return hashPrefix + sum[:hashedToolLen]
}
