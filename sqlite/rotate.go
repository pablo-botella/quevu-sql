package qsqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	qsql "github.com/pablo-botella/quevu-sql"
	"github.com/pablo-botella/quevu/qkernel"
)

// Rotation names a log kept in one database file per period: a file per
// month, per day, per year. Old periods are whole files that can be
// moved, archived or deleted without touching the live one.
//
// The files are Dir/Base-PERIOD.db, where PERIOD is the moment written
// with Layout: "app-log-2025-03.db".
type Rotation struct {
	// Dir is the folder of the files. Empty: the current one.
	Dir string
	// Base is the beginning of every file name.
	Base string
	// Layout writes the period, as a layout of the time package. It is
	// what decides how long a period is: "2006-01" a month (the
	// default), "2006-01-02" a day, "2006" a year. It must sort as text
	// in the order of time, as those do.
	Layout string
	// Location is the zone the period is told in. Nil: the local one.
	Location *time.Location
}

// Monthly is the layout of a file per month.
const Monthly = "2006-01"

func (r Rotation) layout() string {
	if r.Layout == "" {
		return Monthly
	}
	return r.Layout
}

func (r Rotation) location() *time.Location {
	if r.Location == nil {
		return time.Local
	}
	return r.Location
}

// Period returns the name of the period a moment belongs to.
func (r Rotation) Period(t time.Time) string { return t.In(r.location()).Format(r.layout()) }

// Path returns the file of the period a moment belongs to.
func (r Rotation) Path(t time.Time) string {
	return filepath.Join(r.Dir, r.Base+"-"+r.Period(t)+".db")
}

func (r Rotation) check() error {
	if r.Base == "" {
		return errors.New("qsqlite: rotation with no base name")
	}
	if strings.ContainsAny(r.Base, `/\*?[`) {
		return fmt.Errorf("qsqlite: invalid base name %q", r.Base)
	}
	// A layout must tell periods apart, and a period must be readable
	// back from a file name: both fail with a layout that writes no date.
	one := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	other := time.Date(2019, 11, 25, 14, 15, 16, 0, time.UTC)
	name := one.Format(r.layout())
	if back, err := time.ParseInLocation(r.layout(), name, time.UTC); err != nil || back.Format(r.layout()) != name || name == other.Format(r.layout()) {
		return fmt.Errorf("qsqlite: %q is not a layout that tells periods apart", r.layout())
	}
	return nil
}

// Files returns the files of the rotation that exist, oldest first. A
// file whose name does not carry a period of the layout is not one of
// them.
func (r Rotation) Files() ([]string, error) {
	dir := r.Dir
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type file struct {
		at   time.Time
		path string
	}
	var files []file
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, r.Base+"-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		period := strings.TrimSuffix(strings.TrimPrefix(name, r.Base+"-"), ".db")
		at, err := time.ParseInLocation(r.layout(), period, time.UTC)
		if err != nil || at.Format(r.layout()) != period {
			continue
		}
		files = append(files, file{at, filepath.Join(r.Dir, name)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out, nil
}

// Rotating stores a log in the file of the period each chunk belongs to,
// by the time the chunk carries. When a period ends the next chunk opens
// the next file by itself; nothing has to be restarted.
type Rotating struct {
	fails qkernel.Failures // the body of Run, and what it could not write

	rot  Rotation
	opt  Options
	last uint64

	mu      sync.Mutex
	period  string
	dev     *qsql.Device
	misfits uint64 // of the devices already closed
	closed  bool
}

// OpenRotating opens the file of the current period, creating the folder,
// the file and the table if needed, and returns the writer.
//
// Positions go on from one file to the next: when the file of the
// current period is new, the last position is looked for in the files
// before it.
func OpenRotating(rot Rotation, opt Options) (*Rotating, error) {
	if err := rot.check(); err != nil {
		return nil, err
	}
	if _, err := qsql.TableName(opt.Table); err != nil {
		return nil, err
	}
	if rot.Dir != "" {
		if err := os.MkdirAll(rot.Dir, 0o755); err != nil {
			return nil, err
		}
	}
	r := &Rotating{rot: rot, opt: opt}
	now := time.Now()
	if err := r.open(now); err != nil {
		return nil, err
	}
	last, _ := r.dev.LastSeq()
	if last == 0 {
		// A new period: the count goes on from the newest file that has
		// something of this log.
		files, err := rot.Files()
		if err != nil {
			r.dev.Close()
			return nil, err
		}
		current := rot.Path(now)
		for i := len(files) - 1; i >= 0 && last == 0; i-- {
			if files[i] == current {
				continue
			}
			rd, err := NewReader(files[i], opt.Table)
			if err != nil {
				continue
			}
			if tail, err := rd.Tail(1); err == nil && len(tail) == 1 {
				last = tail[0].Seq
			}
			rd.Close()
		}
	}
	r.last = last
	return r, nil
}

// open makes the device of the period t belongs to the current one. Call
// it with mu held, or before the Rotating is shared.
func (r *Rotating) open(t time.Time) error {
	period := r.rot.Period(t)
	if r.dev != nil && period == r.period {
		return nil
	}
	dev, err := Open(r.rot.Path(t), r.opt)
	if err != nil {
		return err
	}
	var closeErr error
	if r.dev != nil {
		r.misfits += r.dev.Misfits()
		closeErr = r.dev.Close()
	}
	r.dev, r.period = dev, period
	return closeErr
}

// Write stores a batch, each chunk in the file of its period. A batch
// that crosses the end of a period is split there.
func (r *Rotating) Write(batch []qkernel.Chunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("qsqlite: device closed")
	}
	var errs []error
	for len(batch) > 0 {
		period := r.rot.Period(batch[0].Time)
		n := 1
		for n < len(batch) && r.rot.Period(batch[n].Time) == period {
			n++
		}
		if err := r.open(batch[0].Time); err != nil {
			errs = append(errs, err)
			if r.dev == nil || r.period != period {
				// The file of this period cannot be had: what belongs to
				// it is not stored. What follows may still be.
				batch = batch[n:]
				continue
			}
		}
		if err := r.dev.Write(batch[:n]); err != nil {
			errs = append(errs, err)
		}
		batch = batch[n:]
	}
	return errors.Join(errs...)
}

// LastSeq returns the last position found when the device was made, in
// the file of the current period or, when that one is new, in the ones
// before it.
func (r *Rotating) LastSeq() (uint64, error) { return r.last, nil }

// Misfits returns how many stored lines were not what the format
// describes, over every file written since the device was made.
func (r *Rotating) Misfits() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dev == nil {
		return r.misfits
	}
	return r.misfits + r.dev.Misfits()
}

// Close stores what the buffer holds and closes the file in use.
func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.dev == nil {
		return nil
	}
	r.misfits += r.dev.Misfits()
	err := r.dev.Close()
	r.dev = nil
	return err
}

// Current is a stored-data provider over the file of the period in
// course: what was stored lately. When the period changes it follows to
// the new file by itself. What older files hold is History's to give.
type Current struct {
	rot   Rotation
	table string
	now   func() time.Time

	mu     sync.Mutex
	period string
	rd     *qsql.Reader
}

// NewCurrent returns the provider. Nothing needs to exist yet.
func NewCurrent(rot Rotation, table string) (*Current, error) {
	if err := rot.check(); err != nil {
		return nil, err
	}
	if _, err := qsql.TableName(table); err != nil {
		return nil, err
	}
	return &Current{rot: rot, table: table, now: time.Now}, nil
}

func (c *Current) reader() (*qsql.Reader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now()
	period := c.rot.Period(t)
	if c.rd != nil && period == c.period {
		return c.rd, nil
	}
	rd, err := NewReader(c.rot.Path(t), c.table)
	if err != nil {
		return nil, err
	}
	if c.rd != nil {
		c.rd.Close()
	}
	c.rd, c.period = rd, period
	return rd, nil
}

// Tail returns the last n chunks of the period in course.
func (c *Current) Tail(n int) ([]qkernel.Chunk, error) {
	rd, err := c.reader()
	if err != nil {
		return nil, err
	}
	return rd.Tail(n)
}

// Before returns the n chunks of the period in course that precede seq.
func (c *Current) Before(seq uint64, n int) ([]qkernel.Chunk, error) {
	rd, err := c.reader()
	if err != nil {
		return nil, err
	}
	return rd.Before(seq, n)
}

// After returns the n chunks of the period in course that follow seq.
func (c *Current) After(seq uint64, n int) ([]qkernel.Chunk, error) {
	rd, err := c.reader()
	if err != nil {
		return nil, err
	}
	return rd.After(seq, n)
}

// Close releases the connection in use.
func (c *Current) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rd == nil {
		return nil
	}
	err := c.rd.Close()
	c.rd = nil
	return err
}

// Periods is one provider over every file of a rotation, oldest first:
// the whole history of the log, however many periods it spans. Files
// that appear or go away are noticed at the next request.
type Periods struct {
	rot   Rotation
	table string

	mu    sync.Mutex
	files string // the list the history in use was made from
	hist  *qsql.History
}

// NewPeriods returns the provider. Nothing needs to exist yet.
func NewPeriods(rot Rotation, table string) (*Periods, error) {
	if err := rot.check(); err != nil {
		return nil, err
	}
	if _, err := qsql.TableName(table); err != nil {
		return nil, err
	}
	return &Periods{rot: rot, table: table}, nil
}

func (p *Periods) history() (*qsql.History, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	files, err := p.rot.Files()
	if err != nil {
		return nil, err
	}
	key := strings.Join(files, "\n")
	if p.hist != nil && key == p.files {
		return p.hist, nil
	}
	tables := make([]Table, len(files))
	for i, f := range files {
		tables[i] = Table{Path: f, Name: p.table}
	}
	hist, err := NewHistory(tables...)
	if err != nil {
		return nil, err
	}
	if p.hist != nil {
		p.hist.Close()
	}
	p.hist, p.files = hist, key
	return hist, nil
}

// Tail returns the last n chunks of the whole log.
func (p *Periods) Tail(n int) ([]qkernel.Chunk, error) {
	h, err := p.history()
	if err != nil {
		return nil, err
	}
	return h.Tail(n)
}

// Before returns the n chunks that precede seq, across files.
func (p *Periods) Before(seq uint64, n int) ([]qkernel.Chunk, error) {
	h, err := p.history()
	if err != nil {
		return nil, err
	}
	return h.Before(seq, n)
}

// After returns the n chunks that follow seq, across files.
func (p *Periods) After(seq uint64, n int) ([]qkernel.Chunk, error) {
	h, err := p.history()
	if err != nil {
		return nil, err
	}
	return h.After(seq, n)
}

// Close releases the connections in use.
func (p *Periods) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hist == nil {
		return nil
	}
	err := p.hist.Close()
	p.hist, p.files = nil, ""
	return err
}

var (
	_ qkernel.Writer    = (*Rotating)(nil)
	_ qkernel.SeqWriter = (*Rotating)(nil)
	_ qkernel.Provider  = (*Current)(nil)
	_ qkernel.Provider  = (*Periods)(nil)
)

// Run receives from the channel the hub gave this writer until it is
// closed, writing what arrives in batches, and closes the writer at the
// end. It is what makes it a qkernel.Writer.
func (r *Rotating) Run(in <-chan qkernel.Chunk) error { return r.fails.Drain(in, 0, r) }

// Failed returns the chunks lost so far in failed batches, and the last
// error.
func (r *Rotating) Failed() (uint64, error) { return r.fails.Failed() }
