package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// bobSetKey writes one top-level key into a settings file, changing nothing else
// in it — byte for byte. It is the reason bob does not call writeSettings.
//
// writeSettings round-trips through json.MarshalIndent over a map[string]any, and
// that is lossy in ways JSON does not consider meaningful but a person reading a
// diff does: Go sorts map keys, so a hand-grouped file is alphabetized; indentation
// is normalized to two spaces; and an inline array like [80, 120] is exploded onto
// one line per element. On a real settings file, adding one key moved
// workbench.colorTheme from first to last and rewrote ten lines. These files are
// hand-curated and frequently committed to a dotfiles repo, so a semantically
// equal but textually large diff is a real cost — and it contradicted the
// "Nothing else in the file changes" line printed directly above the write.
//
// So the edit is textual and surgical, and the parser drives it rather than a
// regex: json.Decoder reports byte offsets, which is what makes it safe to splice
// a document this way. A key whose name appears inside some other string value
// cannot be mistaken for the real member, because the offsets come from the
// tokenizer, not from a search.
//
// value == nil means delete the key. Returns the new file content.
func bobSetKey(src []byte, key string, value any) ([]byte, error) {
	start, end, found, err := bobFindMember(src, key)
	if err != nil {
		return nil, err
	}

	if value == nil {
		if !found {
			return src, nil
		}
		return bobSpliceOut(src, start, end), nil
	}

	// Encoded the same way either way, so a replaced value and an inserted one are
	// formatted identically.
	vb, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	member := append([]byte(strconv.Quote(key)+": "), vb...)

	if found {
		out := make([]byte, 0, len(src)+len(member))
		out = append(out, src[:start]...)
		out = append(out, member...)
		out = append(out, src[end:]...)
		return out, nil
	}
	return bobInsertMember(src, member)
}

// errBobDuplicateKey marks the one bobFindMember failure its callers act on: the same
// top-level key present twice. Every other failure is a tokenizer error that readSettings
// describes better. A sentinel rather than a substring of the message, so rewording the
// sentence cannot quietly disable the gate in bobDuplicateKey.
var errBobDuplicateKey = errors.New("duplicate top-level key")

// bobFindMember locates the byte span of a top-level member, from the opening quote
// of its name through the last byte of its value. Offsets come from json.Decoder,
// so they are the tokenizer's view of the document and not a textual guess.
//
// A document may legally name the same key twice, and both Go and VS Code take the
// LAST one. Splicing out a single span cannot express "remove both", and removing
// either one alone leaves the key still set while every message this command prints
// says it is gone — so this refuses instead, by name. The whole document is scanned
// rather than returning at the first match, which is what makes the second occurrence
// visible at all.
func bobFindMember(src []byte, key string) (start, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, fmt.Errorf("top level is not a JSON object")
	}
	for dec.More() {
		// InputOffset before reading the name is the offset just past the previous
		// token, so skip whitespace forward to the quote that opens this name.
		nameStart := int(dec.InputOffset())
		for nameStart < len(src) && src[nameStart] != '"' {
			nameStart++
		}
		name, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		// Reading the value advances the offset to just past it, which is the end of
		// the whole member. Decoding into json.RawMessage consumes a value of any
		// shape — object, array, scalar — in one step.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		if name == key {
			if found {
				// Wrapped, not just worded: bobDuplicateKey classifies this verdict and
				// used to do it by searching the message text, so rewording the sentence
				// below silently turned that gate off. errors.Is cannot drift that way.
				return 0, 0, false, fmt.Errorf("%w: %q appears more than once at the top "+
					"level; remove the duplicate first — this command edits one member and "+
					"cannot say which of them wins", errBobDuplicateKey, key)
			}
			start, end, found = nameStart, int(dec.InputOffset()), true
		}
	}
	return start, end, found, nil
}

// bobTailIsOnlySeparator reports whether what follows a member on its own line is
// nothing but a comma and whitespace — i.e. no other key shares the line to the right.
//
// Separate from a plain TrimSpace check because the member's own trailing comma is
// part of the separator and not a neighbour.
func bobTailIsOnlySeparator(tail []byte) bool {
	t := bytes.TrimSpace(tail)
	if len(t) > 0 && t[0] == ',' {
		t = bytes.TrimSpace(t[1:])
	}
	return len(t) == 0
}

// bobSpliceOut removes a member and exactly one of the commas around it, leaving the
// surrounding layout intact.
//
// The subtlety is which whitespace goes with the member. Taking only the member's own
// bytes leaves its indentation behind as a line of trailing spaces — invisible in a
// terminal, visible in a diff and to every whitespace linter. So the span removed runs
// from the start of the member's own LINE (its leading indentation) to just past the
// newline that ends it. A blank line the user wrote as a grouping separator sits before
// that indentation and is therefore kept.
//
// The comma is the other half: it goes on whichever side has one, preferring the
// FOLLOWING comma so that removing the last member does not leave a trailing comma,
// which is invalid JSON.
func bobSpliceOut(src []byte, start, end int) []byte {
	// Is the member alone on its line? Only then can the line be taken whole — and
	// "alone" means on BOTH sides. A member that is first on its line but shares it
	// with a later key still may not have its line's leading indentation removed:
	// that indentation belongs to the key that survives. Checking only the left side
	// left `{\n    "http.proxy": ..., "b": 2,\n` as `{\n "b": 2,` — the indent gone
	// and one stray space in its place.
	lineStart := bytes.LastIndexByte(src[:start], '\n') + 1
	// Scanned from end, the end of the VALUE, not from start, the start of the key. A
	// member may straddle a line break — `"http.proxy":\n    "http://..."` is valid
	// JSON and VS Code's own formatter produces it on a long value — and then the first
	// newline after the key is INSIDE the member. Measuring from start there yields a
	// lineEnd below end, and the src[end:lineEnd] slice below panicked on a file that
	// was never malformed: `slice bounds out of range [79:46]`, from `configure bob
	// disable`, after it had already printed that it was keeping a .bak.
	lineEnd := bytes.IndexByte(src[end:], '\n')
	if lineEnd < 0 {
		lineEnd = len(src)
	} else {
		lineEnd += end
	}
	ownLine := len(bytes.TrimSpace(src[lineStart:start])) == 0 &&
		bobTailIsOnlySeparator(src[end:lineEnd])

	// Walk forward past whitespace to a following comma, if there is one.
	after := end
	for after < len(src) && isBobSpace(src[after]) {
		after++
	}

	from, to := start, end
	if after < len(src) && src[after] == ',' {
		// Not the last member: take the member, the comma, and — only when the member
		// had the line to itself — the rest of that line including the newline, so the
		// next member keeps its own indentation.
		//
		// The ownLine guard is load-bearing. A member SHARING a line with another key
		// still has a blank line-tail after its comma when the next key is on the line
		// below, so without the guard this arm consumed that newline and the following
		// line's indentation too, welding two lines together:
		//
		//   {"a": 1,\n    "b": 2, "http.proxy": "...",\n    "c": 3}
		//     became   ...\n    "b": 2,     "c": 3\n...
		//
		// which is exactly the layout change "Nothing else in the file changes" says
		// this function does not make. When the member shares its line, only its own
		// bytes and its comma may go.
		to = after + 1
		if nl := bytes.IndexByte(src[to:], '\n'); ownLine && nl >= 0 && len(bytes.TrimSpace(src[to:to+nl])) == 0 {
			to += nl + 1
		} else if !ownLine {
			// The member shares its line — either a single-line document,
			// `{"a": 1, "http.proxy": "...", "b": 2}`, or one line of a multi-line one.
			// Its own line cannot be taken, and the space that separated this member
			// from what follows would be left beside the space after the previous
			// comma, making a double space. Take one of them.
			for to < len(src) && src[to] == ' ' {
				to++
			}
			// Nothing but the line's end follows: the space just taken was the only
			// thing between the comma and the newline, so the comma we absorbed leaves
			// a trailing space behind instead. Walk BACK over the spaces before the
			// member so the surviving key ends at its own comma. Trailing whitespace is
			// invisible in a terminal and loud in a diff, and it is what
			// TestBobDisable_LeavesNoStrayWhitespace forbids.
			if to < len(src) && src[to] == '\n' {
				for from > 0 && src[from-1] == ' ' {
					from--
				}
			}
		}
		if ownLine {
			from = lineStart
			// A member sitting BETWEEN two separators leaves both behind once it is
			// gone, and two blank lines where the user wrote one is a change to the
			// file's layout — the thing this function exists not to make. So when the
			// bytes on each side of the removed line are both separators of the same
			// kind, one of them goes with it. Same reasoning one line down for the
			// single-line-document form, where the separator is a space rather than a
			// newline and removing a middle member would leave a double space.
			from = bobAbsorbSeparator(src, from, to)
		}
	} else {
		// Last member: take the preceding comma and the whitespace between it and us,
		// so the member that becomes last does not end with a dangling comma.
		for from > 0 && isBobSpace(src[from-1]) {
			from--
		}
		if from > 0 && src[from-1] == ',' {
			from--
		}
		// Anything the user left between the value and the line's end goes too. It is
		// the member's own tail, but walking back to the preceding comma moves the cut
		// ABOVE it, so leaving it behind strands it on the line that survives:
		// `"a": 1,\n    "http.proxy": "..." \n}` became `"a": 1 \n}` — a trailing space
		// on a line the user did not touch. Only spaces and tabs, and only as far as
		// the newline: a following blank line is the user's own grouping, and the
		// ownLine arm above already owns the newline case.
		for to < len(src) && (src[to] == ' ' || src[to] == '\t') {
			to++
		}
	}

	out := make([]byte, 0, len(src)-(to-from))
	out = append(out, src[:from]...)
	out = append(out, src[to:]...)
	return out
}

// bobInsertMember adds a member after the last existing one, matching that member's
// own indentation so the insertion looks hand-written rather than appended.
func bobInsertMember(src []byte, member []byte) ([]byte, error) {
	// The closing brace of the top-level object is the last '}' in the document.
	closing := bytes.LastIndexByte(src, '}')
	if closing < 0 {
		return nil, fmt.Errorf("top level is not a JSON object")
	}

	// Is the object empty? Then there is no last member to follow.
	trimmed := bytes.TrimSpace(src[:closing])
	empty := bytes.HasSuffix(trimmed, []byte("{"))

	// End of the last member, which is where the comma and the new line go.
	insertAt := closing
	for insertAt > 0 && isBobSpace(src[insertAt-1]) {
		insertAt--
	}

	// The indentation to use: the LAST MEMBER's own, not the closing brace's. Copying
	// the brace's line is the obvious-looking choice and it is wrong — in a
	// conventionally formatted file that line is column 0, so every inserted key
	// landed unindented while every existing one was indented. Read it from where the
	// last member starts instead, which is the line the new member will sit beside.
	indent := bobIndentOf(src, insertAt)
	if indent == "" {
		// Either an empty object, or a single-line document with no indentation to
		// copy. Two spaces is the only width this function ever invents, and only
		// when the file itself shows none.
		indent = "  "
	}

	var ins []byte
	if empty {
		ins = append(ins, '\n')
		ins = append(ins, indent...)
		ins = append(ins, member...)
		ins = append(ins, '\n')
	} else {
		ins = append(ins, ',', '\n')
		ins = append(ins, indent...)
		ins = append(ins, member...)
	}

	out := make([]byte, 0, len(src)+len(ins))
	out = append(out, src[:insertAt]...)
	out = append(out, ins...)
	out = append(out, src[insertAt:]...)
	return out, nil
}

// bobIndentOf returns the leading whitespace of the line containing off.
func bobIndentOf(src []byte, off int) string {
	lineStart := bytes.LastIndexByte(src[:off], '\n') + 1
	i := lineStart
	for i < off && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[lineStart:i])
}

// bobAbsorbSeparator extends a removal backward over one blank line when the removed
// span has a blank line on both sides, so a grouping separator is not duplicated.
func bobAbsorbSeparator(src []byte, from, to int) int {
	// Is what follows the removed span a blank line?
	nl := bytes.IndexByte(src[to:], '\n')
	if nl < 0 || len(bytes.TrimSpace(src[to:to+nl])) != 0 {
		return from
	}
	// Is what precedes it also one? The from == 0 test comes first because the walk
	// below slices src[:from-1], which panics on from == 0 rather than reporting it.
	// Unreachable today — every caller passes a from inside an object, so at least the
	// opening brace precedes it — which is exactly why the order is worth fixing now
	// rather than after a new caller makes it reachable.
	if from == 0 || src[from-1] != '\n' {
		return from
	}
	// Walk back over the previous line.
	prevStart := bytes.LastIndexByte(src[:from-1], '\n') + 1
	if len(bytes.TrimSpace(src[prevStart:from-1])) != 0 {
		return from
	}
	return prevStart
}

func isBobSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// bobEdit is one key bobWriteKeys sets, or deletes when value is nil.
type bobEdit struct {
	key   string
	value any
}

// bobWriteKey is bobWriteKeys for a single key.
func bobWriteKey(path, key string, value any) error {
	return bobWriteKeys(path, []bobEdit{{key, value}})
}

// bobWriteKeys is bob's replacement for writeSettings: same backup and atomic-rename
// behaviour, but the content is the original file with the given keys edited rather
// than a re-marshal of the whole document.
//
// Every edit lands in ONE write. enable sets http.proxy and http.systemCertificates
// together, and two writes would leave a window — or, on a failed second write, a
// file — holding one without the other.
func bobWriteKeys(path string, edits []bobEdit) error {
	src, rerr := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if rerr != nil {
		if !os.IsNotExist(rerr) {
			return rerr
		}
		// No file yet: start from an empty object so the insert path has a document
		// to work on. Nothing to back up either. This is a first enable on a Bob that
		// has never saved a setting — bobNoDocument has already checked that Bob's
		// User directory is there.
		src = []byte("{}\n")
	} else {
		// Back the file up ONCE and never overwrite it, matching writeSettings: a
		// second enable, or an enable/disable pair, must not replace the pristine
		// pre-Cortex file with one agentop already edited.
		if berr := backupOnce(path, src); berr != nil {
			return berr
		}
	}

	// An empty or whitespace-only file is an empty document, the same reading
	// readSettings gives it.
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}\n")
	}

	out := src
	for _, e := range edits {
		var err error
		if out, err = bobSetKey(out, e.key, e.value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	// Never hand back something that is not valid JSON, whatever the splicing did.
	// Bob reads this file; a malformed one is worse than a reformatted one.
	var check map[string]any
	if jerr := json.Unmarshal(out, &check); jerr != nil {
		return fmt.Errorf("%s: edit would produce invalid JSON (%w); file left unchanged", path, jerr)
	}

	// Atomic and symlink-resolving; see writeSettings for why the fixed ".tmp"
	// name had to go (#1565).
	// 0600: this file commonly holds API tokens in the same block.
	return writeFileAtomic(path, out, 0o600)
}
