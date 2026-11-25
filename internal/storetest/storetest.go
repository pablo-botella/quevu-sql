// Package storetest is the one test every database package runs: the
// same lines, the same format, the same expectations. What passes here
// on one database and not on another is a difference between databases
// that this module failed to hide.
package storetest

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pablo-botella/dbh"
	qsql "github.com/pablo-botella/quevu-sql"
	"github.com/pablo-botella/quevu/qkernel"
)

// Store is a database under test.
type Store struct {
	// Open returns a device over a table.
	Open func(table string, format *qsql.Format, buffer int) (*qsql.Device, error)
	// Reader returns a provider over a table.
	Reader func(table string) (*qsql.Reader, error)
	// Look is a handle to see what the tables really hold.
	Look *dbh.Dbh
	// Prefix starts the name of every table the test makes. They are
	// dropped at the end.
	Prefix string
}

func (s Store) table(t *testing.T, name string) string {
	table := s.Prefix + name
	s.Look.Exec("DROP TABLE IF EXISTS " + table)
	t.Cleanup(func() { s.Look.Exec("DROP TABLE IF EXISTS " + table) })
	return table
}

type row struct {
	data                       string
	at, customer, amount, paid sql.NullInt64
	level, message             sql.NullString
}

func (s Store) rows(t *testing.T, table string) map[int64]row {
	t.Helper()
	rs, err := s.Look.Query("SELECT seq, data, at, level, customer, amount, paid, message FROM " + table + " ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[int64]row{}
	for rs.Next() {
		var seq int64
		var data []byte
		var r row
		if err := rs.Scan(&seq, &data, &r.at, &r.level, &r.customer, &r.amount, &r.paid, &r.message); err != nil {
			t.Fatal(err)
		}
		r.data = string(data)
		out[seq] = r
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s Store) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := s.Look.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n.Int64
}

func num(n int64) sql.NullInt64   { return sql.NullInt64{Int64: n, Valid: true} }
func str(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// PaymentsFormat is the format the test stores with.
func PaymentsFormat() *qsql.Format {
	return &qsql.Format{Separator: "|", Location: time.UTC, Fields: []qsql.Field{
		{Name: "at", Type: qsql.Time, Layout: "2006-01-02 15:04:05"},
		{Name: "level", Size: 5, Index: true},
		{}, // a piece nobody wants
		{Name: "customer", Type: qsql.Int, Index: true},
		{Name: "amount", Type: qsql.Decimal, Scale: 2},
		{Name: "paid", Type: qsql.Bool},
		{Name: "message", Rest: true},
	}}
}

// Run is the whole test.
func Run(t *testing.T, s Store) {
	t.Run("fields become columns", func(t *testing.T) { s.columns(t) })
	t.Run("a table and its format must agree", func(t *testing.T) { s.agreement(t) })
	t.Run("many columns", func(t *testing.T) { s.wide(t) })
	t.Run("with a buffer", func(t *testing.T) { s.buffered(t) })
}

func (s Store) columns(t *testing.T) {
	table := s.table(t, "pay")
	d, err := s.Open(table, PaymentsFormat(), 0)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 3, 9, 14, 5, 6, 0, time.UTC).UnixNano()
	null, nullS := sql.NullInt64{}, sql.NullString{}
	lines := []struct {
		line string
		want row
		fits bool
	}{
		{"2025-03-09 14:05:06|INFO|x|42|12.50|yes|charged", row{at: num(at), level: str("INFO"), customer: num(42), amount: num(1250), paid: num(1), message: str("charged")}, true},
		{" 2025-03-09 14:05:06 | INFO | | 42 | 12.5 | Y | charged ok ", row{at: num(at), level: str("INFO"), customer: num(42), amount: num(1250), paid: num(1), message: str("charged ok")}, true},
		{"\x1b[90m2025-03-09 14:05:06\x1b[0m|\x1b[31mERROR\x1b[0m|x|-7|-0.05|0|\x1b[1mdeclined\x1b[0m", row{at: num(at), level: str("ERROR"), customer: num(-7), amount: num(-5), paid: num(0), message: str("declined")}, true},
		{"2025-03-09 14:05:06|INFO|x|1|1|1|a|b||c", row{at: num(at), level: str("INFO"), customer: num(1), amount: num(100), paid: num(1), message: str("a|b||c")}, true},
		{"||||||", row{}, true},
		{"2025-03-09 14:05:06|INFO", row{at: num(at), level: str("INFO")}, false},
		{"panic: something broke", row{}, false},
		{"", row{}, false},
		{"2025-03-09 14:05:06|INFO|x|4x2|1.00|1|m", row{at: num(at), level: str("INFO"), amount: num(100), paid: num(1), message: str("m")}, false},
		{"2025-03-09 14:05:06|INFO|x|4|1.005|maybe|m", row{at: num(at), level: str("INFO"), customer: num(4), message: str("m")}, false},
		{"2025-03-09 14:05:06|ÁÉÍÓÚÑ€😀|x|4|1|1|año 😀 € ñ", row{at: num(at), level: str("ÁÉÍÓÚ"), customer: num(4), amount: num(100), paid: num(1), message: str("año 😀 € ñ")}, true},
		{"2025-03-09 14:05:06|I|x|-9223372036854775808|92233720368547758.07|ON|m", row{at: num(at), level: str("I"), customer: num(math.MinInt64), amount: num(math.MaxInt64), paid: num(1), message: str("m")}, true},
		{"2025-03-09 14:05:06|ERROR|x|42|7.25|no|it's \"quoted\" \\ and 100% _odd_; DROP TABLE x; --", row{at: num(at), level: str("ERROR"), customer: num(42), amount: num(725), paid: num(0), message: str("it's \"quoted\" \\ and 100% _odd_; DROP TABLE x; --")}, true},
	}
	_, _ = null, nullS
	var batch []qkernel.Chunk
	misfits := uint64(0)
	for i, l := range lines {
		c := qkernel.Chunk{Seq: uint64(i + 1), Time: time.Unix(1700000000, int64(i)), Data: []byte(l.line)}
		if l.line == "" {
			c.Data = nil
		}
		batch = append(batch, c)
		if !l.fits {
			misfits++
		}
	}
	if err := d.Write(batch); err != nil {
		t.Fatal(err)
	}
	if got := d.Misfits(); got != misfits {
		t.Errorf("misfits %d, want %d", got, misfits)
	}
	got := s.rows(t, table)
	if len(got) != len(lines) {
		t.Fatalf("%d rows, want %d", len(got), len(lines))
	}
	for i, l := range lines {
		r := got[int64(i+1)]
		// The whole line is always there, exactly as it came.
		if r.data != l.line {
			t.Errorf("line %d: stored %q, came %q", i+1, r.data, l.line)
		}
		r.data = ""
		if r != l.want {
			t.Errorf("line %d (%q):\n got  %+v\n want %+v", i+1, l.line, r, l.want)
		}
	}

	// The columns are there to be asked by.
	if n := s.count(t, "SELECT count(*) FROM "+table+" WHERE level = ?", "ERROR"); n != 2 {
		t.Errorf("%d ERROR lines, want 2", n)
	}
	if n := s.count(t, "SELECT sum(amount) FROM "+table+" WHERE customer = ?", 42); n != 1250+1250+725 {
		t.Errorf("customer 42 sums %d", n)
	}
	if n := s.count(t, "SELECT count(*) FROM "+table+" WHERE at IS NULL"); n != 3 {
		t.Errorf("%d lines without time, want 3", n)
	}

	// A refused batch leaves nothing behind, and counts nothing.
	if err := d.Write([]qkernel.Chunk{{Seq: 100, Data: []byte("not a fit")}, {Seq: 1, Data: []byte("repeated")}}); err == nil {
		t.Fatal("a repeated position was stored")
	}
	if n := s.count(t, "SELECT count(*) FROM "+table); n != int64(len(lines)) {
		t.Errorf("%d rows after a refused batch", n)
	}
	if got := d.Misfits(); got != misfits {
		t.Errorf("misfits %d after a refused batch, want %d", got, misfits)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening it again finds the table and its indexes already there.
	d, err = s.Open(table, PaymentsFormat(), 0)
	if err != nil {
		t.Fatalf("opening again: %v", err)
	}
	if last, _ := d.LastSeq(); last != uint64(len(lines)) {
		t.Errorf("last %d", last)
	}
	next := uint64(len(lines) + 1)
	if err := d.Write([]qkernel.Chunk{{Seq: next, Time: time.Now(), Data: []byte("2025-03-09 14:05:06|WARN|x|9|0.01|1|again")}}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if r := s.rows(t, table)[int64(next)]; r.level != str("WARN") || r.amount != num(1) {
		t.Errorf("after reopening: %+v", r)
	}

	// Whoever reads the log by position sees it as any other.
	r, err := s.Reader(table)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tail, err := r.Tail(2)
	if err != nil || len(tail) != 2 || tail[1].Seq != next || string(tail[0].Data) != lines[len(lines)-1].line {
		t.Errorf("tail %v %v", tail, err)
	}

	// The same table without a format: lines go in, columns stay empty.
	d, err = s.Open(table, nil, 0)
	if err != nil {
		t.Fatalf("without format: %v", err)
	}
	if err := d.Write([]qkernel.Chunk{{Seq: next + 1, Time: time.Now(), Data: []byte("2025-03-09 14:05:06|WARN|x|9|0.01|1|plain")}}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if r := s.rows(t, table)[int64(next+1)]; r.level.Valid || r.amount.Valid || !strings.HasSuffix(r.data, "plain") {
		t.Errorf("without format: %+v", r)
	}
}

func (s Store) agreement(t *testing.T) {
	// A table made without a format does not get columns behind the back
	// of whoever owns it.
	table := s.table(t, "old")
	d, err := s.Open(table, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	d.Write([]qkernel.Chunk{{Seq: 1, Time: time.Now(), Data: []byte("kept")}})
	d.Close()
	if _, err := s.Open(table, PaymentsFormat(), 0); err == nil || !strings.Contains(err.Error(), "not altered") {
		t.Fatalf("a format over an older table: %v", err)
	}
	if n := s.count(t, "SELECT count(*) FROM "+table); n != 1 {
		t.Fatalf("the older table was touched: %d rows", n)
	}
	// Nor does a format that grew.
	table = s.table(t, "grew")
	d, err = s.Open(table, PaymentsFormat(), 0)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	bigger := PaymentsFormat()
	bigger.Fields[2].Name = "terminal"
	if _, err := s.Open(table, bigger, 0); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("a format that grew: %v", err)
	}
	// A bad format is refused before anything is made.
	table = s.table(t, "bad")
	if _, err := s.Open(table, &qsql.Format{Separator: "|", Fields: []qsql.Field{{Name: "order"}}}, 0); err == nil {
		t.Fatal("a bad format was accepted")
	}
	if _, err := s.Look.Query("SELECT seq FROM " + table); err == nil {
		t.Fatal("a table was made for a bad format")
	}
}

func (s Store) wide(t *testing.T) {
	table := s.table(t, "wide")
	f := &qsql.Format{Separator: ","}
	for i := 0; i < qsql.MaxFields; i++ {
		f.Fields = append(f.Fields, qsql.Field{Name: fmt.Sprintf("c%d", i), Type: qsql.Int})
	}
	d, err := s.Open(table, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	const lines = 1300
	var batch []qkernel.Chunk
	for l := 1; l <= lines; l++ {
		var sb strings.Builder
		for i := 0; i < qsql.MaxFields; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprint(&sb, l*1000+i)
		}
		batch = append(batch, qkernel.Chunk{Seq: uint64(l), Time: time.Now(), Data: []byte(sb.String())})
	}
	if err := d.Write(batch); err != nil {
		t.Fatal(err)
	}
	if d.Misfits() != 0 {
		t.Errorf("%d misfits", d.Misfits())
	}
	d.Close()
	if n := s.count(t, "SELECT count(*) FROM "+table); n != lines {
		t.Fatalf("%d rows", n)
	}
	// Every value in its own column and its own row.
	if n := s.count(t, "SELECT count(*) FROM "+table+" WHERE c0 = seq * 1000 AND c57 = seq * 1000 + 57 AND c99 = seq * 1000 + 99"); n != lines {
		t.Errorf("%d rows with their values in place, want %d", n, lines)
	}
}

func (s Store) buffered(t *testing.T) {
	table := s.table(t, "buf")
	d, err := s.Open(table, PaymentsFormat(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 120; i++ {
		line := fmt.Sprintf("2025-03-09 14:05:06|INFO|x|%d|%d.00|1|n", i, i)
		if i%10 == 0 {
			line = "free text"
		}
		if err := d.Write([]qkernel.Chunk{{Seq: uint64(i), Time: time.Now(), Data: []byte(line)}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(t, "SELECT count(*) FROM "+table); n != 100 {
		t.Errorf("%d rows before closing, want the 100 that filled the buffer twice", n)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, "SELECT count(*) FROM "+table); n != 120 {
		t.Errorf("%d rows", n)
	}
	if d.Misfits() != 12 {
		t.Errorf("%d misfits, want 12", d.Misfits())
	}
	if n := s.count(t, "SELECT sum(amount) FROM "+table); n != (120*121/2-(10+20+30+40+50+60+70+80+90+100+110+120))*100 {
		t.Errorf("amounts sum %d", n)
	}
}
