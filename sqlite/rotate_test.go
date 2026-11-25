package qsqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	qsql "github.com/pablo-botella/quevu-sql"
	"github.com/pablo-botella/quevu/qkernel"
)

func rows(t *testing.T, path, table string) []uint64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := db.Query("SELECT seq FROM " + table + " ORDER BY seq")
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	defer rs.Close()
	var out []uint64
	for rs.Next() {
		var s uint64
		rs.Scan(&s)
		out = append(out, s)
	}
	return out
}

func nums(cs []qkernel.Chunk) []uint64 {
	var out []uint64
	for _, c := range cs {
		out = append(out, c.Seq)
	}
	return out
}

func TestRotationNames(t *testing.T) {
	dir := t.TempDir()
	r := Rotation{Dir: dir, Base: "app-log", Location: time.UTC}
	at := time.Date(2025, 3, 9, 14, 0, 0, 0, time.UTC)
	if got := r.Path(at); got != filepath.Join(dir, "app-log-2025-03.db") {
		t.Fatal(got)
	}
	// The period is told in the zone configured: the last hour of a
	// month in one zone is the first of the next in another.
	edge := time.Date(2025, 3, 31, 23, 30, 0, 0, time.UTC)
	east := Rotation{Base: "x", Location: time.FixedZone("east", 2*3600)}
	if r.Period(edge) != "2025-03" || east.Period(edge) != "2025-04" {
		t.Fatalf("%s %s", r.Period(edge), east.Period(edge))
	}
	if got := (Rotation{Base: "d", Layout: "2006-01-02", Location: time.UTC}).Path(at); got != "d-2025-03-09.db" {
		t.Fatal(got)
	}
	for _, name := range []string{
		"app-log-2025-03.db", "app-log-2024-12.db", "app-log-2025-01.db",
		// not of this rotation:
		"app-log.db", "app-log-2025.db", "app-log-2025-13.db", "app-log-2025-3.db", "app-log-2025-03.db-wal",
		"other-2025-03.db", "app-log-2025-03.txt", "app-log-old-2025-03.db",
	} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "app-log-2025-02.db"), 0o755)
	files, err := r.Files()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if !reflect.DeepEqual(names, []string{"app-log-2024-12.db", "app-log-2025-01.db", "app-log-2025-03.db"}) {
		t.Fatalf("%v", names)
	}
	if files, err := (Rotation{Dir: filepath.Join(dir, "missing"), Base: "x"}).Files(); err != nil || files != nil {
		t.Fatal("a folder that is not there yet:", files, err)
	}
	for name, bad := range map[string]Rotation{
		"no base":          {Dir: dir},
		"a path as base":   {Dir: dir, Base: `logs\app`},
		"a pattern":        {Dir: dir, Base: "app*"},
		"no period at all": {Dir: dir, Base: "app", Layout: "log"},
	} {
		if _, err := OpenRotating(bad, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := NewCurrent(bad, ""); err == nil {
			t.Errorf("%s: a reader was made", name)
		}
		if _, err := NewPeriods(bad, ""); err == nil {
			t.Errorf("%s: a history was made", name)
		}
	}
	if _, err := OpenRotating(Rotation{Dir: dir, Base: "ok"}, Options{Table: "bad name"}); err == nil {
		t.Error("bad table accepted")
	}
}

func TestEveryChunkGoesToTheFileOfItsPeriod(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made", "on", "demand")
	rot := Rotation{Dir: dir, Base: "log", Location: time.UTC}
	d, err := OpenRotating(rot, Options{Table: "web", Buffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	if last, _ := d.LastSeq(); last != 0 {
		t.Fatalf("a new log starts at %d", last)
	}
	jan := time.Date(2031, 1, 31, 23, 59, 58, 0, time.UTC)
	chunk := func(seq uint64, at time.Time) qkernel.Chunk {
		return qkernel.Chunk{Seq: seq, Time: at, Data: fmt.Appendf(nil, "line %d", seq)}
	}
	// One batch that crosses the end of the month.
	if err := d.Write([]qkernel.Chunk{
		chunk(1, jan), chunk(2, jan.Add(time.Second)),
		chunk(3, jan.Add(2*time.Second)), chunk(4, jan.Add(3*time.Second)), // February
	}); err != nil {
		t.Fatal(err)
	}
	// The month changed: what January had in its buffer was written when
	// its file was closed. February still holds its two in the buffer.
	janFile, febFile := filepath.Join(dir, "log-2031-01.db"), filepath.Join(dir, "log-2031-02.db")
	if got := rows(t, janFile, "web"); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("January holds %v", got)
	}
	if got := rows(t, febFile, "web"); len(got) != 0 {
		t.Fatalf("February already holds %v", got)
	}
	if err := d.Write([]qkernel.Chunk{chunk(5, jan.Add(time.Hour)), chunk(6, jan.Add(40*24*time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Write([]qkernel.Chunk{chunk(9, jan)}); err == nil {
		t.Fatal("written after closing")
	}
	if d.Close() != nil {
		t.Fatal("closing twice")
	}
	if got := rows(t, febFile, "web"); !reflect.DeepEqual(got, []uint64{3, 4, 5}) {
		t.Fatalf("February holds %v", got)
	}
	if got := rows(t, filepath.Join(dir, "log-2031-03.db"), "web"); !reflect.DeepEqual(got, []uint64{6}) {
		t.Fatalf("March holds %v", got)
	}

	// The whole log, as one, across the files.
	all, err := NewPeriods(rot, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	tail, _ := all.Tail(4)
	before, _ := all.Before(4, 3)
	after, _ := all.After(1, 4)
	if !reflect.DeepEqual(nums(tail), []uint64{3, 4, 5, 6}) || !reflect.DeepEqual(nums(before), []uint64{1, 2, 3}) || !reflect.DeepEqual(nums(after), []uint64{2, 3, 4, 5}) {
		t.Fatalf("tail %v before %v after %v", nums(tail), nums(before), nums(after))
	}
	if string(tail[0].Data) != "line 3" || !tail[0].Time.Equal(jan.Add(2*time.Second)) {
		t.Fatalf("%q at %v", tail[0].Data, tail[0].Time)
	}
	// Another log in the same files is another table, and does not mix.
	other, _ := NewPeriods(rot, "mail")
	defer other.Close()
	if cs, err := other.Tail(5); err != nil || len(cs) != 0 {
		t.Fatalf("another log: %v %v", cs, err)
	}

	// The provider of the period in course follows the calendar.
	cur, err := NewCurrent(rot, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	now := jan
	cur.now = func() time.Time { return now }
	if cs, _ := cur.Tail(9); !reflect.DeepEqual(nums(cs), []uint64{1, 2}) {
		t.Fatalf("in January: %v", nums(cs))
	}
	now = jan.Add(time.Hour)
	if cs, _ := cur.Tail(9); !reflect.DeepEqual(nums(cs), []uint64{3, 4, 5}) {
		t.Fatalf("in February: %v", nums(cs))
	}
	if cs, _ := cur.Before(5, 9); !reflect.DeepEqual(nums(cs), []uint64{3, 4}) {
		t.Fatalf("before 5 in February: %v", nums(cs))
	}
	now = jan.Add(100 * 24 * time.Hour) // a month nothing was written in
	if cs, err := cur.Tail(9); err != nil || len(cs) != 0 {
		t.Fatalf("in an empty month: %v %v", cs, err)
	}

	// A file that goes away (archived, deleted) and one that appears are
	// noticed without making the provider again. A provider that has read
	// a file keeps it open, and an open file cannot be removed on every
	// system: closing the provider lets go, and it opens again when asked.
	all.Close()
	other.Close()
	if err := os.Remove(janFile); err != nil {
		t.Fatal(err)
	}
	os.Remove(janFile + "-wal")
	os.Remove(janFile + "-shm")
	apr, err := Open(filepath.Join(dir, "log-2031-04.db"), Options{Table: "web"})
	if err != nil {
		t.Fatal(err)
	}
	apr.Write([]qkernel.Chunk{chunk(7, jan.Add(70*24*time.Hour))})
	apr.Close()
	if cs, _ := all.After(0, 9); !reflect.DeepEqual(nums(cs), []uint64{3, 4, 5, 6, 7}) {
		t.Fatalf("after the files changed: %v", nums(cs))
	}
}

func TestPositionsGoOnFromOneFileToTheNext(t *testing.T) {
	dir := t.TempDir()
	rot := Rotation{Dir: dir, Base: "log"}
	// Files of periods long gone, with something in them.
	for _, f := range []struct {
		period string
		seqs   []uint64
	}{{"2019-11", []uint64{1, 2, 3}}, {"2020-02", []uint64{4, 5, 41}}, {"2020-03", nil}} {
		d, err := Open(filepath.Join(dir, "log-"+f.period+".db"), Options{Table: "web"})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range f.seqs {
			d.Write([]qkernel.Chunk{{Seq: s, Time: time.Now(), Data: []byte("old")}})
		}
		d.Close()
	}
	// Today is a new period: its file does not exist yet. The count goes
	// on from the newest file that holds something of this log, skipping
	// the one that is there but empty.
	d, err := OpenRotating(rot, Options{Table: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if last, _ := d.LastSeq(); last != 41 {
		t.Fatalf("goes on from %d, want 41", last)
	}
	now := time.Now()
	if err := d.Write([]qkernel.Chunk{{Seq: 42, Time: now, Data: []byte("new")}}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := os.Stat(rot.Path(now)); err != nil {
		t.Fatal("the file of today:", err)
	}
	// Opened again in the same period, it goes on from its own file.
	d, err = OpenRotating(rot, Options{Table: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if last, _ := d.LastSeq(); last != 42 {
		t.Fatalf("goes on from %d, want 42", last)
	}
	d.Close()
	// Another log in the same files starts its own count.
	d, _ = OpenRotating(rot, Options{Table: "mail"})
	if last, _ := d.LastSeq(); last != 0 {
		t.Fatalf("another log goes on from %d", last)
	}
	d.Close()
}

func TestRotatingWithAFormat(t *testing.T) {
	rot := Rotation{Dir: t.TempDir(), Base: "log", Location: time.UTC}
	f := &qsql.Format{Separator: "|", Fields: []qsql.Field{{Name: "level", Size: 8, Index: true}, {Name: "n", Type: qsql.Int}}}
	d, err := OpenRotating(rot, Options{Table: "app", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	a, b := time.Date(2033, 5, 31, 12, 0, 0, 0, time.UTC), time.Date(2033, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := d.Write([]qkernel.Chunk{
		{Seq: 1, Time: a, Data: []byte("INFO|7")}, {Seq: 2, Time: a, Data: []byte("free text")},
		{Seq: 3, Time: b, Data: []byte("WARN|x")}, {Seq: 4, Time: b, Data: []byte("WARN|9")},
	}); err != nil {
		t.Fatal(err)
	}
	// The count of misfits covers the file already closed and the one in use.
	if d.Misfits() != 2 {
		t.Fatalf("%d misfits", d.Misfits())
	}
	d.Close()
	if d.Misfits() != 2 {
		t.Fatalf("%d misfits after closing", d.Misfits())
	}
	db, _ := sql.Open("sqlite", rot.Path(b))
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT sum(n) FROM app WHERE level = 'WARN'").Scan(&n); err != nil || n != 9 {
		t.Fatalf("columns in the second file: %d %v", n, err)
	}
}
