package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var releaseDownloadBase = "https://github.com/rossoctl/cortex/releases/download/"

// releaseClient downloads release assets (D10).
var releaseClient = &http.Client{Timeout: 5 * time.Minute}

// Size limits for a release download. GitHub caps a release asset at 2 GiB.
var (
	maxChecksumsBytes int64 = 1 << 20
	maxTarballBytes   int64 = 2 << 30
)

// releaseTarballs is the agentop tarball, then the cortex one, then cortex-gateway when praxis.
func releaseTarballs(tag, goos, goarch string, praxis bool) []string {
	suffix := "_" + tag + "_" + goos + "_" + goarch + ".tar.gz"
	names := []string{"agentop" + suffix, proxyBinary + suffix}
	if praxis {
		names = append(names, "cortex-gateway"+suffix)
	}
	return names
}

// tarballBinary is the binary a release tarball must hold.
func tarballBinary(name string) string {
	switch {
	case strings.HasPrefix(name, "agentop_"):
		return "agentop"
	case strings.HasPrefix(name, "cortex-gateway_"):
		return "cortex-gateway"
	default: // cortex_
		return proxyBinary
	}
}

// checksumFor is the lower-case hex SHA-256 for name in a checksums.txt body.
func checksumFor(sums []byte, name string) (string, error) {
	re := regexp.MustCompile(`(?m)^([0-9a-fA-F]{64})[ \t]+\*?(?:\./)?` + regexp.QuoteMeta(name) + `\r?$`)
	m := re.FindAllSubmatch(sums, -1)
	switch len(m) {
	case 0:
		return "", fmt.Errorf("checksums.txt has no usable entry for %s", name)
	case 1:
		return strings.ToLower(string(m[0][1])), nil
	default:
		return "", fmt.Errorf("checksums.txt lists %s more than once", name)
	}
}

// download writes the release asset name of tag to w. It refuses an asset of more than limit bytes.
func download(ctx context.Context, tag, name string, limit int64, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseDownloadBase+tag+"/"+name, nil)
	if err != nil {
		return 0, fmt.Errorf("download failed: %s: %w", name, err)
	}
	resp, err := releaseClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download failed: %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download failed: %s: %s", name, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, fmt.Errorf("download failed: %s: %w", name, err)
	}
	if n > limit {
		return n, fmt.Errorf("download failed: %s is larger than %d bytes", name, limit)
	}
	return n, nil
}

// downloadTarball saves name into stage and returns its SHA-256 and size.
func downloadTarball(ctx context.Context, tag, name, stage string) ([]byte, int64, error) {
	f, err := os.OpenFile(filepath.Join(stage, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, err
	}
	h := sha256.New()
	n, err := download(ctx, tag, name, maxTarballBytes, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return h.Sum(nil), n, err
}

// stageRelease downloads names for tag into a new stage (D4, D5). size is the
// total tarball bytes. On error it removes the stage and returns stage "".
func stageRelease(ctx context.Context, tag string, names []string, check func(name string, digest []byte) error) (stage string, size int64, err error) {
	stage, err = os.MkdirTemp("", "cortex-stage.")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
			stage, size = "", 0
		}
	}()
	var sums bytes.Buffer
	if _, err = download(ctx, tag, "checksums.txt", maxChecksumsBytes, &sums); err != nil {
		return
	}
	digests := make([][]byte, len(names))
	for i, name := range names {
		var want string
		if want, err = checksumFor(sums.Bytes(), name); err != nil {
			return
		}
		var n int64
		if digests[i], n, err = downloadTarball(ctx, tag, name, stage); err != nil {
			return
		}
		size += n
		if hex.EncodeToString(digests[i]) != want {
			err = fmt.Errorf("checksum mismatch for %s", name)
			return
		}
	}
	for i, name := range names {
		if err = check(name, digests[i]); err != nil {
			return
		}
	}
	for _, name := range names {
		if err = extractTarball(filepath.Join(stage, name), stage); err != nil {
			return
		}
		bin := filepath.Join(stage, tarballBinary(name))
		if fi, lerr := os.Lstat(bin); lerr != nil || !fi.Mode().IsRegular() {
			err = fmt.Errorf("archive did not contain expected binary: %s", tarballBinary(name))
			return
		}
		if err = os.Chmod(bin, 0o755); err != nil {
			return
		}
	}
	return stage, size, nil
}

// extractTarball extracts a .tar.gz into dir under rule D6: only regular files and
// dirs with local names, so no entry can reach outside dir.
func extractTarball(path, dir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		name := filepath.Clean(h.Name)
		if (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe entry %s in %s", h.Name, filepath.Base(path))
		}
		target := filepath.Join(dir, name)
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
}
