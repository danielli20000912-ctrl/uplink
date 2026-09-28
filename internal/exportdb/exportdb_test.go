package exportdb

import (
	"path/filepath"
	"testing"
)

func TestReplaceCatalogsKeepsFailedProbe(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "export.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	write := func(cats map[[2]string]string, refreshed, configured []string) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := ReplaceCatalogs(tx, "t", cats, refreshed, configured); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	write(map[[2]string]string{{"a", "a"}: "A1", {"b", "b"}: "B1", {"c", "c"}: "C1"}, []string{"a", "b", "c"}, []string{"a", "b", "c"})
	// Probe b fails this cycle; group c is no longer configured.
	write(map[[2]string]string{{"a", "a"}: "A2"}, []string{"a"}, []string{"a", "b"})
	for _, c := range []struct{ g, want string }{{"a", "A2"}, {"b", "B1"}, {"c", ""}} {
		js, _, ok, err := LookupCatalog(db, c.g, c.g)
		if err != nil {
			t.Fatal(err)
		}
		if (c.want == "") == ok || js != c.want {
			t.Errorf("group %s: got %q ok=%v, want %q", c.g, js, ok, c.want)
		}
	}
}

func TestSchemaVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.db")
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path, true); err == nil {
		t.Error("old schema version accepted")
	}
}
