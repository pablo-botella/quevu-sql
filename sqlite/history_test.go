package qsqlite

import (
	"path/filepath"
	"testing"

	"github.com/pablo-botella/quevu/qkernel"
)

// store writes a stretch of a log to one table of one database.
func stretch(t *testing.T, path, table string, from, to uint64) {
	t.Helper()
	d, err := Open(path, Options{Table: table})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Write(mk(from, to)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryOverSeveralTablesAndDatabases(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.db")
	live := filepath.Join(dir, "live.db")
	// Two years archived as tables of one database, the current one in
	// another database altogether.
	stretch(t, archive, "y2024", 1, 100)
	stretch(t, archive, "y2025", 101, 250)
	stretch(t, live, "", 251, 300)

	h, err := NewHistory(
		Table{Path: archive, Name: "y2024"},
		Table{Path: archive, Name: "y2025"},
		Table{Path: filepath.Join(dir, "never-made.db"), Name: "y2026"},
		Table{Path: live},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	cases := []struct {
		name string
		got  func() ([]qkernel.Chunk, error)
		want string
	}{
		{"tail", func() ([]qkernel.Chunk, error) { return h.Tail(3) }, "298,299,300"},
		{"before, across databases", func() ([]qkernel.Chunk, error) { return h.Before(253, 4) }, "249,250,251,252"},
		{"before, across tables", func() ([]qkernel.Chunk, error) { return h.Before(102, 3) }, "99,100,101"},
		{"after, across tables", func() ([]qkernel.Chunk, error) { return h.After(99, 3) }, "100,101,102"},
		{"after, across databases", func() ([]qkernel.Chunk, error) { return h.After(249, 3) }, "250,251,252"},
		{"before the first", func() ([]qkernel.Chunk, error) { return h.Before(1, 3) }, ""},
		{"after the last", func() ([]qkernel.Chunk, error) { return h.After(300, 3) }, ""},
	}
	for _, c := range cases {
		got, err := c.got()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if seqs(got) != c.want {
			t.Fatalf("%s: got %q want %q", c.name, seqs(got), c.want)
		}
	}
	all, err := h.Tail(1000)
	if err != nil || len(all) != 300 || all[0].Seq != 1 || all[299].Seq != 300 {
		t.Fatalf("everything: %d chunks, %v", len(all), err)
	}

	// A bounded search step walks the same way.
	r, err := qkernel.Search(h, qkernel.Query{Text: "line 77", Budget: 1000})
	if err != nil || len(r.Matches) != 1 || r.Matches[0].Seq != 77 {
		t.Fatalf("search: %+v %v", r, err)
	}
}

func TestHistoryWithNothingBehindIt(t *testing.T) {
	h, err := NewHistory(Table{Path: filepath.Join(t.TempDir(), "none.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got, err := h.Tail(5); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := NewHistory(Table{Path: "x.db", Name: "bad name"}); err == nil {
		t.Fatal("bad table name accepted")
	}
}
