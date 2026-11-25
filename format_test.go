package qsql

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pablo-botella/dbh"
)

func TestFormatIsChecked(t *testing.T) {
	ok := func(fields ...Field) *Format { return &Format{Separator: "|", Fields: fields} }
	good := []*Format{
		nil,
		ok(Field{Name: "a"}),
		ok(Field{Name: "a", Type: Text, Size: 20, Index: true}, Field{}, Field{Name: "b", Type: Int, Index: true}),
		ok(Field{Name: "amount", Type: Decimal, Scale: 18}, Field{Name: "at", Type: Time, Layout: LayoutUnixMilli}),
		ok(Field{Name: "flag", Type: Bool}, Field{Name: "message", Rest: true}),
		{Separator: " :: ", Fields: []Field{{Name: "a"}, {Name: "b"}}},
	}
	for i, f := range good {
		if err := f.Check(); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	many := make([]Field, MaxFields+1)
	for i := range many {
		many[i] = Field{Name: fmt.Sprintf("f%d", i)}
	}
	bad := map[string]*Format{
		"no separator":        {Fields: []Field{{Name: "a"}}},
		"no fields":           {Separator: "|"},
		"too many":            ok(many...),
		"all skipped":         ok(Field{}, Field{}),
		"not a name":          ok(Field{Name: "a b"}),
		"injection":           ok(Field{Name: "a); drop table x; --"}),
		"starts with digit":   ok(Field{Name: "1a"}),
		"taken by table":      ok(Field{Name: "seq"}),
		"taken by table 2":    ok(Field{Name: "Data"}),
		"sql word":            ok(Field{Name: "order"}),
		"sql word upper":      ok(Field{Name: "USER"}),
		"twice":               ok(Field{Name: "a"}, Field{Name: "A"}),
		"rest not last":       ok(Field{Name: "a", Rest: true}, Field{Name: "b"}),
		"index without size":  ok(Field{Name: "a", Index: true}),
		"index too long":      ok(Field{Name: "a", Size: MaxIndexedSize + 1, Index: true}),
		"skipped and indexed": ok(Field{Index: true}, Field{Name: "a"}),
		"negative size":       ok(Field{Name: "a", Size: -1}),
		"huge size":           ok(Field{Name: "a", Size: 20000}),
		"size on int":         ok(Field{Name: "a", Type: Int, Size: 4}),
		"scale on text":       ok(Field{Name: "a", Scale: 2}),
		"scale too big":       ok(Field{Name: "a", Type: Decimal, Scale: 19}),
		"negative scale":      ok(Field{Name: "a", Type: Decimal, Scale: -1}),
		"layout on int":       ok(Field{Name: "a", Type: Int, Layout: LayoutUnix}),
		"unknown type":        ok(Field{Name: "a", Type: FieldType(99)}),
	}
	for name, f := range bad {
		if err := f.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheTableOfAFormatInEveryDialect(t *testing.T) {
	f := &Format{Separator: "|", Fields: []Field{
		{Name: "level", Size: 8, Index: true},
		{}, // skipped: no column
		{Name: "customer", Type: Int, Index: true},
		{Name: "amount", Type: Decimal, Scale: 2},
		{Name: "paid", Type: Bool},
		{Name: "at", Type: Time},
		{Name: "message", Rest: true},
	}}
	if got := f.Columns(); !reflect.DeepEqual(got, []string{"level", "customer", "amount", "paid", "at", "message"}) {
		t.Fatalf("columns %v", got)
	}
	m := Meta("pay", f)
	mysql := m.BuildCreateTable(dbh.MySQL)
	for _, want := range []string{"seq BIGINT", "data TEXT NOT NULL", "level VARCHAR(8)", "customer BIGINT", "amount BIGINT", "paid TINYINT", "at BIGINT", "message TEXT", "PRIMARY KEY (seq)"} {
		if !strings.Contains(mysql, want) {
			t.Errorf("MySQL: no %q in\n%s", want, mysql)
		}
	}
	// The columns of a format can always be empty.
	for _, line := range strings.Split(mysql, "\n")[4:10] {
		if strings.Contains(line, "NOT NULL") {
			t.Errorf("a column of the format that cannot be empty: %s", line)
		}
	}
	sqlite := m.BuildCreateTable(dbh.SQLite)
	for _, want := range []string{"seq INTEGER PRIMARY KEY", "level TEXT", "customer INTEGER", "amount INTEGER", "paid INTEGER", "message TEXT"} {
		if !strings.Contains(sqlite, want) {
			t.Errorf("SQLite: no %q in\n%s", want, sqlite)
		}
	}
	ix := m.BuildIndexes(dbh.MySQL)
	if len(ix) != 2 || !strings.Contains(ix[0], "idx_pay_level ON pay (level)") || !strings.Contains(ix[1], "(customer)") {
		t.Fatalf("indexes %v", ix)
	}
	// Without a format the table is the one it always was.
	if a, b := Meta("pay", nil), Meta("pay", &Format{}); len(a.Fields) != 3 || len(b.Fields) != 3 || len(a.Indexes) != 0 {
		t.Fatal("a table without format grew")
	}
}

func TestValues(t *testing.T) {
	utc := time.UTC
	f := &Format{Separator: "|", Location: utc, Fields: []Field{
		{Name: "at", Type: Time, Layout: "2006-01-02 15:04:05"},
		{Name: "level", Size: 5},
		{}, // skipped
		{Name: "customer", Type: Int},
		{Name: "amount", Type: Decimal, Scale: 2},
		{Name: "paid", Type: Bool},
		{Name: "message", Rest: true},
	}}
	if err := f.Check(); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 3, 9, 14, 5, 6, 0, utc).UnixNano()
	cases := []struct {
		name string
		line string
		want []any
		fits bool
	}{
		{"plain", "2025-03-09 14:05:06|INFO|x|42|12.50|yes|charged", []any{at, "INFO", int64(42), int64(1250), int64(1), "charged"}, true},
		{"blanks around", " 2025-03-09 14:05:06 | INFO |  | 42 | 12.5 | Y | charged ok \r\n", []any{at, "INFO", int64(42), int64(1250), int64(1), "charged ok"}, true},
		{"colors", "\x1b[90m2025-03-09 14:05:06\x1b[0m|\x1b[31mERROR\x1b[0m|x|-7|-0.05|0|\x1b[1mdeclined\x1b[0m", []any{at, "ERROR", int64(-7), int64(-5), int64(0), "declined"}, true},
		{"separators in the rest", "2025-03-09 14:05:06|INFO|x|1|1|1|a|b||c", []any{at, "INFO", int64(1), int64(100), int64(1), "a|b||c"}, true},
		{"empty pieces are no value", "||||||", []any{nil, nil, nil, nil, nil, nil}, true},
		{"short", "2025-03-09 14:05:06|INFO", []any{at, "INFO", nil, nil, nil, nil}, false},
		{"no separators at all", "panic: something broke", []any{nil, nil, nil, nil, nil, nil}, false},
		{"empty line", "", []any{nil, nil, nil, nil, nil, nil}, false},
		{"bad number", "2025-03-09 14:05:06|INFO|x|4x2|1.00|1|m", []any{at, "INFO", nil, int64(100), int64(1), "m"}, false},
		{"float where int", "2025-03-09 14:05:06|INFO|x|4.0|1.00|1|m", []any{at, "INFO", nil, int64(100), int64(1), "m"}, false},
		{"too many decimals", "2025-03-09 14:05:06|INFO|x|4|1.005|1|m", []any{at, "INFO", int64(4), nil, int64(1), "m"}, false},
		{"zeros beyond the scale", "2025-03-09 14:05:06|INFO|x|4|1.5000|1|m", []any{at, "INFO", int64(4), int64(150), int64(1), "m"}, true},
		{"bad bool", "2025-03-09 14:05:06|INFO|x|4|1|maybe|m", []any{at, "INFO", int64(4), int64(100), nil, "m"}, false},
		{"bad time", "09/03/2025|INFO|x|4|1|1|m", []any{nil, "INFO", int64(4), int64(100), int64(1), "m"}, false},
		{"long text is cut by characters", "2025-03-09 14:05:06|ÁÉÍÓÚÑ€😀|x|4|1|1|m", []any{at, "ÁÉÍÓÚ", int64(4), int64(100), int64(1), "m"}, true},
		{"broken text is mended", "2025-03-09 14:05:06|a\xffb|x|4|1|1|caf\xc3", []any{at, "a\uFFFDb", int64(4), int64(100), int64(1), "caf\uFFFD"}, true},
		{"limits", "2025-03-09 14:05:06|I|x|-9223372036854775808|92233720368547758.07|ON|m", []any{at, "I", int64(math.MinInt64), int64(math.MaxInt64), int64(1), "m"}, true},
		{"over the limits", "2025-03-09 14:05:06|I|x|9223372036854775808|92233720368547758.08|1|m", []any{at, "I", nil, nil, int64(1), "m"}, false},
	}
	for _, c := range cases {
		got, fits := f.Values([]byte(c.line))
		if !reflect.DeepEqual(got, c.want) || fits != c.fits {
			t.Errorf("%s:\n got  %#v fits %v\n want %#v fits %v", c.name, got, fits, c.want, c.fits)
		}
	}

	// Without Rest, pieces left over are a misfit and are not stored.
	g := &Format{Separator: ";;", Fields: []Field{{Name: "a"}, {Name: "b", Type: Int}}}
	if got, fits := g.Values([]byte("x;;5;;more")); !reflect.DeepEqual(got, []any{"x", int64(5)}) || fits {
		t.Errorf("left over: %#v %v", got, fits)
	}
	if got, fits := g.Values([]byte("x;5;;7")); !reflect.DeepEqual(got, []any{"x;5", int64(7)}) || !fits {
		t.Errorf("long separator: %#v %v", got, fits)
	}
	// No format, no values, and nothing to object to.
	if got, fits := (*Format)(nil).Values([]byte("x")); got != nil || !fits {
		t.Error("nil format")
	}
}

func TestTimes(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skip("no zone database:", err)
	}
	one := func(layout string, loc *time.Location, s string) (int64, bool) {
		f := &Format{Separator: "|", Location: loc, Fields: []Field{{Name: "at", Type: Time, Layout: layout}}}
		v, fits := f.Values([]byte(s))
		if v[0] == nil {
			return 0, fits
		}
		return v[0].(int64), fits
	}
	want := time.Date(2025, 7, 1, 10, 0, 0, 0, time.UTC).UnixNano()
	for _, c := range []struct {
		layout, s string
		loc       *time.Location
	}{
		{"", "2025-07-01T10:00:00Z", nil},
		{"", "2025-07-01T12:00:00+02:00", nil},
		{"", "2025-07-01T10:00:00.000000000Z", madrid},
		{"2006-01-02 15:04:05", "2025-07-01 12:00:00", madrid}, // no zone written: the one configured
		{"2006-01-02 15:04:05", "2025-07-01 10:00:00", time.UTC},
		{"2006-01-02 15:04:05 -0700", "2025-07-01 05:00:00 -0500", madrid}, // a zone written wins
		{LayoutUnix, "1751364000", nil},
		{LayoutUnixMilli, "1751364000000", nil},
		{LayoutUnixMicro, "1751364000000000", nil},
		{LayoutUnixNano, "1751364000000000000", nil},
	} {
		if got, fits := one(c.layout, c.loc, c.s); got != want || !fits {
			t.Errorf("%q %q: got %d fits %v, want %d", c.layout, c.s, got, fits, want)
		}
	}
	// With no location configured, a time without zone is local.
	local := time.Date(2025, 7, 1, 10, 0, 0, 0, time.Local).UnixNano()
	if got, _ := one("2006-01-02 15:04:05", nil, "2025-07-01 10:00:00"); got != local {
		t.Errorf("local: %d, want %d", got, local)
	}
	// Fractions survive to the nanosecond.
	if got, _ := one("", nil, "2025-07-01T10:00:00.123456789Z"); got != want+123456789 {
		t.Errorf("fraction: %d", got-want)
	}
	// What does not fit in the column does not convert, instead of
	// wrapping around to another date.
	for _, c := range [][2]string{
		{"", "1500-01-01T00:00:00Z"}, {"", "2400-01-01T00:00:00Z"},
		{LayoutUnix, "99999999999999"}, {LayoutUnixMilli, "9999999999999999999"},
		{LayoutUnix, "12.5"}, {LayoutUnix, "soon"}, {"", "2025-13-01T00:00:00Z"},
	} {
		if got, fits := one(c[0], nil, c[1]); got != 0 || fits {
			t.Errorf("%q %q accepted as %d", c[0], c[1], got)
		}
	}
}

// Decimals are checked against arithmetic that cannot be wrong: the same
// text read by math/big.
func TestDecimalsAgainstBig(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	maxI, minI := big.NewInt(math.MaxInt64), big.NewInt(math.MinInt64)
	accepted, refused := 0, 0
	for i := 0; i < 200000; i++ {
		scale := rnd.Intn(19)
		var sb strings.Builder
		switch rnd.Intn(6) {
		case 0:
			sb.WriteByte('-')
		case 1:
			sb.WriteByte('+')
		}
		nw := rnd.Intn(21)
		for j := 0; j < nw; j++ {
			sb.WriteByte(byte('0' + rnd.Intn(10)))
		}
		nf := 0
		if rnd.Intn(3) > 0 {
			sb.WriteByte('.')
			nf = rnd.Intn(scale + 3)
			for j := 0; j < nf; j++ {
				d := rnd.Intn(10)
				if j >= scale && rnd.Intn(2) == 0 {
					d = 0
				}
				sb.WriteByte(byte('0' + d))
			}
		}
		if rnd.Intn(40) == 0 {
			sb.WriteString([]string{"x", " ", ".", "e3", ","}[rnd.Intn(5)])
		}
		s := sb.String()
		if s == "" {
			continue
		}
		got, ok := parseDecimal(s, scale)

		// The reference: exact, by math/big.
		wantOK := false
		var want int64
		body := strings.TrimLeft(s, "+-")
		signs := len(s) - len(body)
		valid := signs <= 1 && body != "" && body != "." && strings.Count(body, ".") <= 1 && strings.Trim(body, "0123456789.") == ""
		if valid {
			r, good := new(big.Rat).SetString(s)
			if good {
				r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
				if r.IsInt() && r.Num().Cmp(maxI) <= 0 && r.Num().Cmp(minI) >= 0 {
					wantOK, want = true, r.Num().Int64()
				}
			}
		}
		if ok != wantOK || (ok && got != want) {
			t.Fatalf("parseDecimal(%q, %d) = %d, %v; exact arithmetic says %d, %v", s, scale, got, ok, want, wantOK)
		}
		if ok {
			accepted++
		} else {
			refused++
		}
	}
	if accepted < 10000 || refused < 10000 {
		t.Fatalf("the test did not try both sides enough: %d accepted, %d refused", accepted, refused)
	}
}

// Whatever a line holds, cutting it never fails and always gives one
// value per column.
func FuzzValues(f *testing.F) {
	fm := &Format{Separator: "|", Location: time.UTC, Fields: []Field{
		{Name: "at", Type: Time}, {Name: "l", Size: 3}, {}, {Name: "n", Type: Int},
		{Name: "d", Type: Decimal, Scale: 4}, {Name: "b", Type: Bool}, {Name: "m", Rest: true},
	}}
	for _, s := range []string{"", "|", "||||||", "2025-01-01T00:00:00Z|abc|x|1|1.5|t|m|m", "\x1b[31m|\xff|", "-|+|.|-.|+.|", "a|b"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, _ := fm.Values(data)
		if len(v) != 6 {
			t.Fatalf("%d values", len(v))
		}
		for i, x := range v {
			switch x := x.(type) {
			case nil, int64:
			case string:
				if i != 1 && i != 5 {
					t.Fatalf("text in column %d", i)
				}
				if x == "" || !strings.EqualFold(strings.ToValidUTF8(x, "?"), x) {
					t.Fatalf("text %q", x)
				}
			default:
				t.Fatalf("a %T", x)
			}
		}
	})
}
