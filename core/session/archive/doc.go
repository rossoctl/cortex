// Package archive persists sessions to disk so they survive a restart and the store's own
// eviction, and so agentop can browse them afterwards. It is the session archive of
// docs/superpowers/specs/2026-10-01-session-archive-design.md.
//
// IT EXISTS BECAUSE NAIVE PERSISTENCE IS NOT AFFORDABLE. An LLM request re-sends the whole
// conversation, so the store keeps one ever-larger copy of it per request — the duplication
// core/session/intern.go removes in memory. Written out as-is, 7,066 live events were 337MB.
// This package removes the same duplication on disk, then compresses what is left: those
// events take 2.3MB, 146x smaller, with every one decoding back byte-identical.
//
// ON DISK: one directory per session id, holding append-only segments. A segment is one zstd
// stream of JSON lines; each line is either a string record or an event record, and an
// event's large strings — exactly the seven fields the interner shares — are written as
// references to string records earlier in the same segment. See format.go for the records,
// segment.go and reader.go for the stream, and retention.go for what gets deleted.
//
// Files rather than SQLite, on measured grounds: the engine changed the size by about 3%, a
// pure-Go SQLite driver adds 4MB to every binary while zstd adds nothing (it is already
// linked), and only a stream can compress across events.
package archive
