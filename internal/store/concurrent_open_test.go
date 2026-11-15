package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Two processes can open a new database at the same moment (the panel and an agent's
// `maestria post`). Every open must succeed and each migration must run exactly once.
func TestConcurrentOpensOfAFreshDatabaseApplyEachMigrationOnce(t *testing.T) {
	// CREATE TABLE without IF NOT EXISTS fails if a migration runs twice, and the second one
	// leaves a mark that shows how many times it ran.
	migs := []string{
		"CREATE TABLE runs (n INTEGER NOT NULL)",
		"INSERT INTO runs (n) VALUES (2); CREATE TABLE extra (id INTEGER PRIMARY KEY)",
	}
	const rounds, workers = 5, 16
	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), "race.db")
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, workers)
		dbs := make([]*sql.DB, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				dbs[i], errs[i] = OpenWith(path, migs)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: open %d: %v", round, i, err)
			}
		}
		var version, runs int
		if err := dbs[0].QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migs) {
			t.Fatalf("round %d: version %d err %v", round, version, err)
		}
		if err := dbs[0].QueryRow("SELECT COUNT(*) FROM runs").Scan(&runs); err != nil || runs != 1 {
			t.Fatalf("round %d: migration 2 ran %d times (err %v)", round, runs, err)
		}
		for _, db := range dbs {
			db.Close()
		}
	}
}

// The real schema, opened the same way.
func TestConcurrentOpensApplyTheRealSchema(t *testing.T) {
	dir := t.TempDir()
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := Open(dir)
			if err == nil {
				err = db.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion() {
		t.Fatalf("version %d err %v", version, err)
	}
}

// A migration that fails leaves nothing behind, even when earlier ones in the batch succeeded.
func TestFailedMigrationRollsBackTheWholeBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.db")
	_, err := OpenWith(path, []string{"CREATE TABLE a (x INTEGER)", "CREATE TABLE b (x INTEGER); NOT SQL"})
	if err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("want a migration 2 error, got %v", err)
	}
	db, err := OpenWith(path, []string{"CREATE TABLE a (x INTEGER)"})
	if err != nil {
		t.Fatalf("a retry should start from an empty database: %v", err)
	}
	db.Close()
}
