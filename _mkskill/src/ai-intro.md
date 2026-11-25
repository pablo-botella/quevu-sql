---
mkskill:
  pos: 5
  in: ai*
---

# quevu-sql - agent notes

Go module `github.com/pablo-botella/quevu-sql`, one `go.mod`, several
packages:

- `qsql` (the root): `Device` (a `qkernel.Writer` and `qkernel.SeqWriter`),
  `Reader` (a `qkernel.Provider`), `History`, `Meta`, `TableName`,
  `Format` (columns of its own for each log, in `format.go`). Built on
  `github.com/pablo-botella/dbh`. **It must import no driver and open no
  connection.**
- `sqlite` (package `qsqlite`): the driver (`modernc.org/sqlite`) and how
  a SQLite file is opened; `rotate.go`: a database file per period
  (`Rotation`, `OpenRotating`, `NewCurrent`, `NewPeriods`).
- `mysql` (package `qmysql`): the driver (`github.com/go-sql-driver/mysql`)
  and how a server is reached.

dbh is Pablo's library and is not part of this project: use it, never
modify it. The table is described once in `qsql.Meta` and its DDL comes
from `dbh.TableMeta.BuildCreateTable`; do not write `CREATE TABLE` by hand
here.

Every database package runs the same test, `internal/storetest`: a new
database package must pass it too, and a new rule of the common piece goes
there, not in one database's own tests.
