package qmysql

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pablo-botella/quevu/qkernel"
)

func TestConfigBecomesWhatTheDriverNeeds(t *testing.T) {
	mc, err := driverConfig(Config{
		Host: "db.example.com", User: "example-user", Password: "example-password",
		Database: "logs", Table: "web", TLS: "true", Timeout: 5 * time.Second,
		Params: map[string]string{"charset": "utf8mb4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mc.Net != "tcp" || mc.Addr != "db.example.com:3306" || mc.DBName != "logs" || mc.TLSConfig != "true" || mc.Params["charset"] != "utf8mb4" {
		t.Fatalf("%+v", mc)
	}
	// Whatever the password holds, it travels as a value, not as text
	// pasted into an address.
	mc, _ = driverConfig(Config{Host: "h", Port: 3307, User: "u", Password: "p@ss/w:rd?x", Database: "d"})
	if mc.Addr != "h:3307" || mc.Passwd != "p@ss/w:rd?x" {
		t.Fatalf("%+v", mc)
	}
	mc, _ = driverConfig(Config{Socket: "/run/mysqld/mysqld.sock", Database: "d"})
	if mc.Net != "unix" || mc.Addr != "/run/mysqld/mysqld.sock" {
		t.Fatalf("%+v", mc)
	}
}

func TestConfigIsChecked(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no database": {Host: "h"},
		"no server":   {Database: "d"},
		"bad table":   {Host: "h", Database: "d", Table: "x; drop"},
	} {
		if _, err := driverConfig(cfg); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if _, err := NewReader(cfg); err == nil {
			t.Fatalf("%s: a reader was made", name)
		}
	}
	// Preparing a provider needs no server: nothing connects until it is
	// first asked.
	r, err := NewReader(Config{Host: "127.0.0.1", Port: 1, Database: "d"})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := NewHistory(Config{Host: "h", Database: "a"}, Config{Host: "h", Database: "b", Table: "y2025"}); err != nil {
		t.Fatal(err)
	}
}

// The test against a real server runs only when it is told where one is.
// Nothing about it is written here: set
//
//	QUEVU_MYSQL_HOST, QUEVU_MYSQL_PORT, QUEVU_MYSQL_USER,
//	QUEVU_MYSQL_PASSWORD, QUEVU_MYSQL_DATABASE
//
// It creates a table with a name of its own in that database and drops
// it when it is done.
func TestRealServer(t *testing.T) {
	host := os.Getenv("QUEVU_MYSQL_HOST")
	if host == "" {
		t.Skip("set QUEVU_MYSQL_HOST and friends to run against a real server")
	}
	port, _ := strconv.Atoi(os.Getenv("QUEVU_MYSQL_PORT"))
	cfg := Config{
		Host: host, Port: port,
		User: os.Getenv("QUEVU_MYSQL_USER"), Password: os.Getenv("QUEVU_MYSQL_PASSWORD"),
		Database: os.Getenv("QUEVU_MYSQL_DATABASE"),
		Table:    fmt.Sprintf("quevu_test_%d", time.Now().UnixNano()),
		Timeout:  10 * time.Second,
	}
	d, err := Open(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	drop, err := connect(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		drop.Exec("DROP TABLE " + cfg.Table)
		drop.Close()
	}()

	var in []qkernel.Chunk
	for s := uint64(1); s <= 1200; s++ {
		in = append(in, qkernel.Chunk{Seq: s, Time: time.Unix(1700000000, int64(s)), Data: fmt.Appendf(nil, "line %d", s)})
	}
	in[4].Data = []byte("áñ \U0001f600 accents and an emoji")
	in[5].Data = nil
	if err := d.Write(in); err != nil {
		t.Fatal(err)
	}
	// A repeated position is refused, whole.
	if err := d.Write(in[10:12]); err == nil || !strings.Contains(err.Error(), "already stored") {
		t.Fatalf("a repeated position: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tail, err := r.Tail(3)
	if err != nil || len(tail) != 3 || tail[2].Seq != 1200 {
		t.Fatalf("tail: %v %v", tail, err)
	}
	before, _ := r.Before(600, 2)
	after, _ := r.After(600, 2)
	if len(before) != 2 || before[1].Seq != 599 || len(after) != 2 || after[0].Seq != 601 {
		t.Fatalf("before %v after %v", before, after)
	}
	got, _ := r.After(4, 2)
	if string(got[0].Data) != string(in[4].Data) || len(got[1].Data) != 0 || !got[0].Time.Equal(in[4].Time) {
		t.Fatalf("data: %q / %q at %v", got[0].Data, got[1].Data, got[0].Time)
	}
	// Positions go on where the table left them.
	d, err = Open(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if last, _ := d.LastSeq(); last != 1200 {
		t.Fatalf("last %d", last)
	}
}
