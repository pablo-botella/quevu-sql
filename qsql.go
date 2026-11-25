// Package qsql keeps a quevu log in a SQL database and gives it back by
// position. It is what the SQLite, MySQL and PostgreSQL stores have in
// common: the table, how a batch is written, how chunks are read by
// position, and the provider that serves them.
//
// It is built on dbh (github.com/pablo-botella/dbh): a database arrives
// here as a *dbh.Dbh, already opened and carrying its dialect, and the
// table is described once as a dbh.TableMeta, from which dbh writes the
// statement that creates it in each dialect. This package imports no
// driver and opens no connection. The packages next to it, one per
// database, do that: each holds its driver and knows how its database
// should be opened, so an application links only the one it imports.
//
// As everywhere in quevu, it has two halves that do not know each other
// at run time:
//
//   - Device is a persistent writer of a hub (qkernel.Writer). Every
//     batch is one transaction. It gives nothing back.
//   - Reader is a stored-data provider (qkernel.Provider), with a
//     connection of its own, so reading never competes with writing.
//     Which role it plays for a console, immediate or historical, is
//     said where it is configured (qkernel.Roles), not here.
//
// A log lives in one table, whose primary key is the position. Several
// logs can share a database, each in a table of its own.
package qsql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pablo-botella/dbh"
	"github.com/pablo-botella/quevu/qkernel"
)

// DefaultTable is the table used when none is named.
const DefaultTable = "log"

// rowsPerStatement bounds one INSERT. Every database limits how many
// values a statement may carry; this is well under all of them.
const rowsPerStatement = 500

// valuesPerStatement bounds the values of one INSERT: with many columns,
// fewer rows go in each statement.
const valuesPerStatement = 30000

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TableName checks a table name and applies the default. Names are plain
// identifiers: dbh does not quote them, and neither does this package.
func TableName(name string) (string, error) {
	if name == "" {
		return DefaultTable, nil
	}
	if !identRE.MatchString(name) {
		return "", fmt.Errorf("qsql: invalid table name %q", name)
	}
	return name, nil
}

// Meta describes the table of a log, once, for every dialect. dbh turns
// it into the statement that creates the table in each database. A
// format adds its columns after the three every table has; nil adds none.
func Meta(table string, format *Format) dbh.TableMeta {
	fields, indexes := format.metaFields()
	return dbh.TableMeta{
		Table:   table,
		Indexes: indexes,
		Fields: append([]dbh.TableMetaField{
			// The position of the chunk in its log.
			{Name: "seq", Type: dbh.TypeBigInt, IsPrimaryKey: true, NotNull: true},
			// When it entered the log, in nanoseconds since the Unix epoch.
			{Name: "time", Type: dbh.TypeBigInt, NotNull: true},
			// The data. It is bytes, not text: this column becomes dbh's
			// binary type as soon as dbh has one. Until then it is
			// declared as text, which SQLite stores untouched and other
			// databases do not.
			{Name: "data", Type: dataType, NotNull: true},
		}, fields...),
	}
}

// dataType is the column type of the data of a chunk. See Meta.
const dataType = dbh.TypeText

// Options tunes a Device.
type Options struct {
	// Table is the table of this log. Default DefaultTable.
	Table string
	// Buffer holds chunks back until this many are waiting, then writes
	// them in one transaction. Zero writes every batch as it arrives.
	// What sits in the buffer is lost if the process dies: how much to
	// risk for how much speed is chosen by whoever configures it.
	Buffer int
	// FlushEvery bounds how long a chunk may wait in the buffer. Zero
	// means only Buffer and Close empty it. Ignored when Buffer is zero.
	FlushEvery time.Duration
	// Format says how the lines of this log are cut into columns of
	// their own, next to the whole line. Nil stores the line and nothing
	// else. It must not be changed once the device is made.
	Format *Format
}

// Device stores the chunks of a log in a table.
type Device struct {
	fails qkernel.Failures // the body of Run, and what it could not write

	db     *dbh.Dbh
	table  string
	format *Format
	insert string // "INSERT INTO t (seq, time, data, ...) VALUES "
	row    string // "(?, ?, ?, ...)"
	width  int    // values per row
	perStm int    // rows per statement
	misfit atomic.Uint64
	last   uint64
	limit  int
	mu     sync.Mutex
	buf    []qkernel.Chunk
	err    error // a failed timed flush, reported by the next Write
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

// NewDevice returns a writer that stores into a table of the database,
// creating the table if needed. The device owns the handle from here on
// and closes it.
func NewDevice(db *dbh.Dbh, opt Options) (*Device, error) {
	table, err := TableName(opt.Table)
	if err != nil {
		return nil, err
	}
	if err := opt.Format.Check(); err != nil {
		return nil, err
	}
	meta := Meta(table, opt.Format)
	if _, err := db.Exec(meta.BuildCreateTable(db.GetDbType())); err != nil {
		return nil, fmt.Errorf("qsql: creating table %s: %w", table, err)
	}
	columns := opt.Format.Columns()
	if len(columns) > 0 {
		// The table may be older than the format. It is not altered from
		// here: what to do with what it already holds is not this
		// package's to decide.
		rows, err := db.Query("SELECT " + strings.Join(columns, ", ") + " FROM " + table + " WHERE 1 = 0")
		if err != nil {
			return nil, fmt.Errorf("qsql: table %s exists without the columns of the format (%s); it is not altered from here: %w",
				table, strings.Join(columns, ", "), err)
		}
		rows.Close()
		for _, stmt := range meta.BuildIndexes(db.GetDbType()) {
			if _, err := db.Exec(stmt); err != nil && !indexExists(err) {
				return nil, fmt.Errorf("qsql: indexing table %s: %w", table, err)
			}
		}
	}
	var last *int64
	if err := db.QueryRow("SELECT max(seq) FROM " + table).Scan(&last); err != nil {
		return nil, fmt.Errorf("qsql: reading table %s: %w", table, err)
	}
	d := &Device{db: db, table: table, limit: opt.Buffer, format: opt.Format}
	d.width = 3 + len(columns)
	d.insert = "INSERT INTO " + table + " (" + strings.Join(append([]string{"seq", "time", "data"}, columns...), ", ") + ") VALUES "
	d.row = "(" + strings.TrimSuffix(strings.Repeat("?, ", d.width), ", ") + ")"
	d.perStm = max(1, min(rowsPerStatement, valuesPerStatement/d.width))
	if last != nil {
		d.last = uint64(*last)
	}
	if d.limit > 0 && opt.FlushEvery > 0 {
		d.stop = make(chan struct{})
		d.done = make(chan struct{})
		go d.flushLoop(opt.FlushEvery)
	}
	return d, nil
}

func (d *Device) flushLoop(every time.Duration) {
	defer close(d.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.mu.Lock()
			if err := d.flushLocked(); err != nil && d.err == nil {
				d.err = err
			}
			d.mu.Unlock()
		}
	}
}

// Write stores a batch, or holds it in the buffer when one is configured.
// With a buffer, an error may belong to chunks accepted by an earlier
// call: the ones a timed flush failed to store.
func (d *Device) Write(batch []qkernel.Chunk) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("qsql: device closed")
	}
	if err := d.err; err != nil {
		d.err = nil
		return err
	}
	if d.limit <= 0 {
		return d.commit(batch)
	}
	d.buf = append(d.buf, batch...)
	if len(d.buf) >= d.limit {
		return d.flushLocked()
	}
	return nil
}

func (d *Device) flushLocked() error {
	if len(d.buf) == 0 {
		return nil
	}
	err := d.commit(d.buf)
	// Kept or lost, the buffer starts again: a batch that cannot be
	// stored must not hold back everything that comes after it.
	d.buf = d.buf[:0]
	return err
}

// commit stores a batch in one transaction, a few hundred rows per
// statement: one statement per row is what makes a database slow.
func (d *Device) commit(batch []qkernel.Chunk) error {
	if len(batch) == 0 {
		return nil
	}
	// The fields of every line are worked out before the transaction
	// opens: it should last as little as possible.
	var fields [][]any
	var misfits uint64
	if d.format != nil {
		fields = make([][]any, len(batch))
		for i, c := range batch {
			values, fits := d.format.Values(c.Data)
			if !fits {
				misfits++
			}
			fields[i] = values
		}
	}
	all := batch
	err := d.db.WithTx(func(tx *dbh.Tx) error {
		batch, at := all, 0
		for len(batch) > 0 {
			n := min(len(batch), d.perStm)
			var sb strings.Builder
			sb.WriteString(d.insert)
			args := make([]any, 0, n*d.width)
			for i, c := range batch[:n] {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(d.row)
				data := c.Data
				if data == nil {
					data = []byte{}
				}
				args = append(args, int64(c.Seq), c.Time.UnixNano(), data)
				if fields != nil {
					args = append(args, fields[at+i]...)
				}
			}
			at += n
			if _, err := tx.Exec(sb.String(), args...); err != nil {
				if dbh.IsUniqueConstraintError(err, "") {
					return fmt.Errorf("qsql: a position in %s is already stored: %w", d.table, err)
				}
				return err
			}
			batch = batch[n:]
		}
		return nil
	})
	if err == nil {
		d.misfit.Add(misfits)
	}
	return err
}

// Misfits returns how many stored lines were not what the format
// describes: fields missing or left over, or a value that did not
// convert. Such a line is stored whole all the same, with the columns it
// could not fill left empty. Always zero without a format.
func (d *Device) Misfits() uint64 { return d.misfit.Load() }

// indexExists reports whether an error only says the index is already
// there, in the words of a database that cannot be asked to create it
// only if missing.
func indexExists(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "duplicate key name") || // MySQL
		strings.Contains(s, "already exists") // PostgreSQL
}

// Close stores what the buffer holds and closes the database handle.
func (d *Device) Close() error {
	if d.stop != nil {
		close(d.stop)
		<-d.done
		d.stop = nil
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	err := errors.Join(d.err, d.flushLocked())
	d.mu.Unlock()
	return errors.Join(err, d.db.Close())
}

// LastSeq returns the last position found in the table when the device
// was made, so the hub goes on counting from there.
func (d *Device) LastSeq() (uint64, error) { return d.last, nil }

// Reader gives back the chunks stored in a table, by position.
type Reader struct {
	table   string
	connect func() (*dbh.Dbh, error)

	mu sync.Mutex
	db *dbh.Dbh
}

// NewReader returns a provider over a table. connect opens the database
// when the first request arrives; it may return nil with no error to say
// "nothing there yet", and is then asked again next time. That is how a
// provider can be set up before anything has been stored.
//
// The handle connect returns is the reader's own and is closed by Close.
func NewReader(connect func() (*dbh.Dbh, error), table string) (*Reader, error) {
	table, err := TableName(table)
	if err != nil {
		return nil, err
	}
	if connect == nil {
		return nil, errors.New("qsql: no way to open the database")
	}
	return &Reader{table: table, connect: connect}, nil
}

func (r *Reader) conn() (*dbh.Dbh, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		return r.db, nil
	}
	db, err := r.connect()
	if err != nil || db == nil {
		return nil, err
	}
	r.db = db
	return db, nil
}

// missingTable reports whether an error says the table does not exist,
// in the words of any of the databases this package serves. A table that
// is not there yet is an empty log, not a failure.
func missingTable(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such table") || // SQLite
		strings.Contains(s, "doesn't exist") || // MySQL
		strings.Contains(s, "does not exist") // PostgreSQL
}

func (r *Reader) query(tail string, n int, args ...any) ([]qkernel.Chunk, error) {
	if n <= 0 {
		return nil, nil
	}
	db, err := r.conn()
	if db == nil {
		return nil, err
	}
	args = append(args, n)
	rows, err := db.Query("SELECT seq, time, data FROM "+r.table+" "+tail+" LIMIT ?", args...)
	if err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []qkernel.Chunk
	for rows.Next() {
		var seq, ns int64
		var data []byte
		if err := rows.Scan(&seq, &ns, &data); err != nil {
			return nil, err
		}
		out = append(out, qkernel.Chunk{Seq: uint64(seq), Time: time.Unix(0, ns), Data: data})
	}
	return out, rows.Err()
}

func reverse(cs []qkernel.Chunk) []qkernel.Chunk {
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs
}

// Tail returns the last n chunks of the table.
func (r *Reader) Tail(n int) ([]qkernel.Chunk, error) {
	cs, err := r.query("ORDER BY seq DESC", n)
	return reverse(cs), err
}

// Before returns the n chunks that precede seq.
func (r *Reader) Before(seq uint64, n int) ([]qkernel.Chunk, error) {
	cs, err := r.query("WHERE seq < ? ORDER BY seq DESC", n, int64(seq))
	return reverse(cs), err
}

// After returns the n chunks that follow seq.
func (r *Reader) After(seq uint64, n int) ([]qkernel.Chunk, error) {
	return r.query("WHERE seq > ? ORDER BY seq ASC", n, int64(seq))
}

// Close releases the connection of the reader.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// History is one provider over several tables, in one database or in
// many. It is the shape a history takes once it has been archived: a
// table per year, a database per month, whatever the application chose.
// Whoever reads it sees one provider and never learns how many places
// the chunks come from.
//
// It is built from the same Reader a single table is read with. What
// makes it a history is only where the configuration puts it.
type History struct {
	qkernel.Provider
	readers []*Reader
}

// NewHistory returns one provider over the readers given, oldest first.
// It owns them from here on.
func NewHistory(oldestFirst ...*Reader) *History {
	h := &History{}
	parts := make([]qkernel.Provider, 0, len(oldestFirst))
	for _, r := range oldestFirst {
		if r != nil {
			h.readers = append(h.readers, r)
			parts = append(parts, r)
		}
	}
	h.Provider = qkernel.Aggregate(parts...)
	return h
}

// Close releases the connections of every reader.
func (h *History) Close() error {
	var errs []error
	for _, r := range h.readers {
		errs = append(errs, r.Close())
	}
	return errors.Join(errs...)
}

var (
	_ qkernel.Writer    = (*Device)(nil)
	_ qkernel.SeqWriter = (*Device)(nil)
	_ qkernel.Provider  = (*Reader)(nil)
)

// Run receives from the channel the hub gave this writer until it is
// closed, writing what arrives in batches, and closes the writer at the
// end. It is what makes it a qkernel.Writer.
func (d *Device) Run(in <-chan qkernel.Chunk) error { return d.fails.Drain(in, 0, d) }

// Failed returns the chunks lost so far in failed batches, and the last
// error.
func (d *Device) Failed() (uint64, error) { return d.fails.Failed() }
