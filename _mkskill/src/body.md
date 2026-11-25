---
mkskill:
  pos: 100
  in: "*"
---

## How it is laid out

There is a common point, which is what is done with
[dbh](https://github.com/pablo-botella/dbh), and a component per database
on top of it.

**The common piece (`qsql`)** has everything the stores share: the table,
how a batch is written, how chunks are read by position, the provider and
the aggregator. A database reaches it as a `*dbh.Dbh`, already opened and
carrying its dialect. The table is described once (`qsql.Meta`) and dbh
writes the statement that creates it in each dialect.

**Each database package** holds its driver and knows how its database
should be opened: the settings SQLite wants, the address and credentials
of a MySQL server. It hands the common piece a handle and gets back the
same two things.

## The two halves

As everywhere in quevu, they do not know each other at run time:

- **`Device`**, a persistent writer of a hub. Every batch is one
  transaction, a few hundred rows per statement. It gives nothing back.
- **`Reader`**, a stored-data provider, with a connection of its own, so
  reading never competes with writing and works with the application
  stopped. Which role it plays for a console, immediate or historical, is
  said where it is configured (`qkernel.Roles`), not here: the same reader
  class serves both.

A log lives in one table whose primary key is the position:

| Column | Holds |
|---|---|
| `seq` | The position of the chunk in its log |
| `time` | When it entered the log, nanoseconds since the Unix epoch |
| `data` | The data |

Several logs share a database, each in a table of its own. Table names are
plain identifiers.

## Columns of its own for each log: `Format`

Every log is its own: what is useful in one is noise in another. A
`Format` says, for one log, how a line is cut into fields and what each
becomes in the table: a column with a type, next to the three every table
has.

```go
store, _ := qsqlite.Open("app.db", qsqlite.Options{
	Table: "payments",
	Format: &qsql.Format{
		Separator: "|",
		Fields: []qsql.Field{
			{Name: "at", Type: qsql.Time, Layout: "2006-01-02 15:04:05"},
			{Name: "level", Size: 8, Index: true},
			{},                                   // a piece nobody wants
			{Name: "customer", Type: qsql.Int, Index: true},
			{Name: "amount", Type: qsql.Decimal, Scale: 2},
			{Name: "paid", Type: qsql.Bool},
			{Name: "message", Rest: true},        // keeps what is left
		},
	},
})
```

| Type | Reads | Stored as |
|---|---|---|
| `Text` (the default) | the piece as it is; with `Size`, cut to that many characters | text, or `VARCHAR(Size)` |
| `Int` | a whole number | 64-bit integer |
| `Decimal` | a number with at most `Scale` decimals | 64-bit integer of the smallest unit: `12.50` with scale 2 is `1250` |
| `Bool` | `1/0`, `true/false`, `t/f`, `yes/no`, `y/n`, `on/off` | `1` or `0` |
| `Time` | `Layout`: a layout of the `time` package, or `unix`, `unixmilli`, `unixmicro`, `unixnano`; RFC 3339 when empty | nanoseconds since the Unix epoch, like the `time` column |

The rules:

- **The whole line is always stored**, untouched, in `data`. The columns
  are an addition to query by, never a replacement.
- **A line the format does not fit loses nothing**: it is stored, and the
  columns it could not fill are `NULL`. `Device.Misfits()` counts them.
- **Nothing is rounded or guessed.** `1.005` is not a decimal of scale 2;
  `4.0` is not a whole number; a date that does not fit is not a date.
- **Terminal colors are taken out** before cutting, and every piece loses
  the blanks around it. An empty piece is no value.
- **A time written without a zone** is taken to be in `Format.Location`,
  the local zone of the machine when nil.
- **Column names are plain identifiers** and cannot be `seq`, `time`,
  `data` nor a word of SQL (`order`, `user`, `key`...): `Check` says so.
- **An indexed text field needs a `Size`.**
- **A table is never altered from here.** A format over a table that
  lacks its columns is refused with an error: what to do with what the
  table already holds is for its owner to decide.
- There is no floating-point type: dbh has none. Use `Decimal`.

`Format.Values(line)` cuts and converts one line without any database,
to try a format out.
## Speed against safety

A database is slow when every row is committed on its own. `Buffer` holds
chunks back until that many are waiting and writes them together;
`FlushEvery` bounds how long one may wait. What sits in the buffer is lost
if the process dies. How much to risk for how much speed is chosen by
whoever configures the device.

## History: one provider over many tables

```go
h, _ := qsqlite.NewHistory(
	qsqlite.Table{Path: "archive.db", Name: "y2024"},
	qsqlite.Table{Path: "archive.db", Name: "y2025"},
	qsqlite.Table{Path: "live.db"}, // oldest first
)
```

Several tables, in one database or in many, seen as one provider: the
shape a history takes once it has been archived. Whoever reads it never
learns how many places the chunks come from. `qmysql.NewHistory` takes one
`Config` per table.

## SQLite (`qsqlite`)

`Open(path, Options)` and `NewReader(path, table)`. The driver is pure Go:
no C compiler is needed. Without `Sync` the database runs in WAL mode with
`synchronous=NORMAL` (a commit survives the death of the process; a power
cut may lose the last ones without damaging the file); with it, every
commit waits for the disk. A file or a table that does not exist yet is an
empty log.

### The same store, with SQLite in C (`qsqlitec`)

```go
import qsqlitec "github.com/pablo-botella/quevu-sql/sqlitec"

store, _ := qsqlitec.Open("app.db", qsqlitec.Options{Table: "mailer"})
```

`qsqlitec` is `qsqlite` with another driver: the SQLite library itself,
in C, instead of its translation to Go. Everything else is the same, name
by name, and the two read and write the same files, so changing from one
to the other is changing the import.

| | `qsqlite` | `qsqlitec` |
|---|---|---|
| Driver | `modernc.org/sqlite`, in Go | `github.com/mattn/go-sqlite3`, in C |
| To build | Nothing but Go | cgo and a C compiler |
| Cross compilation | As any Go program | Needs a C compiler for the target |
| Size it adds to a program, without debug information | About 5 MB | About 3 MB |

Take `qsqlitec` when the program is built with cgo anyway, and `qsqlite`
when it must build with Go alone. Without cgo `qsqlitec` does not build,
instead of building a program that cannot open a database.

### A file per period

```go
rot := qsqlite.Rotation{Dir: `D:\logs`, Base: "app-log"} // app-log-2025-03.db, a month each
store, _ := qsqlite.OpenRotating(rot, qsqlite.Options{Table: "mailer"})
recent, _ := qsqlite.NewCurrent(rot, "mailer") // the period in course
whole, _ := qsqlite.NewPeriods(rot, "mailer")  // every file, as one log

src := qkernel.Join(screen, qkernel.Roles{Immediate: recent, Historical: whole})
```

A `Rotation` names a log kept in one database file per period: old
periods are whole files that can be moved, archived or deleted without
touching the live one. `Layout` writes the period and so decides its
length: `2006-01` a month (the default), `2006-01-02` a day, `2006` a
year. Each file holds the tables of the logs stored with that rotation,
one table per log.

- **Each chunk goes to the file of its period**, by the time it carries.
  When a period ends, the next chunk opens the next file by itself; a
  batch that crosses the end is split there.
- **Positions go on from one file to the next.** When the file of the
  period in course is new, the last position is looked for in the files
  before it.
- **`Current`** reads the period in course and follows the calendar.
  **`Periods`** reads every file as one log, and notices files that
  appear or go away.
- **A provider keeps open the files it has read.** On Windows an open
  file cannot be moved or deleted: close the provider first (it opens
  again when asked).
## MySQL (`qmysql`)

`Open(Config, Options)` and `NewReader(Config)`. `Config` says which
server, which database and as whom: host and port or a socket, user,
password, database, table, TLS, further parameters. **All of it comes from
the configuration of the deployment. Never write a real password in source
code, in an example or in a test.**

Tested against MySQL 8.0. The test runs only when told where a server
is, through `QUEVU_MYSQL_HOST`, `QUEVU_MYSQL_PORT`, `QUEVU_MYSQL_USER`,
`QUEVU_MYSQL_PASSWORD` and `QUEVU_MYSQL_DATABASE`; it creates a table with
a name of its own and drops it.

## Rules and gotchas

- **The data column is declared as text for now**, because dbh has no
  binary column type yet. SQLite stores bytes in it untouched. MySQL does
  not: until the column becomes dbh's binary type, **data that is not
  valid UTF-8 text will not survive in MySQL**, and a chunk tops out at
  64 KB there. The change is one line in `qsql.Meta`.
- **A repeated position is an error**, and the batch that carried it is
  rolled back whole.
- **With a buffer, an error from `Write` may belong to chunks accepted by
  an earlier call**: the ones a timed flush failed to store.
- **A batch that cannot be stored is dropped**, not retried: it must not
  hold back everything behind it. The hub counts it as failed.
- **A device and a reader each own their handle** and close it. Close the
  reader: it holds a connection once it has been used.
- **Searching reads the table** page by page within the budget of a step;
  there is no full-text index. The reader is the place for one.
- **Parquet is not a live destination**; it is a fine format to archive
  closed history into, and can be built from these tables.
