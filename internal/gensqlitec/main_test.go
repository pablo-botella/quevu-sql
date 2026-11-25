package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The package sqlitec in the repository is what the package sqlite gives
// today: nobody changed one and forgot the other.
func TestSqlitecIsUpToDate(t *testing.T) {
	want, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	have, err := filepath.Glob(filepath.Join(target, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(have) != len(want) {
		t.Errorf("sqlitec holds %d Go files, sqlite gives %d: run go generate ./...", len(have), len(want))
	}
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// A checkout may have changed the line ends.
		if strings.ReplaceAll(string(got), "\r\n", "\n") != string(data) {
			t.Errorf("sqlitec/%s is not what sqlite/%s gives: run go generate ./...", name, name)
		}
	}
}
