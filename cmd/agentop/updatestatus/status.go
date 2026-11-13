// Package updatestatus holds the type of ~/.cortex/update.json, the result of
// the last `agentop update --auto` run. The writer is in package main.
package updatestatus

import (
	"encoding/json"
	"os"
	"regexp"
	"time"
)

// FileName is the name of the status file in ~/.cortex.
const FileName = "update.json"

// Values of Status.Result.
const (
	ResultDisabled    = "disabled"
	ResultNotARelease = "not-a-release"
	ResultCurrent     = "current"
	ResultWaiting     = "waiting"
	ResultCheckFailed = "check-failed"
	ResultUpdated     = "updated"
	ResultFailed      = "failed"
)

// Status is the content of the status file.
type Status struct {
	CheckedAt    time.Time `json:"checkedAt"`
	Latest       string    `json:"latest"`
	Installed    string    `json:"installed"`
	Result       string    `json:"result"`
	InstallAfter time.Time `json:"installAfter,omitzero"`
	Error        string    `json:"error"`
}

// Read decodes the status file at path. A missing file gives an error that
// satisfies errors.Is(err, fs.ErrNotExist).
func Read(path string) (Status, error) {
	var s Status
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

var releaseRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// IsRelease reports whether v is a plain release tag such as v0.9.5.
func IsRelease(v string) bool { return releaseRE.MatchString(v) }
