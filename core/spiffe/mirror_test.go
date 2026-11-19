package spiffe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

func TestAtomicWrite_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "contents" {
		t.Errorf("got %q, want %q", got, "contents")
	}
}

func TestAtomicWrite_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	_ = os.WriteFile(path, []byte("old"), 0o644)
	if err := atomicWrite(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestAtomicWrite_NoTempLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(entries), entries)
	}
}

// fakeMirrorX509 is a hand-rolled implementation of mirrorX509Source. We
// can't pass a *workloadapi.X509Source directly because there's no public
// way to inject SVIDs into it without spinning up a workload API gRPC
// server; the seam interface lets us substitute this fake in tests.
type fakeMirrorX509 struct {
	mu      sync.Mutex
	svid    *x509svid.SVID
	svidErr error

	bundle    *x509bundle.Bundle
	bundleErr error

	updated chan struct{}
}

func (f *fakeMirrorX509) GetX509SVID() (*x509svid.SVID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.svidErr != nil {
		return nil, f.svidErr
	}
	return f.svid, nil
}

func (f *fakeMirrorX509) GetX509BundleForTrustDomain(_ spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bundleErr != nil {
		return nil, f.bundleErr
	}
	return f.bundle, nil
}

func (f *fakeMirrorX509) Updated() <-chan struct{} {
	return f.updated
}

// fakeMirrorJWT implements mirrorJWTSource, returning successive SVIDs
// from a list each call. After the list is exhausted, the last SVID is
// reused.
type fakeMirrorJWT struct {
	mu     sync.Mutex
	svids  []*jwtsvid.SVID
	calls  int
	err    error
	wakeup chan struct{}
}

func (f *fakeMirrorJWT) FetchJWTSVID(_ context.Context, _ jwtsvid.Params) (*jwtsvid.SVID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.svids) == 0 {
		return nil, errors.New("no svids configured")
	}
	idx := f.calls - 1
	if idx >= len(f.svids) {
		idx = len(f.svids) - 1
	}
	return f.svids[idx], nil
}

func (f *fakeMirrorJWT) Updated() <-chan struct{} {
	if f.wakeup == nil {
		f.wakeup = make(chan struct{})
	}
	return f.wakeup
}

// requireFile fails the test unless path exists and is non-empty.
func requireFile(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if st.Size() == 0 {
		t.Fatalf("%s is empty", path)
	}
}

func TestMirror_X509_WritesAllThreeFiles(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	id := spiffeid.RequireFromString("spiffe://example.org/workload")
	svid, bundle := generateSVIDAndBundle(t, id)

	fake := &fakeMirrorX509{
		svid:    svid,
		bundle:  bundle,
		updated: make(chan struct{}, 1),
	}

	dir := t.TempDir()
	m := newMirror(mirrorConfig{
		Dir:  dir,
		X509: fake,
	})

	// run writes before it waits for a rotation, and returns on a cancelled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.run(ctx)

	for _, name := range []string{"svid.pem", "svid_key.pem", "svid_bundle.pem"} {
		requireFile(t, filepath.Join(dir, name))
	}

	// JWT not configured: must NOT be written.
	if _, err := os.Stat(filepath.Join(dir, "jwt_svid.token")); !os.IsNotExist(err) {
		t.Errorf("expected jwt_svid.token to be absent, err=%v", err)
	}

	// Verify file modes.
	checkMode := func(name string, want os.FileMode) {
		t.Helper()
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
	checkMode("svid.pem", 0o644)
	checkMode("svid_key.pem", 0o600)
	checkMode("svid_bundle.pem", 0o644)
	_ = td
}

func TestRunJWTMirror_WritesAndRefreshes(t *testing.T) {
	const audience = "https://keycloak/realms/test"

	// Two JWT SVIDs, so the refresh writes a different token than the first write.
	jwt1 := makeJWTSVID(t, audience)
	jwt1.Expiry = time.Now().Add(time.Hour)
	jwt2 := makeJWTSVID(t, audience)
	jwt2.Expiry = time.Now().Add(2 * time.Hour)

	jwtFake := &fakeMirrorJWT{svids: []*jwtsvid.SVID{jwt1, jwt2}}

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// after reports each delay the loop waits out, after the write before it, and
	// fires when the test sends on tick.
	waits := make(chan time.Duration)
	tick := make(chan time.Time)
	after := func(d time.Duration) <-chan time.Time {
		waits <- d
		return tick
	}

	done := make(chan struct{})
	go func() {
		runJWTMirror(ctx, jwtFake, audience, dir, after)
		close(done)
	}()

	tokenPath := filepath.Join(dir, "jwt_svid.token")
	if d := <-waits; d <= 53*time.Minute || d > 54*time.Minute {
		t.Errorf("refresh after %v, want 90%% of the token's hour", d)
	}
	requireFile(t, tokenPath)
	st1, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := st1.Mode().Perm(); got != 0o644 {
		t.Errorf("jwt_svid.token mode = %o, want 0o644", got)
	}

	tick <- time.Now()
	<-waits
	jwtFake.mu.Lock()
	c := jwtFake.calls
	jwtFake.mu.Unlock()
	if c != 2 {
		t.Errorf("FetchJWTSVID called %d times, want 2: the first write and one refresh", c)
	}
	if got, err := os.ReadFile(tokenPath); err != nil || string(got) != jwt2.Marshal() {
		t.Errorf("token after the refresh = %q, %v; want the second SVID", got, err)
	}

	cancel()
	<-done
}
