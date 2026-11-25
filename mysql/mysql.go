// Package qmysql is the MySQL store of quevu: it holds the driver and
// knows how to reach a MySQL server, and leaves everything else to the
// common piece (github.com/pablo-botella/quevu-sql).
package qmysql

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pablo-botella/dbh"
	qsql "github.com/pablo-botella/quevu-sql"
)

// Config says which server, which database and as whom. All of it comes
// from the configuration of the deployment: nothing here has a value
// taken from code, and no example or test carries a real one.
type Config struct {
	// Host and Port of the server. Port zero means 3306.
	Host string
	Port int
	// Socket is the path of a Unix socket, instead of Host and Port.
	Socket string
	// User and Password.
	User     string
	Password string
	// Database is the schema the tables of the logs live in.
	Database string
	// Table is the table of the log. Default qsql.DefaultTable.
	Table string
	// TLS names how the connection is protected, in the words of the
	// driver: "true", "skip-verify", "preferred", or the name of a
	// configuration registered with the driver. Empty: no TLS.
	TLS string
	// Timeout bounds the time to connect. Zero: the driver's default.
	Timeout time.Duration
	// Params are further connection parameters, passed to the driver as
	// they are.
	Params map[string]string
}

// driverConfig turns a Config into the driver's own.
func driverConfig(cfg Config) (*mysql.Config, error) {
	if cfg.Database == "" {
		return nil, errors.New("qmysql: no database")
	}
	if _, err := qsql.TableName(cfg.Table); err != nil {
		return nil, err
	}
	mc := mysql.NewConfig()
	mc.User, mc.Passwd, mc.DBName = cfg.User, cfg.Password, cfg.Database
	mc.TLSConfig, mc.Timeout = cfg.TLS, cfg.Timeout
	switch {
	case cfg.Socket != "":
		mc.Net, mc.Addr = "unix", cfg.Socket
	case cfg.Host != "":
		port := cfg.Port
		if port == 0 {
			port = 3306
		}
		mc.Net, mc.Addr = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	default:
		return nil, errors.New("qmysql: no host and no socket")
	}
	if len(cfg.Params) > 0 {
		mc.Params = make(map[string]string, len(cfg.Params))
		for k, v := range cfg.Params {
			mc.Params[k] = v
		}
	}
	return mc, nil
}

// connect prepares a handle on the server. No connection is made until
// it is first used.
func connect(cfg Config) (*dbh.Dbh, error) {
	mc, err := driverConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("qmysql: %w", err)
	}
	return dbh.New(sql.OpenDB(c), dbh.MySQL), nil
}

// Options tunes a device: see qsql.Options.
type Options struct {
	Buffer     int
	FlushEvery time.Duration
	Format     *qsql.Format
}

// Open connects to the server, creates the table if needed, and returns
// the writer that stores a log in it.
func Open(cfg Config, opt Options) (*qsql.Device, error) {
	db, err := connect(cfg)
	if err != nil {
		return nil, err
	}
	d, err := qsql.NewDevice(db, qsql.Options{Table: cfg.Table, Buffer: opt.Buffer, FlushEvery: opt.FlushEvery, Format: opt.Format})
	if err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// NewReader returns a stored-data provider over the table the Config
// names. It has a connection of its own, so reading never competes with
// a device that is storing. The table does not need to exist yet: a
// missing one is an empty log.
func NewReader(cfg Config) (*qsql.Reader, error) {
	if _, err := driverConfig(cfg); err != nil {
		return nil, err
	}
	return qsql.NewReader(func() (*dbh.Dbh, error) { return connect(cfg) }, cfg.Table)
}

// NewHistory returns one provider over the tables the Configs name,
// oldest first, on one server or on many.
func NewHistory(oldestFirst ...Config) (*qsql.History, error) {
	readers := make([]*qsql.Reader, 0, len(oldestFirst))
	for _, cfg := range oldestFirst {
		r, err := NewReader(cfg)
		if err != nil {
			return nil, err
		}
		readers = append(readers, r)
	}
	return qsql.NewHistory(readers...), nil
}
