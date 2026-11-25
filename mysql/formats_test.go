package qmysql

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	qsql "github.com/pablo-botella/quevu-sql"
	"github.com/pablo-botella/quevu-sql/internal/storetest"
)

// Runs only when told where a server is: see TestRealServer.
func TestFormatsOnARealServer(t *testing.T) {
	host := os.Getenv("QUEVU_MYSQL_HOST")
	if host == "" {
		t.Skip("set QUEVU_MYSQL_HOST and friends to run against a real server")
	}
	port, _ := strconv.Atoi(os.Getenv("QUEVU_MYSQL_PORT"))
	cfg := Config{
		Host: host, Port: port,
		User: os.Getenv("QUEVU_MYSQL_USER"), Password: os.Getenv("QUEVU_MYSQL_PASSWORD"),
		Database: os.Getenv("QUEVU_MYSQL_DATABASE"), Timeout: 10 * time.Second,
	}
	look, err := connect(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer look.Close()
	at := func(table string) Config { c := cfg; c.Table = table; return c }
	storetest.Run(t, storetest.Store{
		Open: func(table string, f *qsql.Format, buffer int) (*qsql.Device, error) {
			return Open(at(table), Options{Format: f, Buffer: buffer})
		},
		Reader: func(table string) (*qsql.Reader, error) { return NewReader(at(table)) },
		Look:   look,
		Prefix: fmt.Sprintf("quevu_test_%d_", time.Now().UnixNano()),
	})
}
