// Package backup implements backup import/export.
// Ported from astrbot/core/backup/
package backup

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	zipFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

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

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		writer, err := zw.Create(relPath)
		if err != nil {
			return err
		}

		_, err = io.Copy(writer, file)
		return err
	})
	if err != nil {
		return fmt.Errorf("walk data dir: %w", err)
	}

	logger.Info("Backup exported to %s", destPath)
	return nil
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
		// Prevent zip slip (path traversal)
		if strings.Contains(file.Name, "..") {
			logger.Warn("Skipping suspicious path in backup: %s", file.Name)
			continue
		}

		destPath := filepath.Join(i.dataDir, file.Name)
		if !strings.HasPrefix(destPath, i.dataDir) {
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

		dstFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			return err
		}

		srcFile, err := file.Open()
		if err != nil {
			dstFile.Close()
			return err
		}

		_, err = io.Copy(dstFile, srcFile)
		dstFile.Close()
		srcFile.Close()
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
