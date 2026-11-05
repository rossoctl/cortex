package main

import (
	"path/filepath"
	"testing"
)

func TestSkipDirSkipsInRepoWorktrees(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("..", "..", ".worktrees"), true},
		{filepath.Join("..", "..", ".claude", "worktrees"), true},
		{filepath.Join("..", "..", ".git"), true},
		{filepath.Join("..", "..", "docs", "worktrees"), false},
		{filepath.Join("..", "..", ".claude", "skills"), false},
		{filepath.Join("..", "..", "core"), false},
	}
	for _, c := range cases {
		if got := skipDir(c.path); got != c.want {
			t.Errorf("skipDir(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
