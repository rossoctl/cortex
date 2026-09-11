package tui

import (
	"reflect"
	"testing"
)

// resetSettingsForTest clears the package-level Settings and restores it after the
// test. Settings is global (see its doc comment), so without this a test that sets
// a filter leaks it into every later test in the package — and the failure would
// surface as an unrelated test failing depending on run order.
//
// t.Cleanup rather than a defer in each caller: the reset has to happen even when
// the test fails partway through.
func resetSettingsForTest(t *testing.T) {
	t.Helper()
	prev := Settings
	Settings = UserSettings{}
	t.Cleanup(func() { Settings = prev })
}

// TestColumnSelection_AbsentColumnsDefaultOn is the headline rule of the file
// format: the config records deviations, so a column the file does not mention is
// VISIBLE. Inverting this would mean a column added in a later abctl starts hidden
// for everyone who already has a config file.
func TestColumnSelection_AbsentColumnsDefaultOn(t *testing.T) {
	s := UserSettings{Events: EventSettings{Columns: []ColumnSetting{
		{Name: string(colCost), Visible: false},
	}}}
	sel := s.columnSelection()

	if sel[colCost] {
		t.Errorf("COST was named visible:false in the config but is on")
	}
	// Every other column — eleven of them — must be on without being mentioned.
	for _, c := range eventColumns {
		if c.id == colCost {
			continue
		}
		if !sel[c.id] {
			t.Errorf("column %q is absent from the config and should default ON, got off", c.id)
		}
	}
}

// TestColumnSelection_EmptyConfigIsAllDefaults: the zero value (no file, or a file
// with no columns key) must be exactly the built-in selection.
func TestColumnSelection_EmptyConfigIsAllDefaults(t *testing.T) {
	got, want := (UserSettings{}).columnSelection(), defaultColumnSelection()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empty config selection = %v, want the defaults %v", got, want)
	}
}

// TestColumnSelection_UnknownNamesAreIgnored: a name from a newer build (or a
// typo) must not create a map key with no column behind it. anyColumnSelected and
// selectedColumns both walk eventColumns, so a phantom key would make the former
// claim a selection the latter cannot render.
func TestColumnSelection_UnknownNamesAreIgnored(t *testing.T) {
	s := UserSettings{Events: EventSettings{Columns: []ColumnSetting{
		{Name: "WARP_DRIVE", Visible: false},
		{Name: string(colHost), Visible: false},
	}}}
	sel := s.columnSelection()

	if _, ok := sel["WARP_DRIVE"]; ok {
		t.Error("unknown column name leaked into the selection map")
	}
	if len(sel) != len(eventColumns) {
		t.Errorf("selection has %d entries, want one per known column (%d)", len(sel), len(eventColumns))
	}
	// The known entry alongside it still applies — one bad name must not discard
	// the rest of the file.
	if sel[colHost] {
		t.Error("HOST was named visible:false but is on; a neighbouring unknown name should not void it")
	}
}

// TestColumnSelection_AllOffFallsBackToDefaults: a hand-edited file can turn
// everything off, which would render a table with no columns. selectedColumns'
// fallback rescues the render but not the picker's checkboxes, so the selection
// itself is re-seeded — matching what the toggle handler already does.
func TestColumnSelection_AllOffFallsBackToDefaults(t *testing.T) {
	var all []ColumnSetting
	for _, c := range eventColumns {
		all = append(all, ColumnSetting{Name: string(c.id), Visible: false})
	}
	s := UserSettings{Events: EventSettings{Columns: all}}

	if got, want := s.columnSelection(), defaultColumnSelection(); !reflect.DeepEqual(got, want) {
		t.Errorf("all-off config selection = %v, want the defaults %v", got, want)
	}
}

// TestColumnSettingsFrom_RecordsOnlyDeviations: an untouched selection serialises
// to nothing, so a user who never opened the picker gets no columns key at all
// rather than twelve `visible: true` lines.
func TestColumnSettingsFrom_RecordsOnlyDeviations(t *testing.T) {
	if got := columnSettingsFrom(defaultColumnSelection()); len(got) != 0 {
		t.Errorf("default selection serialised to %v, want nothing written down", got)
	}

	sel := defaultColumnSelection()
	sel[colTokens] = false
	got := columnSettingsFrom(sel)
	want := []ColumnSetting{{Name: string(colTokens), Visible: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("one column off serialised to %v, want %v", got, want)
	}
}

// TestColumnSettingsFrom_IsDeterministicInDisplayOrder: Go randomises map
// iteration, so emitting in map order would rewrite the file with the same
// entries reshuffled on every save — invisible in the TUI, obvious in a diff.
func TestColumnSettingsFrom_IsDeterministicInDisplayOrder(t *testing.T) {
	sel := defaultColumnSelection()
	sel[colHost] = false
	sel[colIndex] = false
	sel[colMethod] = false

	first := columnSettingsFrom(sel)
	// Display order, per eventColumns: # before METHOD before HOST.
	want := []ColumnSetting{
		{Name: string(colIndex), Visible: false},
		{Name: string(colMethod), Visible: false},
		{Name: string(colHost), Visible: false},
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("got %v, want display order %v", first, want)
	}
	// Repeat on the same map: map iteration order varies between range loops, so a
	// single call cannot catch this.
	for i := 0; i < 20; i++ {
		if got := columnSettingsFrom(sel); !reflect.DeepEqual(got, first) {
			t.Fatalf("iteration %d returned %v, want the stable %v", i, got, first)
		}
	}
}

// TestColumnSettings_RoundTrip: selection → file → selection is the identity, for
// a non-default selection. This is what makes a saved preference actually come
// back on the next start.
func TestColumnSettings_RoundTrip(t *testing.T) {
	want := defaultColumnSelection()
	want[colCost] = false
	want[colDuration] = false

	s := UserSettings{Events: EventSettings{Columns: columnSettingsFrom(want)}}
	if got := s.columnSelection(); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}

// TestColumnSelection_AbsentHonorsDefaultOn: "absent from the file" means "the user
// never changed this", so the fallback must be the column's own defaultOn — not a
// literal true.
//
// Cannot be observed through eventColumns as it stands: all twelve entries are
// defaultOn, so the two spellings agree. This substitutes a table containing a
// defaultOn:false column, which is what a future column would look like, and pins
// that columnSelection agrees with defaultColumnSelection about it.
func TestColumnSelection_AbsentHonorsDefaultOn(t *testing.T) {
	prev := eventColumns
	t.Cleanup(func() { eventColumns = prev })
	eventColumns = []eventColumn{
		{id: colTime, width: 12, defaultOn: true, cell: func(cellContext) string { return "" }},
		{id: "OPTIONAL", width: 8, defaultOn: false, cell: func(cellContext) string { return "" }},
	}

	// Nothing in the file: each column gets its own default.
	got := (UserSettings{}).columnSelection()
	if !got[colTime] {
		t.Error("a defaultOn:true column is off with an empty config")
	}
	if got["OPTIONAL"] {
		t.Error("a defaultOn:false column is ON with an empty config; absent must mean the default, not visible")
	}
	if want := defaultColumnSelection(); !reflect.DeepEqual(got, want) {
		t.Errorf("columnSelection = %v, want it to agree with defaultColumnSelection %v", got, want)
	}

	// A file can still turn the opt-in column on explicitly.
	on := UserSettings{Events: EventSettings{Columns: []ColumnSetting{{Name: "OPTIONAL", Visible: true}}}}
	if !on.columnSelection()["OPTIONAL"] {
		t.Error("an explicit visible:true did not turn on a defaultOn:false column")
	}
}

// TestColumnSettings_RoundTripsADefaultOffColumn: turning ON a defaultOn:false
// column is as much a deviation as turning a default one off, so it has to survive
// a save/load. Recording only the off-list would make it unpersistable — the user
// would enable the column, close the picker, and find it off again next launch.
func TestColumnSettings_RoundTripsADefaultOffColumn(t *testing.T) {
	prev := eventColumns
	t.Cleanup(func() { eventColumns = prev })
	eventColumns = []eventColumn{
		{id: colTime, width: 12, defaultOn: true, cell: func(cellContext) string { return "" }},
		{id: "OPTIONAL", width: 8, defaultOn: false, cell: func(cellContext) string { return "" }},
	}

	want := map[eventColumnID]bool{colTime: true, "OPTIONAL": true}
	written := columnSettingsFrom(want)
	if len(written) != 1 || written[0] != (ColumnSetting{Name: "OPTIONAL", Visible: true}) {
		t.Fatalf("wrote %+v, want just OPTIONAL visible:true", written)
	}
	got := UserSettings{Events: EventSettings{Columns: written}}.columnSelection()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}
