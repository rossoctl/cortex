package sqlitecli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type row struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// testDB makes a database with one row ("x'y", 2). It skips the test when sqlite3 is not on PATH.
func testDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(Program); err != nil {
		t.Skip("sqlite3 is not on PATH")
	}
	db := filepath.Join(t.TempDir(), "t.db")
	if out, err := exec.Command(Program, db, "create table t(a text, b integer); insert into t values('x''y', 2);").CombinedOutput(); err != nil {
		t.Fatalf("make db: %v: %s", err, out)
	}
	return db
}

func TestQuery_DecodesRows(t *testing.T) {
	db := testDB(t)
	var rows []row
	if err := Query(Program, db, "select a, b from t", &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != (row{"x'y", 2}) {
		t.Fatalf("rows = %+v, want [{x'y 2}]", rows)
	}
}

func TestQuery_NoRowsIsEmpty(t *testing.T) {
	db := testDB(t)
	var rows []row
	if err := Query(Program, db, "select a from t where b = 99", &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

func TestQuery_MissingProgramNamesIt(t *testing.T) {
	var rows []row
	err := Query("sqlite3-not-here-1872", filepath.Join(t.TempDir(), "t.db"), "select 1", &rows)
	if err == nil || !strings.Contains(err.Error(), "sqlite3-not-here-1872") || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want it to name the program and say not installed", err)
	}
}

func TestQuery_BadSQLGivesStderr(t *testing.T) {
	db := testDB(t)
	var rows []row
	if err := Query(Program, db, "select * from nope", &rows); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want it to contain sqlite3's message about nope", err)
	}
}

func TestQuote(t *testing.T) {
	if got := Quote("a'b"); got != "'a''b'" {
		t.Fatalf("Quote(a'b) = %s, want 'a''b'", got)
	}
	db := testDB(t)
	var rows []row
	if err := Query(Program, db, "select a from t where a = "+Quote("x'y"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
}
