package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

// attestationsAPI is the GitHub attestations endpoint; the key is "sha256:<hex>".
var attestationsAPI = "https://api.github.com/repos/rossoctl/cortex/attestations/"

// provenanceTrustedMaterial loads the Sigstore public-good trusted root (TUF, cached under ~/.sigstore).
var provenanceTrustedMaterial = func() (root.TrustedMaterial, error) {
	// The TUF fetch takes no context, its default client has no timeout, and
	// installRelease catches Ctrl-C: without this a stalled connection hangs the update.
	opts := tuf.DefaultOptions()
	opts.RepositoryBaseURL = tufMirror
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPClient(&http.Client{Timeout: tufFetchTimeout})
	opts.Fetcher = f
	return root.FetchTrustedRootWithOptions(opts)
}

// tufMirror and tufFetchTimeout are the TUF repository and the limit on each of its
// requests; a test points them at a stalled server.
var (
	tufMirror       = tuf.DefaultMirror
	tufFetchTimeout = 30 * time.Second
)

// provenanceWant is the certificate identity that a release attestation must have.
type provenanceWant struct{ issuer, san string }

// releaseProvenance is the identity of the release workflow run for tag.
func releaseProvenance(tag string) provenanceWant {
	return provenanceWant{
		issuer: "https://token.actions.githubusercontent.com",
		san:    "https://github.com/rossoctl/cortex/.github/workflows/release-binaries.yaml@refs/tags/" + tag,
	}
}

// fetchAttestations returns the inline bundles for the sha256 digest; name is only for messages.
func fetchAttestations(ctx context.Context, name string, digest []byte) ([]json.RawMessage, error) {
	url := attestationsAPI + "sha256:" + hex.EncodeToString(digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the GitHub attestation API (%s): %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no attestation for %s", name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub attestation API answered %s for %s", resp.Status, name)
	}
	var body struct {
		Attestations []struct {
			Bundle json.RawMessage `json:"bundle"`
		} `json:"attestations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("GitHub attestation API answer for %s: %w", name, err)
	}
	if len(body.Attestations) == 0 {
		return nil, fmt.Errorf("no attestation for %s", name)
	}
	bundles := make([]json.RawMessage, 0, len(body.Attestations))
	for _, a := range body.Attestations {
		// shortcut: a bundle_url-only answer is refused (snappy blob, new dependency); upgrade when GitHub stops inlining bundles.
		if len(a.Bundle) == 0 || string(a.Bundle) == "null" {
			return nil, fmt.Errorf("attestation for %s has no inline bundle", name)
		}
		bundles = append(bundles, a.Bundle)
	}
	return bundles, nil
}

// verifyBundles is nil when at least one bundle passes the policy for digest.
func verifyBundles(name string, bundles []json.RawMessage, digest []byte, want provenanceWant, tm root.TrustedMaterial) error {
	if len(bundles) == 0 {
		return fmt.Errorf("no attestation for %s", name)
	}
	v, err := verify.NewVerifier(tm, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("provenance of %s: %w", name, err)
	}
	id, err := verify.NewShortCertificateIdentity(want.issuer, "", want.san, "")
	if err != nil {
		return fmt.Errorf("provenance of %s: %w", name, err)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))
	var errs []error
	for _, raw := range bundles {
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := v.Verify(&b, policy); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	return fmt.Errorf("provenance of %s: %w", name, errors.Join(errs...))
}

// checkProvenance accepts the release tarball name with this sha256 digest only when
// GitHub holds an attestation for it from the release workflow run of tag.
func checkProvenance(ctx context.Context, name string, digest []byte, tag string) error {
	bundles, err := fetchAttestations(ctx, name, digest)
	if err != nil {
		return err
	}
	tm, err := provenanceTrustedMaterial()
	if err != nil {
		return fmt.Errorf("could not load the Sigstore trusted root: %w", err)
	}
	return verifyBundles(name, bundles, digest, releaseProvenance(tag), tm)
}
