package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

const provenanceFixtureName = "agentop_v0.9.5_darwin_arm64.tar.gz"
const provenanceFixtureDigest = "6c6205e6ec0869a4c8291d7d9a0f0cca7c054d494619721085f9416595787461"

func provenanceFixture(t *testing.T) ([]json.RawMessage, root.TrustedMaterial, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/provenance/agentop_v0.9.5_darwin_arm64.attestations.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Attestations []struct{ Bundle json.RawMessage } `json:"attestations"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	var bundles []json.RawMessage
	for _, a := range resp.Attestations {
		bundles = append(bundles, a.Bundle)
	}
	rootJSON, err := os.ReadFile("testdata/provenance/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := root.NewTrustedRootFromJSON(rootJSON)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := hex.DecodeString(provenanceFixtureDigest)
	return bundles, tm, digest
}

func wantErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("error = %v, want one that contains %q", err, sub)
	}
}

func TestProvenanceAccepted(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	if err := verifyBundles(provenanceFixtureName, bundles, digest, releaseProvenance("v0.9.5"), tm); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceWrongDigest(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	digest[len(digest)-1] ^= 1
	wantErrContains(t, verifyBundles(provenanceFixtureName, bundles, digest, releaseProvenance("v0.9.5"), tm), "provenance of "+provenanceFixtureName)
}

func TestProvenanceWrongSANRepository(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	want := releaseProvenance("v0.9.5")
	want.san = strings.Replace(want.san, "rossoctl/cortex", "rossoctl/other", 1)
	wantErrContains(t, verifyBundles(provenanceFixtureName, bundles, digest, want, tm), "SAN")
}

func TestProvenanceWrongSANTag(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	wantErrContains(t, verifyBundles(provenanceFixtureName, bundles, digest, releaseProvenance("v0.9.6"), tm), "refs/tags/v0.9.6")
}

func TestProvenanceWrongIssuer(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	want := releaseProvenance("v0.9.5")
	want.issuer = "https://accounts.google.com"
	wantErrContains(t, verifyBundles(provenanceFixtureName, bundles, digest, want, tm), "provenance of ")
}

// changedSignature returns b with one bit of the DSSE signature flipped.
func changedSignature(t *testing.T, b json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	s := m["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
	sig, err := base64.StdEncoding.DecodeString(s["sig"].(string))
	if err != nil {
		t.Fatal(err)
	}
	sig[10] ^= 1
	s["sig"] = base64.StdEncoding.EncodeToString(sig)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProvenanceChangedSignature(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	bad := []json.RawMessage{changedSignature(t, bundles[0])}
	wantErrContains(t, verifyBundles(provenanceFixtureName, bad, digest, releaseProvenance("v0.9.5"), tm), "provenance of ")
}

func TestProvenanceSecondBundlePasses(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	two := []json.RawMessage{changedSignature(t, bundles[0]), bundles[0]}
	if err := verifyBundles(provenanceFixtureName, two, digest, releaseProvenance("v0.9.5"), tm); err != nil {
		t.Fatal(err)
	}
}

// attestationServer serves body for the fixture digest and 404 for any other path.
// It records the Authorization header of the last request.
func attestationServer(t *testing.T, status int, body []byte) (*httptest.Server, *string) {
	t.Helper()
	auth := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		if r.URL.Path != "/sha256:"+provenanceFixtureDigest {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := attestationsAPI
	attestationsAPI = srv.URL + "/"
	t.Cleanup(func() { attestationsAPI = old })
	return srv, auth
}

func TestFetchAttestations(t *testing.T) {
	fixture, err := os.ReadFile("testdata/provenance/agentop_v0.9.5_darwin_arm64.attestations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _, digest := provenanceFixture(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, gh, github, want string }{
		{"GH_TOKEN wins", "a", "b", "Bearer a"},
		{"GITHUB_TOKEN only", "", "b", "Bearer b"},
		{"no token", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			_, auth := attestationServer(t, http.StatusOK, fixture)
			got, err := fetchAttestations(ctx, provenanceFixtureName, digest)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d bundles, want 1", len(got))
			}
			if *auth != tc.want {
				t.Fatalf("Authorization = %q, want %q", *auth, tc.want)
			}
		})
	}

	t.Run("404", func(t *testing.T) {
		attestationServer(t, http.StatusNotFound, nil)
		_, err := fetchAttestations(ctx, provenanceFixtureName, digest)
		wantErrContains(t, err, "no attestation for "+provenanceFixtureName)
	})
	t.Run("empty list", func(t *testing.T) {
		attestationServer(t, http.StatusOK, []byte(`{"attestations":[]}`))
		_, err := fetchAttestations(ctx, provenanceFixtureName, digest)
		wantErrContains(t, err, "no attestation for "+provenanceFixtureName)
	})
	t.Run("500", func(t *testing.T) {
		attestationServer(t, http.StatusInternalServerError, nil)
		_, err := fetchAttestations(ctx, provenanceFixtureName, digest)
		wantErrContains(t, err, "GitHub attestation API answered 500 Internal Server Error for "+provenanceFixtureName)
	})
	t.Run("bundle_url only", func(t *testing.T) {
		attestationServer(t, http.StatusOK, []byte(`{"attestations":[{"bundle":null,"bundle_url":"x"}]}`))
		_, err := fetchAttestations(ctx, provenanceFixtureName, digest)
		wantErrContains(t, err, "attestation for "+provenanceFixtureName+" has no inline bundle")
	})
	t.Run("closed server", func(t *testing.T) {
		srv, _ := attestationServer(t, http.StatusOK, fixture)
		srv.Close()
		_, err := fetchAttestations(ctx, provenanceFixtureName, digest)
		wantErrContains(t, err, "could not reach the GitHub attestation API")
		wantErrContains(t, err, srv.URL)
	})
}

func TestCheckProvenanceEndToEnd(t *testing.T) {
	fixture, err := os.ReadFile("testdata/provenance/agentop_v0.9.5_darwin_arm64.attestations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, tm, digest := provenanceFixture(t)
	attestationServer(t, http.StatusOK, fixture)
	old := provenanceTrustedMaterial
	t.Cleanup(func() { provenanceTrustedMaterial = old })
	provenanceTrustedMaterial = func() (root.TrustedMaterial, error) { return tm, nil }
	ctx := context.Background()

	if err := checkProvenance(ctx, provenanceFixtureName, digest, "v0.9.5"); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, checkProvenance(ctx, provenanceFixtureName, digest, "v0.9.6"), "refs/tags/v0.9.6")

	provenanceTrustedMaterial = func() (root.TrustedMaterial, error) { return nil, errors.New("tuf down") }
	wantErrContains(t, checkProvenance(ctx, provenanceFixtureName, digest, "v0.9.5"), "Sigstore trusted root")
}

// A TUF server that accepts the connection and never answers must not hang the update.
func TestProvenanceTrustedMaterialTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	t.Setenv("HOME", t.TempDir()) // an empty TUF cache, so the client must fetch
	oldMirror, oldTimeout := tufMirror, tufFetchTimeout
	t.Cleanup(func() { tufMirror, tufFetchTimeout = oldMirror, oldTimeout })
	tufMirror, tufFetchTimeout = srv.URL, 200*time.Millisecond

	done := make(chan error, 1)
	go func() { _, err := provenanceTrustedMaterial(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("trusted root loaded from a server that never answers")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("trusted root fetch did not time out")
	}
}

// A statement of another predicate type (an SBOM, for example) from the same workflow is refused.
func TestProvenanceWrongPredicateType(t *testing.T) {
	bundles, tm, digest := provenanceFixture(t)
	old := slsaProvenanceV1
	t.Cleanup(func() { slsaProvenanceV1 = old })
	slsaProvenanceV1 = "https://spdx.dev/Document/v2.3"
	wantErrContains(t, verifyBundles(provenanceFixtureName, bundles, digest, releaseProvenance("v0.9.5"), tm), "attestation is not SLSA build provenance v1")
}

func TestFetchAttestationsSkipsURLOnlyEntries(t *testing.T) {
	fixture, err := os.ReadFile("testdata/provenance/agentop_v0.9.5_darwin_arm64.attestations.json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Attestations []json.RawMessage `json:"attestations"`
	}
	if err := json.Unmarshal(fixture, &body); err != nil {
		t.Fatal(err)
	}
	mixed := []byte(`{"attestations":[{"bundle":null,"bundle_url":"x"},` + string(body.Attestations[0]) + `]}`)
	attestationServer(t, http.StatusOK, mixed)
	_, _, digest := provenanceFixture(t)
	got, err := fetchAttestations(context.Background(), provenanceFixtureName, digest)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d bundles, %v; want the 1 inline bundle", len(got), err)
	}
}

func TestFetchAttestationsHints(t *testing.T) {
	_, _, digest := provenanceFixture(t)
	for _, tc := range []struct {
		status int
		token  string
		want   string
	}{
		{http.StatusUnauthorized, "stale", "refresh it, or unset it"},
		{http.StatusForbidden, "", "set GH_TOKEN to raise it"},
		{http.StatusTooManyRequests, "", "set GH_TOKEN to raise it"},
	} {
		t.Setenv("GH_TOKEN", tc.token)
		t.Setenv("GITHUB_TOKEN", "")
		attestationServer(t, tc.status, nil)
		_, err := fetchAttestations(context.Background(), provenanceFixtureName, digest)
		wantErrContains(t, err, tc.want)
	}
}
