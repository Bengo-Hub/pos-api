package customers

import (
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// Pure Go sqlite registered as "sqlite3" for ent, the same shim other packages use.
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

// openTestDB opens a migrated sqlite database in a temp file.
func openTestDB(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", filepath.Join(t.TempDir(), "customers.sqlite"))
	t.Cleanup(func() { _ = client.Close() })
	return client
}
