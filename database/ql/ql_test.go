package ql

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	dt "github.com/golang-migrate/migrate/v4/database/testing"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "modernc.org/ql/driver"
)

func Test(t *testing.T) {
	dir := t.TempDir()
	t.Logf("DB path : %s\n", filepath.Join(dir, "ql.db"))

	db, err := sql.Open("ql", filepath.Join(dir, "ql.db"))
	if err != nil {
		return
	}
	defer func() {
		if err := db.Close(); err != nil {
			return
		}
	}()
	d, err := WithInstance(db, &Config{DropAuthorization: database.DropAuthorization{Authorized: true}})
	if err != nil {
		t.Fatal(err)
	}
	dt.Test(t, d, []byte("CREATE TABLE t (Qty int, Name string);"))
}

func TestMigrate(t *testing.T) {
	dir := t.TempDir()
	t.Logf("DB path : %s\n", filepath.Join(dir, "ql.db"))

	db, err := sql.Open("ql", filepath.Join(dir, "ql.db"))
	if err != nil {
		return
	}
	defer func() {
		if err := db.Close(); err != nil {
			return
		}
	}()

	driver, err := WithInstance(db, &Config{DropAuthorization: database.DropAuthorization{Authorized: true}})
	if err != nil {
		t.Fatal(err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		"file://./examples/migrations",
		"ql", driver)
	if err != nil {
		t.Fatal(err)
	}
	dt.TestMigrate(t, m)
}
