package parsercommon

import "bytes"

// NormalizeSSE puts a buffered SSE body into the one shape the line scanners read: no
// leading byte-order mark, and LF line endings.
//
// BOTH HALVES ARE WIRE-LEGAL AND BOTH WERE MISSED. The event-stream format allows CRLF,
// LF or CR as the line terminator and requires a decoder to strip one leading BOM —
// while detection looked for "data:" after trimming " \t\r\n" (which does not include a
// BOM) or for a literal "\ndata:" (which a CR-only stream never contains), and the
// parsers split on LF alone. So a BOM-prefixed body, or a CR-only body whose first field
// is not `data:`, matched no data: line at all.
//
// SHARED BECAUSE THE MISS WAS A COPY PATTERN (#1567): inference-parser had this fix and
// the a2a and mcp parsers did not — a BOM-prefixed or CR-only MCP/A2A stream left the
// extension empty, and IBAC treats an unclassified exchange as one that passes by
// default. Detection and parsing use the same function deliberately: fixing only a
// detector would route such a body to a parser that still cannot read it, which trades
// a silent miss for a silent empty parse.
//
// Allocation-free for the overwhelming majority: a body with no BOM and no CR is
// returned as it came.
func NormalizeSSE(body []byte) []byte {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if !bytes.ContainsRune(body, '\r') {
		return body
	}
	// WHAT IS LOAD-BEARING HERE, MEASURED RATHER THAN ASSUMED. The BOM strip and the
	// CR-only replacement are: a CR-only body is ONE line to bytes.Split(body, "\n"), so
	// every `data:` prefix check misses it, and a leading BOM defeats the same check on
	// the first line.
	//
	// THE CRLF-FIRST ORDERING IS NOT. The scanners are line-based and TrimSpace each
	// line, so a trailing \r is already handled — dropping this replacement entirely,
	// leaving CR->LF alone, changes no observable behaviour (mutation-tested on the
	// inference parsers). It stays as defence in depth for a future consumer that splits
	// on a blank line rather than scanning lines, where CRLF collapsed by CR->LF alone
	// WOULD become a blank line between every field.
	out := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
}
