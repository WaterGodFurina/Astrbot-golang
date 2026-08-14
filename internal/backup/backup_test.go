package backup

import (
	"archive/zip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestExportConsistentDBSnapshot verifies the exporter snapshots the live
// SQLite database (consistent copy) instead of copying the raw WAL database,
// and skips the -wal/-shm sidecar files.
func TestExportConsistentDBSnapshot(t *testing.T) {
	dataDir := t.TempDir()

	// A real SQLite database with a table + row.
	dbPath := filepath.Join(dataDir, "astrbot.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO t (v) VALUES ('hello')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	// Stray WAL sidecar files must be skipped (they are not a consistent copy).
	if err := os.WriteFile(filepath.Join(dataDir, "astrbot.db-wal"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "astrbot.db-shm"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "notes.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	e := NewExporter(dataDir)
	dest := filepath.Join(t.TempDir(), "backup.zip")
	if err := e.Export(dest); err != nil {
		t.Fatalf("export: %v", err)
	}

	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = f
	}
	if names["astrbot.db"] == nil {
		t.Error("zip missing astrbot.db snapshot")
	}
	if names["astrbot.db-wal"] != nil || names["astrbot.db-shm"] != nil {
		t.Error("zip must not contain WAL sidecar files")
	}
	if names["notes.txt"] == nil {
		t.Error("zip missing regular file")
	}

	// The snapshot entry must be a readable, consistent database.
	if names["astrbot.db"] == nil {
		t.Fatal("abort: no snapshot entry")
	}
	src, err := names["astrbot.db"].Open()
	if err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(t.TempDir(), "astrbot.db")
	out, err := os.Create(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	out.Close()
	src.Close()

	snapDB, err := sql.Open("sqlite", snapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snapDB.Close()
	var v string
	if err := snapDB.QueryRow("SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("snapshot database is not readable: %v", err)
	}
	if v != "hello" {
		t.Errorf("snapshot row = %q, want %q", v, "hello")
	}
}
