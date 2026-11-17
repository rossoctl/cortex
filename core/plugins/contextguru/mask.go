package contextguru

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
)

// credMaskKey masks a caller's credential while this plugin holds it between requests
// (keep-alive). Be precise about what this buys: a heap or core dump, a crash report, a
// `/proc/<pid>/mem` read or a stray `strings` over a snapshot yields masked bytes instead of a
// working key — the accidental-capture class, the realistic one. It does NOT stop an attacker with
// code execution in this process: the key is in the same address space. It is obfuscation at rest,
// not encryption.
//
// Private to this plugin: credential custody is each host's own business, so this is not shared
// with the engine's core or with the proxy host.
var credMaskKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the credential mask: " + err.Error())
	}
	return b
}()

// credMask masks or unmasks in place — XOR is its own inverse, so one function does both.
func credMask(b []byte) {
	for i := range b {
		b[i] ^= credMaskKey[i%len(credMaskKey)]
	}
}

// credZero overwrites a buffer. Dropping the reference is not enough: the bytes sit in the heap
// until a collection that may never come, and a dump taken in between still has them.
func credZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// heldHeader is one auth header held masked. The NAME is not a secret; the value is.
type heldHeader struct {
	name string
	val  []byte
}

func (h heldHeader) clone() heldHeader {
	return heldHeader{name: h.name, val: append([]byte(nil), h.val...)}
}

func zeroHeaders(hs []heldHeader) {
	for _, h := range hs {
		credZero(h.val)
	}
}

// credIDKey keys credID's HMAC: random per process, in memory only, never logged or stored.
var credIDKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the keep-alive credential id key: " + err.Error())
	}
	return b
}()

// credID is a per-process opaque id for the caller credential a request carried, for
// keepalive.Record.Cred. Cortex pings with the CALLER's own credential (cortexDispatcher.hold),
// so the keeper must be able to tell one caller's key from another's: with Cred "" the package
// treats the key as server-held and feeds a 401/403 to the route breaker, whose key is shared by
// every caller of that host, path and model — so one caller's revoked key would stop keep-alive
// for every other caller for an hour. With an id, a refusal marks only that credential dead.
//
// Computed as context-guru's proxy host computes its own (proxy/credmask.go credKey), without
// importing it: an HMAC-SHA256 under credIDKey over the auth slots (authHeaderNames), each value
// as `name\x00value`, sorted and joined with `\x01`, truncated to 16 bytes, hex. The values are
// hashed in their masked form (credMask, a bijection), so no further plaintext copy is made, and
// the masked copies are zeroed after. "" when the request carries no auth header. The id is never
// logged or stored by this plugin.
func credID(hdr http.Header) string {
	var parts [][]byte
	for _, name := range authHeaderNames {
		for _, v := range hdr.Values(name) {
			b := make([]byte, 0, len(name)+1+len(v))
			b = append(b, name...)
			b = append(b, 0)
			b = append(b, v...)
			credMask(b[len(name)+1:])
			parts = append(parts, b)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	defer func() {
		for _, b := range parts {
			credZero(b)
		}
	}()
	slices.SortFunc(parts, bytes.Compare)
	m := hmac.New(sha256.New, credIDKey)
	for i, b := range parts {
		if i > 0 {
			m.Write([]byte{1})
		}
		m.Write(b)
	}
	return hex.EncodeToString(m.Sum(nil)[:16])
}
