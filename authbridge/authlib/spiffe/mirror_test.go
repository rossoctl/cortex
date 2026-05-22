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

// eventually polls fn until it returns nil, or fails the test after timeout.
func eventually(t *testing.T, fn func() error, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("eventually: timeout after %s, last err: %v", timeout, lastErr)
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	eventually(t, func() error {
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Size() == 0 {
			return errors.New("file is empty")
		}
		return nil
	}, timeout)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.run(ctx)
		close(done)
	}()

	for _, name := range []string{"svid.pem", "svid_key.pem", "svid_bundle.pem"} {
		waitFile(t, filepath.Join(dir, name), 2*time.Second)
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

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror.run did not exit after ctx cancel")
	}
	_ = td
}

func TestRunJWTMirror_WritesAndRefreshes(t *testing.T) {
	const audience = "https://keycloak/realms/test"

	// Two JWT SVIDs: first expires soon (forces a fast refresh), second
	// has a longer expiry so the refresh sleep is observable.
	jwt1 := makeJWTSVID(t, audience)
	jwt1.Expiry = time.Now().Add(200 * time.Millisecond)
	jwt2 := makeJWTSVID(t, audience)
	jwt2.Expiry = time.Now().Add(time.Hour)

	jwtFake := &fakeMirrorJWT{svids: []*jwtsvid.SVID{jwt1, jwt2}}

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runJWTMirror(ctx, jwtFake, audience, dir)
		close(done)
	}()

	tokenPath := filepath.Join(dir, "jwt_svid.token")
	waitFile(t, tokenPath, 2*time.Second)

	st1, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := st1.Mode().Perm(); got != 0o644 {
		t.Errorf("jwt_svid.token mode = %o, want 0o644", got)
	}

	// The first SVID expires in ~200ms → 90% lifetime ~= 180ms refresh.
	// Wait for at least two FetchJWTSVID calls.
	eventually(t, func() error {
		jwtFake.mu.Lock()
		c := jwtFake.calls
		jwtFake.mu.Unlock()
		if c < 2 {
			return errors.New("not enough fetches yet")
		}
		return nil
	}, 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runJWTMirror did not exit after ctx cancel")
	}
}

func TestMirror_GoroutineExitsOnCancel(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	id := spiffeid.RequireFromString("spiffe://example.org/workload")
	svid, bundle := generateSVIDAndBundle(t, id)
	_ = td

	x509Fake := &fakeMirrorX509{
		svid:    svid,
		bundle:  bundle,
		updated: make(chan struct{}, 1),
	}

	dir := t.TempDir()
	m := newMirror(mirrorConfig{
		Dir:  dir,
		X509: x509Fake,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.run(ctx)
		close(done)
	}()

	// Wait for at least one X.509 write so we know the goroutine started.
	waitFile(t, filepath.Join(dir, "svid.pem"), 2*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror.run did not exit within 1s of ctx cancel")
	}
}
