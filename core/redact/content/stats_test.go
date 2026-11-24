package content

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// THE STATISTICAL TESTS of the token shape: random tokens of each common alphabet and length must
// be found, and identifiers made of words must be kept, each at 99% or better. Both use a fixed
// seed, so they are deterministic.

// tokenAlphabets is a slice, not a map, so the order (and with it each cell's random stream) is
// the same on every run.
var tokenAlphabets = []struct{ name, chars string }{
	{"hex", "0123456789abcdef"},
	{"lower+digit", "abcdefghijklmnopqrstuvwxyz0123456789"},
	{"alnum", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
	{"base64", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"},
	{"base64url", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"},
	{"lower", "abcdefghijklmnopqrstuvwxyz"},
	{"mixed-case letters", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"},
}

func randomToken(rng *rand.Rand, alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

func TestTokenShape_RandomTokensAreFound(t *testing.T) {
	const trials = 1000
	for ai, a := range tokenAlphabets {
		lengths := []int{16, 20, 24, 32, 40, 64}
		if a.name == "lower" {
			lengths = []int{32, 40, 64} // one kind of character: only long runs are tokens
		}
		for _, n := range lengths {
			rng := rand.New(rand.NewPCG(uint64(ai+1), uint64(n))) // one stream per cell
			found := 0
			for range trials {
				if secretShaped(tierSecret, randomToken(rng, a.chars, n), valueBare) {
					found++
				}
			}
			if found*100 < trials*99 {
				t.Errorf("%s, %d characters: %d of %d random tokens found, want 99%%", a.name, n, found, trials)
			}
		}
	}
}

// identifierWords are words identifiers are made of.
var identifierWords = strings.Fields(`user profile settings config file path name id key cache value data
	request response session client server token secret auth login account email address phone list map
	item index count total max min default enable disabled status state event handler callback error
	message queue worker job task build test deploy release version update create delete get set load
	save read write open close start stop init main app api service model view controller database table
	column row field type kind group role permission access level mode flag option param input output`)

// randomIdentifier joins 2 to 5 words in one of the common styles, sometimes with a short
// version or number.
func randomIdentifier(rng *rand.Rand) string {
	n := 2 + rng.IntN(4)
	w := make([]string, n)
	for i := range w {
		w[i] = identifierWords[rng.IntN(len(identifierWords))]
	}
	switch rng.IntN(3) {
	case 0:
		w = append(w, []string{"v2", "1", "2024", "x86", "k8s"}[rng.IntN(5)])
	}
	title := func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
	switch rng.IntN(6) {
	case 0:
		return strings.Join(w, "_")
	case 1:
		return strings.Join(w, "-")
	case 2:
		return strings.Join(w, ".")
	case 3:
		return strings.ToUpper(strings.Join(w, "_"))
	case 4:
		for i := 1; i < len(w); i++ {
			w[i] = title(w[i])
		}
		return strings.Join(w, "")
	default:
		for i := range w {
			w[i] = title(w[i])
		}
		return strings.Join(w, "")
	}
}

func TestTokenShape_IdentifiersAreKept(t *testing.T) {
	const trials = 2000
	rng := rand.New(rand.NewPCG(3, 5))
	kept := 0
	var leaked []string
	for range trials {
		id := randomIdentifier(rng)
		if !secretShaped(tierSecret, id, valueBare) && !secretShaped(tierKey, id, valueBare) {
			kept++
		} else if len(leaked) < 10 {
			leaked = append(leaked, id)
		}
	}
	if kept*100 < trials*99 {
		t.Errorf("%d of %d identifiers kept, want 99%%; for example %q", kept, trials, leaked)
	}
}
