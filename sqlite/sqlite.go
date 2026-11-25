// Package qsqlite is the SQLite store of quevu: it holds the driver and
// knows how a SQLite database should be opened, and leaves everything
// else to the common piece (github.com/pablo-botella/quevu-sql).
//
// SQLite is slow when every insert is committed on its own and very fast
// when many share a transaction. The store offers the whole range between
// the two (see Options.Buffer), and whoever configures it chooses.
//
// The driver is modernc.org/sqlite, which is pure Go: no C compiler is
// needed to build, and cross compilation keeps working.
package qsqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pablo-botella/dbh"
	qsql "github.com/pablo-botella/quevu-sql"
	_ "modernc.org/sqlite"
)

// Options tunes a device.
type Options struct {
	// Table is the table of this log. Default qsql.DefaultTable.
	Table string
	// Sync makes every commit wait for the disk (synchronous=FULL).
	// Without it SQLite runs in WAL mode with synchronous=NORMAL: a
	// commit survives the death of the process, and a power cut may lose
	// the last ones without damaging the database.
	Sync bool
	// Buffer and FlushEvery hold chunks back to write them together: see
	// qsql.Options.
	Buffer     int
	FlushEvery time.Duration
	// Format cuts the lines of this log into columns of their own: see
	// qsql.Format. Nil stores the line and nothing else.
	Format *qsql.Format
}

// connect opens a database file the way this store wants it: one
// connection, so the settings below stay in force.
func connect(path string, pragmas ...string) (*dbh.Dbh, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range pragmas {
		if _, err := db.Exec("PRAGMA " + p); err != nil {
			db.Close()
			return nil, fmt.Errorf("qsqlite: PRAGMA %s: %w", p, err)
		}
	}
	return dbh.New(db, dbh.SQLite), nil
}

// Open opens the database, creating the file and the table if needed,
// and returns the writer that stores a log in it.
func Open(path string, opt Options) (*qsql.Device, error) {
	if _, err := qsql.TableName(opt.Table); err != nil {
		return nil, err
	}
	synchronous := "NORMAL"
	if opt.Sync {
		synchronous = "FULL"
	}
	db, err := connect(path, "busy_timeout = 5000", "journal_mode = WAL", "synchronous = "+synchronous)
	if err != nil {
		return nil, err
	}
	d, err := qsql.NewDevice(db, qsql.Options{Table: opt.Table, Buffer: opt.Buffer, FlushEvery: opt.FlushEvery, Format: opt.Format})
	if err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// NewReader returns a stored-data provider over one table of the
// database at path; an empty table name means qsql.DefaultTable. Neither
// the file nor the table needs to exist yet: a missing one is an empty
// log. The reader has a connection of its own, which never writes, so it
// works while a device is storing and with the application stopped.
func NewReader(path, table string) (*qsql.Reader, error) {
	return qsql.NewReader(func() (*dbh.Dbh, error) {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Nothing there yet. Opening it would create it.
				return nil, nil
			}
			return nil, err
		}
		return connect(path, "busy_timeout = 5000", "query_only = 1")
	}, table)
}

// Table names one stretch of a log kept in SQLite: a table, in a
// database file. An empty Name means qsql.DefaultTable.
type Table struct {
	Path string
	Name string
}

// NewHistory returns one provider over the tables given, oldest first,
// in one database or in many. None of them needs to exist yet.
func NewHistory(oldestFirst ...Table) (*qsql.History, error) {
	readers := make([]*qsql.Reader, 0, len(oldestFirst))
	for _, t := range oldestFirst {
		r, err := NewReader(t.Path, t.Name)
		if err != nil {
			return nil, err
		}
		readers = append(readers, r)
	}
	return qsql.NewHistory(readers...), nil
}
