package qsql

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pablo-botella/dbh"
	"github.com/pablo-botella/quevu/qkernel"
)

// Every log is its own: what is useful in one is noise in another. A
// Format says, for one log, how a line is cut into fields and what each
// field becomes in the table: a column of its own, with a type, next to
// the three every table has (position, time and the whole line).
//
// The whole line is always stored, untouched. The columns are an addition
// to query by, never a replacement: a line the format does not fit loses
// nothing, it only has empty columns.

// FieldType is what a field becomes.
type FieldType int

const (
	// Text is kept as it is. With a Size it is a bounded column
	// (VARCHAR), which is what an index needs; without one it is
	// unbounded.
	Text FieldType = iota + 1
	// Int is a whole number, with or without sign.
	Int
	// Decimal is a number with a fixed count of decimals (Scale), stored
	// exactly as a whole number of the smallest unit: with Scale 2,
	// "12.50" is stored as 1250. It is the type for money. A value with
	// more decimals than Scale is not rounded: it does not convert.
	Decimal
	// Bool is yes or no: 1/0, true/false, t/f, yes/no, y/n, on/off.
	// Stored as 1 or 0.
	Bool
	// Time is a moment, read with Layout and stored like the time of the
	// chunk: nanoseconds since the Unix epoch.
	Time
)

func (t FieldType) String() string {
	switch t {
	case Text:
		return "text"
	case Int:
		return "int"
	case Decimal:
		return "decimal"
	case Bool:
		return "bool"
	case Time:
		return "time"
	}
	return fmt.Sprintf("FieldType(%d)", int(t))
}

// Layouts of a Time field that are not a Go layout: a number counted from
// the Unix epoch.
const (
	LayoutUnix      = "unix"      // seconds
	LayoutUnixMilli = "unixmilli" // milliseconds
	LayoutUnixMicro = "unixmicro" // microseconds
	LayoutUnixNano  = "unixnano"  // nanoseconds
)

// Field is one piece of a line.
type Field struct {
	// Name is the column. Empty means the piece at this place is not
	// stored: it is skipped.
	Name string
	// Type is what the piece becomes. Zero means Text.
	Type FieldType
	// Size bounds a Text field, in characters. A longer value is cut to
	// fit (the whole line is in its own column anyway). Needed to index
	// a Text field.
	Size int
	// Scale is the count of decimals of a Decimal field, 0 to 18.
	Scale int
	// Layout says how a Time field is written: a layout of the time
	// package ("2006-01-02 15:04:05"), or one of LayoutUnix and its
	// siblings. Empty means RFC 3339.
	Layout string
	// Index asks for an index on the column.
	Index bool
	// Rest, on the last field only, makes it keep everything that is
	// left of the line, separators included.
	Rest bool
}

// Format says how the lines of one log are cut and stored.
type Format struct {
	// Separator stands between two fields. It may be longer than one
	// character.
	Separator string
	// Fields, in the order they come in the line.
	Fields []Field
	// Location is where a Time field written without a zone is taken to
	// be. Nil means the local zone of the machine.
	Location *time.Location
}

// MaxFields bounds the fields of a format.
const MaxFields = 100

// MaxIndexedSize bounds the Size of a Text field that is indexed: the
// longest key every database served here accepts.
const MaxIndexedSize = 768

// reserved are the names a column cannot have: the three of every table,
// and the words a database would take for something else. Names are not
// quoted, here or in dbh.
var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`seq time data
		add all alter analyze and any as asc before between by case check
		column condition constraint create cross current_date current_time
		current_timestamp current_user database default delete desc
		distinct do drop else end except exists false for foreign from
		full function grant group having if in index inner insert
		intersect interval into is join key keys left like limit lock
		match natural not null offset on only or order outer over
		partition primary procedure range rank read references right row
		rows schema select session_user set table then to trigger true
		union unique update usage user using values when where window
		with write`) {
		reserved[w] = true
	}
}

// Check says whether the format can be used, and why not.
func (f *Format) Check() error {
	if f == nil {
		return nil
	}
	if f.Separator == "" {
		return errors.New("qsql: format with no separator")
	}
	if len(f.Fields) == 0 {
		return errors.New("qsql: format with no fields")
	}
	if len(f.Fields) > MaxFields {
		return fmt.Errorf("qsql: format with %d fields, more than %d", len(f.Fields), MaxFields)
	}
	seen := map[string]bool{}
	stored := 0
	for i, fd := range f.Fields {
		where := fmt.Sprintf("qsql: field %d (%q)", i+1, fd.Name)
		if fd.Rest && i != len(f.Fields)-1 {
			return fmt.Errorf("%s: only the last field can keep the rest", where)
		}
		if fd.Name == "" {
			if fd.Index {
				return fmt.Errorf("%s: a skipped field cannot be indexed", where)
			}
			continue
		}
		stored++
		low := strings.ToLower(fd.Name)
		if !identRE.MatchString(fd.Name) {
			return fmt.Errorf("%s: not a plain name", where)
		}
		if reserved[low] {
			return fmt.Errorf("%s: the name is taken by the table or by SQL; choose another", where)
		}
		if seen[low] {
			return fmt.Errorf("%s: the name is used twice", where)
		}
		seen[low] = true
		if fd.Size < 0 {
			return fmt.Errorf("%s: negative size", where)
		}
		switch fd.kind() {
		case Text:
			if fd.Index && fd.Size == 0 {
				return fmt.Errorf("%s: a text field needs a Size to be indexed", where)
			}
			if fd.Index && fd.Size > MaxIndexedSize {
				return fmt.Errorf("%s: an indexed text field can have a Size of %d at most", where, MaxIndexedSize)
			}
			if fd.Size > 16000 {
				return fmt.Errorf("%s: a Size over 16000; leave it at zero for unbounded text", where)
			}
		case Decimal:
			if fd.Scale < 0 || fd.Scale > 18 {
				return fmt.Errorf("%s: a scale of %d, out of 0 to 18", where, fd.Scale)
			}
		case Int, Bool, Time:
		default:
			return fmt.Errorf("%s: unknown type %d", where, int(fd.Type))
		}
		if fd.kind() != Text && fd.Size != 0 {
			return fmt.Errorf("%s: only a text field has a Size", where)
		}
		if fd.kind() != Decimal && fd.Scale != 0 {
			return fmt.Errorf("%s: only a decimal field has a Scale", where)
		}
		if fd.kind() != Time && fd.Layout != "" {
			return fmt.Errorf("%s: only a time field has a Layout", where)
		}
	}
	if stored == 0 {
		return errors.New("qsql: format where every field is skipped")
	}
	return nil
}

func (fd Field) kind() FieldType {
	if fd.Type == 0 {
		return Text
	}
	return fd.Type
}

// Columns returns the names of the columns the format adds, in order.
func (f *Format) Columns() []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, fd := range f.Fields {
		if fd.Name != "" {
			out = append(out, fd.Name)
		}
	}
	return out
}

// metaFields describes the columns of the format for dbh.
func (f *Format) metaFields() ([]dbh.TableMetaField, []dbh.TableMetaIndex) {
	if f == nil {
		return nil, nil
	}
	var fields []dbh.TableMetaField
	var indexes []dbh.TableMetaIndex
	for _, fd := range f.Fields {
		if fd.Name == "" {
			continue
		}
		mf := dbh.TableMetaField{Name: fd.Name}
		switch fd.kind() {
		case Text:
			if fd.Size > 0 {
				mf.Type, mf.MaxSize = dbh.TypeVarchar, fd.Size
			} else {
				mf.Type = dbh.TypeText
			}
		case Bool:
			mf.Type = dbh.TypeTinyInt
		default: // Int, Decimal, Time
			mf.Type = dbh.TypeBigInt
		}
		fields = append(fields, mf)
		if fd.Index {
			indexes = append(indexes, dbh.TableMetaIndex{Columns: []string{fd.Name}})
		}
	}
	return fields, indexes
}

// Values cuts a line and converts its fields. It returns one value per
// column of the format, in the order of Columns: nil where the line has
// nothing for it or what it has does not convert. fits is false when the
// line is not what the format describes: fields missing or left over, or
// a value that does not convert.
//
// Colors and other terminal sequences are taken out before cutting, and
// every piece loses the blanks around it. An empty piece is no value.
func (f *Format) Values(data []byte) (values []any, fits bool) {
	if f == nil {
		return nil, true
	}
	line := string(qkernel.PlainText(nil, data))
	line = strings.TrimRight(line, "\r\n")
	var pieces []string
	last := len(f.Fields) - 1
	if f.Fields[last].Rest {
		pieces = strings.SplitN(line, f.Separator, len(f.Fields))
	} else {
		pieces = strings.Split(line, f.Separator)
	}
	fits = len(pieces) == len(f.Fields)
	values = make([]any, 0, len(f.Fields))
	for i, fd := range f.Fields {
		if fd.Name == "" {
			continue
		}
		if i >= len(pieces) {
			values = append(values, nil)
			continue
		}
		v, ok := f.convert(fd, strings.TrimSpace(pieces[i]))
		if !ok {
			fits = false
		}
		values = append(values, v)
	}
	return values, fits
}

// convert turns one piece into the value of its column. An empty piece
// is no value, and that is not a misfit.
func (f *Format) convert(fd Field, s string) (any, bool) {
	if s == "" {
		return nil, true
	}
	switch fd.kind() {
	case Text:
		s = strings.ToValidUTF8(s, "�")
		if fd.Size > 0 && utf8.RuneCountInString(s) > fd.Size {
			n := 0
			for i := range s {
				if n == fd.Size {
					s = s[:i]
					break
				}
				n++
			}
		}
		return s, true
	case Int:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return n, true
	case Decimal:
		n, ok := parseDecimal(s, fd.Scale)
		if !ok {
			return nil, false
		}
		return n, true
	case Bool:
		switch strings.ToLower(s) {
		case "1", "true", "t", "yes", "y", "on":
			return int64(1), true
		case "0", "false", "f", "no", "n", "off":
			return int64(0), true
		}
		return nil, false
	case Time:
		t, ok := f.parseTime(fd.Layout, s)
		if !ok {
			return nil, false
		}
		return t.UnixNano(), true
	}
	return nil, false
}

// The moments that fit in nanoseconds since the epoch.
var (
	minTime = time.Unix(0, math.MinInt64)
	maxTime = time.Unix(0, math.MaxInt64)
)

func (f *Format) parseTime(layout, s string) (time.Time, bool) {
	var per int64
	switch layout {
	case LayoutUnix:
		per = int64(time.Second)
	case LayoutUnixMilli:
		per = int64(time.Millisecond)
	case LayoutUnixMicro:
		per = int64(time.Microsecond)
	case LayoutUnixNano:
		per = 1
	}
	if per != 0 {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n > math.MaxInt64/per || n < math.MinInt64/per {
			return time.Time{}, false
		}
		return time.Unix(0, n*per), true
	}
	if layout == "" {
		layout = time.RFC3339
	}
	loc := f.Location
	if loc == nil {
		loc = time.Local
	}
	t, err := time.ParseInLocation(layout, s, loc)
	if err != nil || t.Before(minTime) || t.After(maxTime) {
		return time.Time{}, false
	}
	return t, true
}

// parseDecimal reads a number with at most scale decimals as a whole
// number of the smallest unit. Nothing is rounded and nothing overflows
// in silence: what does not fit exactly does not convert.
func parseDecimal(s string, scale int) (int64, bool) {
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, false
	}
	if len(frac) > scale {
		// Decimals beyond the scale are accepted only when they are
		// zeros: nothing is lost by dropping them.
		if strings.Trim(frac[scale:], "0") != "" {
			return 0, false
		}
		frac = frac[:scale]
	}
	digits := whole + frac + strings.Repeat("0", scale-len(frac))
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, true
	}
	if neg {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
