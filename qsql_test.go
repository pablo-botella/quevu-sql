package qsql

import (
	"errors"
	"strings"
	"testing"

	"github.com/pablo-botella/dbh"
)

// The table is described once, and dbh writes it for each database. This
// test pins what comes out, so a change in either place is noticed.
func TestTheTableInEveryDialect(t *testing.T) {
	sqlite := Meta("web", nil).BuildCreateTable(dbh.SQLite)
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS web", "seq INTEGER PRIMARY KEY", "time INTEGER NOT NULL", "data "} {
		if !strings.Contains(sqlite, want) {
			t.Fatalf("SQLite: no %q in\n%s", want, sqlite)
		}
	}
	mysql := Meta("web", nil).BuildCreateTable(dbh.MySQL)
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS web", "seq BIGINT", "time BIGINT NOT NULL", "PRIMARY KEY (seq)", "ENGINE=InnoDB"} {
		if !strings.Contains(mysql, want) {
			t.Fatalf("MySQL: no %q in\n%s", want, mysql)
		}
	}
}

func TestTableNames(t *testing.T) {
	if n, err := TableName(""); err != nil || n != DefaultTable {
		t.Fatalf("%q %v", n, err)
	}
	if n, err := TableName("mailer_2025"); err != nil || n != "mailer_2025" {
		t.Fatalf("%q %v", n, err)
	}
	for _, bad := range []string{"x; drop", "1abc", "a-b", "a.b", "a b", "`a`"} {
		if _, err := TableName(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAMissingTableIsToldApartInEveryDatabase(t *testing.T) {
	for _, text := range []string{
		"SQL logic error: no such table: web (1)",
		"Error 1146 (42S02): Table 'logs.web' doesn't exist",
		`ERROR: relation "web" does not exist (SQLSTATE 42P01)`,
	} {
		if !missingTable(errors.New(text)) {
			t.Fatalf("not recognized: %s", text)
		}
	}
	if missingTable(errors.New("connection refused")) {
		t.Fatal("a real failure taken for a missing table")
	}
}

func TestSettingUpNeedsNoDatabase(t *testing.T) {
	if _, err := NewReader(nil, "web"); err == nil {
		t.Fatal("a reader with no way to open anything")
	}
	if _, err := NewReader(func() (*dbh.Dbh, error) { return nil, nil }, "bad name"); err == nil {
		t.Fatal("bad table accepted")
	}
	// Nothing there yet: an empty log, asked again next time.
	asked := 0
	r, err := NewReader(func() (*dbh.Dbh, error) { asked++; return nil, nil }, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if got, err := r.Tail(5); err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	}
	if asked != 2 {
		t.Fatalf("asked %d times", asked)
	}
	// A database out of reach is an error, not an empty log.
	boom := errors.New("connection refused")
	r, _ = NewReader(func() (*dbh.Dbh, error) { return nil, boom }, "")
	if _, err := r.Before(10, 5); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if r.Close() != nil || NewHistory(nil, r).Close() != nil {
		t.Fatal("close")
	}
}
