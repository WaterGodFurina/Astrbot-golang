// Package backup implements backup import/export.
// Ported from astrbot/core/backup/
package backup

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Backup")

// Exporter creates backup archives.
type Exporter struct {
	dataDir string
}

// NewExporter creates an exporter for the given data directory.
func NewExporter(dataDir string) *Exporter {
	return &Exporter{dataDir: dataDir}
}

// Export creates a zip archive of the data directory.
func (e *Exporter) Export(destPath string) error {
	zipFile, err := os.Create(destPath) // #nosec G304 -- destPath is the caller-chosen export destination
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

	// Snapshot the live SQLite database into a consistent file before walking,
	// so a WAL-mode database is never copied while being written to. The raw
	// astrbot.db/-wal/-shm files are skipped and the snapshot is zipped instead.
	snapshot, err := e.snapshotDB()
	if err != nil {
		return err
	}
	if snapshot != "" {
		defer os.Remove(snapshot)
	}

	err = filepath.Walk(e.dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(e.dataDir, path)
		if err != nil {
			return err
		}

		// Skip temporary and lock files
		if strings.HasSuffix(relPath, ".lock") || strings.HasPrefix(relPath, "tmp/") {
			return nil
		}
		// Skip the live database and its WAL sidecar files; the snapshot above
		// is zipped in their place (avoids a torn copy of a live WAL database).
		if strings.HasSuffix(relPath, ".db-wal") || strings.HasSuffix(relPath, ".db-shm") ||
			(relPath == "astrbot.db" && snapshot != "") {
			return nil
		}

		file, err := os.Open(path) // #nosec G304 -- path originates from filepath.Walk of the local dataDir
		if err != nil {
			return err
		}

		writer, err := zw.Create(relPath)
		if err != nil {
			_ = file.Close()
			return err
		}

		_, err = io.Copy(writer, file)
		_ = file.Close()
		return err
	})
	if err != nil {
		return fmt.Errorf("walk data dir: %w", err)
	}

	// Zip the consistent DB snapshot (only present when a live database was
	// found under the data dir).
	if snapshot != "" {
		snap, err := os.Open(snapshot) // #nosec G304 -- snapshot is a temp file created via os.CreateTemp in snapshotDB
		if err != nil {
			return err
		}
		writer, err := zw.Create("astrbot.db")
		if err != nil {
			_ = snap.Close()
			return err
		}
		_, err = io.Copy(writer, snap)
		_ = snap.Close()
		if err != nil {
			return err
		}
	}

	logger.Info("Backup exported to %s", destPath)
	return nil
}

// snapshotDB produces a consistent copy of dataDir/astrbot.db via
// `VACUUM INTO`, which reads the live WAL database and writes a self-contained
// snapshot file without requiring exclusive access. It returns "" when no
// database exists at the expected location.
func (e *Exporter) snapshotDB() (string, error) {
	dbPath := filepath.Join(e.dataDir, "astrbot.db")
	if _, err := os.Stat(dbPath); err != nil {
		return "", nil
	}
	tmp, err := os.CreateTemp("", "astrbot_db_snapshot_*.db")
	if err != nil {
		return "", fmt.Errorf("create db snapshot temp: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(30000)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("open db for snapshot: %w", err)
	}
	defer conn.Close()

	sql := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(tmpName, "'", "''"))
	if _, err := conn.Exec(sql); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("snapshot db: %w", err)
	}
	return tmpName, nil
}

// Importer restores from backup archives.
type Importer struct {
	dataDir string
}

// NewImporter creates an importer for the given data directory.
func NewImporter(dataDir string) *Importer {
	return &Importer{dataDir: dataDir}
}

// Import extracts a backup archive to the data directory.
func (i *Importer) Import(srcPath string) error {
	reader, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		// Prevent zip slip (path traversal): reject absolute paths and any
		// entry that escapes the data dir once joined.
		rel := filepath.Clean(file.Name)
		if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			logger.Warn("Skipping suspicious path in backup: %s", file.Name)
			continue
		}

		destPath := filepath.Join(i.dataDir, rel)
		if destPath != i.dataDir && !strings.HasPrefix(destPath, i.dataDir+string(filepath.Separator)) {
			logger.Warn("Skipping path outside data dir: %s", file.Name)
			continue
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		dstFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode()) // #nosec G304 -- destPath validated against traversal above
		if err != nil {
			return err
		}

		srcFile, err := file.Open()
		if err != nil {
			_ = dstFile.Close()
			return err
		}

		_, err = io.Copy(dstFile, srcFile)
		_ = dstFile.Close()
		_ = srcFile.Close()
		if err != nil {
			return err
		}
	}

	logger.Info("Backup imported from %s", srcPath)
	return nil
}

// DefaultBackupName generates a backup filename.
func DefaultBackupName() string {
	return fmt.Sprintf("astrbot_backup_%s.zip", time.Now().Format("20060102_150405"))
}
