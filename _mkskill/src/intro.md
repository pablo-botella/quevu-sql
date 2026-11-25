---
mkskill:
  pos: 10
  in: readme
---

# quevu-sql

SQL stores for [quevu](https://github.com/pablo-botella/quevu): a log kept
in a database and given back by position.

One repository, one module, a package per database. Go links only what is
imported, so an application gets only the driver it uses.

| Package | Import path | What it is | State |
|---|---|---|---|
| `qsql` | `github.com/pablo-botella/quevu-sql` | What every store has in common. No driver | Tested |
| `qsqlite` | `.../quevu-sql/sqlite` | SQLite, with a driver written in Go: no C compiler needed | Tested |
| `qsqlitec` | `.../quevu-sql/sqlitec` | SQLite, with the SQLite library itself, in C: needs cgo | Tested |
| `qmysql` | `.../quevu-sql/mysql` | MySQL | Tested against MySQL 8.0 |
| `qpostgres` | `.../quevu-sql/postgres` | PostgreSQL | Not written yet |

```go
import qsqlite "github.com/pablo-botella/quevu-sql/sqlite"

store, _ := qsqlite.Open("app.db", qsqlite.Options{Table: "mailer"})
mail, _ := qkernel.NewQHub("mailer", qkernel.WithWriter(store), qkernel.WithWriterOptions(screen, qkernel.WriterOptions{OnFull: qkernel.Skip}))

// Storing a log does not mean reading it back: that takes a provider.
reader, _ := qsqlite.NewReader("app.db", "mailer")
src := qkernel.Join(screen, qkernel.Roles{Immediate: reader})
```
