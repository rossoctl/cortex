// Package sqlitecli reads a SQLite database through the sqlite3 program, so that no module
// links a SQLite driver. Importer sources (Bob, OpenCode) use it.
package sqlitecli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Program is the name of the sqlite3 program on PATH.
const Program = "sqlite3"

// Query runs query on db with prog in read-only mode, and decodes the JSON array that prog
// prints into out. prog prints nothing for zero rows: then out stays unchanged and Query
// gives nil.
func Query(prog, db, query string, out any) error {
	path, err := exec.LookPath(prog)
	if err != nil {
		return fmt.Errorf("%s is not installed or not on PATH: %w", prog, err)
	}
	stdout, err := exec.Command(path, "-readonly", "-json", db, query).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return fmt.Errorf("%s %s: %w: %s", prog, db, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("%s %s: %w", prog, db, err)
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		return fmt.Errorf("%s %s: decode output: %w", prog, db, err)
	}
	return nil
}

// Quote gives s as a SQL string literal: s in single quotes, with each ' in s doubled.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
