package main

import (
	"os"
	"path/filepath"
	"testing"
)

// assetPath is the committed asset the README points at. GitHub cannot build the
// SVG, so it has to be in the tree.
const assetPath = "../../docs/assets/cortex-demo.svg"

// TestCommittedAssetIsCurrent fails when the committed SVG no longer matches what
// demo.yaml and the current agentop produce.
//
// A fabricated demo is only worth having if it cannot quietly drift from the
// product it claims to show; this converts a TUI change that would have made the
// asset a lie into a failing check.
func TestCommittedAssetIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read committed asset: %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.svg")
	if err := run("demo.yaml", fresh); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	got, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}

	// Compared byte for byte, with nothing masked: masking digits once made CTX(1M) and
	// CTX(2M) compare equal. The capturer canonicalises ages and clocks at fixed width.
	if string(committed) != string(got) {
		t.Errorf(`%s is stale.

Regenerate it:
    go -C scripts/readme-demo run .

(committed %d bytes, regenerated %d bytes)`, assetPath, len(committed), len(got))
	}
}

// TestCommittedAssetIsWithinBudget keeps the README from growing a megabyte of
// markup unnoticed.
func TestCommittedAssetIsWithinBudget(t *testing.T) {
	info, err := os.Stat(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 560 << 10 // 535 KB once agentop drew boxed panels (#2027)
	if info.Size() > budget {
		t.Errorf("asset is %d KB, over the %d KB budget", info.Size()>>10, budget>>10)
	}
}
