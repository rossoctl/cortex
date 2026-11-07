package model

import (
	"encoding/json"
	"log/slog"
)

// Direction indicates whether a request is inbound (caller → this agent) or
// outbound (this agent → target service).
type Direction int

const (
	Inbound Direction = iota
	Outbound
)

// String returns "inbound" / "outbound". Used for structured logs and the
// wire format of SessionEvent.
func (d Direction) String() string {
	switch d {
	case Inbound:
		return "inbound"
	case Outbound:
		return "outbound"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the string form ("inbound"/"outbound") so the wire
// format is human-readable without an enum→int lookup.
func (d Direction) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a Direction from the string form emitted by
// MarshalJSON. Unknown strings decode to Inbound (zero value) without
// error so downstream consumers stay tolerant of forward-compatible
// additions. A Debug-level log fires on unknown input so wire-format
// drift is at least observable in a verbose test run.
func (d *Direction) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "inbound":
		*d = Inbound
	case "outbound":
		*d = Outbound
	default:
		slog.Debug("pipeline: unknown Direction, defaulting to inbound", "value", s)
		*d = Inbound
	}
	return nil
}
