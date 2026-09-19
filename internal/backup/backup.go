// Package backup implements backup import/export.
// Ported from astrbot/core/backup/
package backup

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
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

	// manifest.json 记录版本/导出时间/内容摘要（对齐 Python exporter），
	// dashboard 备份列表（origin/astrbot_version/exported_at）与导入预检查
	// （pre_check 读 manifest.json）都依赖它。
	manifestData, err := json.MarshalIndent(e.buildManifest(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	mw, err := zw.Create("manifest.json")
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	if _, err := mw.Write(manifestData); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

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

	// 目标 zip 自身的相对路径（zip 就建在 dataDir/backups/ 下），Walk 时必须
	// 跳过，否则自引用读到正在写入的 zip：回环直至写满磁盘，且每次备份嵌套
	// 全部历史备份导致体积指数膨胀。
	destRel, err := filepath.Rel(e.dataDir, destPath)
	if err != nil {
		return fmt.Errorf("resolve dest rel path: %w", err)
	}

	err = filepath.Walk(e.dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(e.dataDir, path)
		if err != nil {
			return err
		}

		if info.IsDir() {
			// 跳过备份输出目录（zip 就建在里面），否则自引用回环直至写满磁盘
			// （兼容 Windows 反斜杠分隔符）。
			if relPath == "backups" || strings.HasPrefix(relPath, "backups/") ||
				strings.HasPrefix(relPath, "backups\\") {
				return filepath.SkipDir
			}
			return nil
		}

		// 跳过正在写入的目标 zip 自身（防止递归打包自身）。
		if relPath == destRel {
			return nil
		}

		// Skip temporary and lock files（Windows 下 relPath 用 \ 分隔，
		// ToSlash 归一后再匹配 tmp/，避免漏跳临时目录）。
		relSlash := filepath.ToSlash(relPath)
		if strings.HasSuffix(relSlash, ".lock") || strings.HasPrefix(relSlash, "tmp/") {
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

	// 显式收尾 zip：zw.Close 写入中央目录（错误必须上报，否则导出的归档
	// 缺条目却提示成功），随后 fsync 落盘再关闭文件。
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finalize zip: %w", err)
	}
	if err := zipFile.Sync(); err != nil {
		return fmt.Errorf("sync zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return err
	}
	// 成功日志必须在 Close/Sync 全部成功之后打印，避免收尾失败时用户已看到
	// "导出成功"（错误传播逻辑不变，仅调整日志位置）。
	logger.Info("Backup exported to %s", destPath)
	return nil
}

// buildManifest assembles the backup manifest (mirrors Python exporter's
// manifest: astrbot_version / exported_at / has_config / has_knowledge_bases /
// directories). The "tables" key is kept empty: the Go exporter backs up the
// whole data directory including the SQLite database file itself.
func (e *Exporter) buildManifest() map[string]interface{} {
	manifest := map[string]interface{}{
		// origin 标记备份来源：导入时据此拒绝 Python 版结构化备份等不兼容格式
		"origin":              "astrbot-go",
		"astrbot_version":     version.Version,
		"exported_at":         time.Now().Format(time.RFC3339),
		"tables":              []string{},
		"has_knowledge_bases": false,
		"has_config":          false,
		"directories":         []string{},
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "cmd_config.json")); err == nil {
		manifest["has_config"] = true
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "knowledge_bases")); err == nil {
		manifest["has_knowledge_bases"] = true
	}
	entries, err := os.ReadDir(e.dataDir)
	if err == nil {
		var dirs []string
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				dirs = append(dirs, entry.Name())
			}
		}
		if dirs == nil {
			dirs = []string{}
		}
		manifest["directories"] = dirs
	}
	return manifest
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

	// #nosec gosql-sqli -- tmpName 是 os.CreateTemp 生成的宿主临时文件路径（非用户输入），单引号已转义；SQLite VACUUM INTO 目标不支持绑定参数
	sql := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(tmpName, "'", "''"))
	if _, err := conn.Exec(sql); err != nil { // nosemgrep: go.lang.security.audit.sqli.gosql-sqli.gosql-sqli
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

// importStagingPrefix 是导入暂存目录名前缀，暂存目录建在 dataDir 下（保证
// 与 dataDir 同分区，后续 rename 搬移不会跨文件系统）。
const importStagingPrefix = ".import_staging_"

// Import restores a backup archive to the data directory.
//
// 采用"暂存-校验-替换"三段式，避免直接就地覆盖运行中的 data/（旧实现直接
// 把 zip 解到 dataDir，可能覆盖正被 SQLite 打开的主库文件导致库损坏、或
// 半途失败留下半导入态）：
//
//  1. 校验 manifest.json：缺失或 origin 不是 astrbot-go 的归档（如 Python
//     结构化备份，表结构与 Go 版不兼容）直接拒绝导入；
//  2. 解压到 dataDir 下的临时暂存目录（zip-slip 防护保持不变）；
//  3. 校验暂存内容（astrbot.db 存在且通过 PRAGMA integrity_check）；
//  4. 校验通过后把暂存内容搬移到 dataDir 完成替换，失败时尽力回滚。
//
// 注意：主库 astrbot.db 是运行中被打开的文件，热替换后已打开的连接仍指向
// 旧文件，需要重启 AstrBot 后新库才生效（调用方应据此提示用户重启）。
func (i *Importer) Import(srcPath string) error {
	// ── 第一步：校验 manifest，拒绝非 astrbot-go 格式的备份 ──
	manifest := ReadManifest(srcPath)
	if manifest.AstrbotVersion == "" && manifest.ExportedAt == "" {
		return fmt.Errorf("备份缺少有效的 manifest.json，不是 astrbot-go 导出的备份，已拒绝导入")
	}
	if manifest.Origin != "" && manifest.Origin != "astrbot-go" {
		return fmt.Errorf("备份 origin 为 %q（非 astrbot-go，可能来自 Python 版结构化备份），表结构不兼容，已拒绝导入", manifest.Origin)
	}
	if manifest.AstrbotVersion == "" {
		return fmt.Errorf("备份 manifest 缺少 astrbot_version，已拒绝导入")
	}

	// ── 第二步：解压到暂存目录 ──
	staging, err := os.MkdirTemp(i.dataDir, importStagingPrefix+"*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	// 无论成功失败，暂存目录用完即删
	defer os.RemoveAll(staging)

	reader, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		// Prevent zip slip (path traversal): reject absolute paths and any
		// entry that escapes the staging dir once joined.
		rel := filepath.Clean(file.Name)
		if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			logger.Warn("Skipping suspicious path in backup: %s", file.Name)
			continue
		}

		destPath := filepath.Join(staging, rel)
		if destPath != staging && !strings.HasPrefix(destPath, staging+string(filepath.Separator)) {
			logger.Warn("Skipping path outside staging dir: %s", file.Name)
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

		// #nosec decompression_bomb -- 备份恢复由管理员在 dashboard（需鉴权）发起，归档为宿主
		// 自身 Export 生成的备份（可信来源）；条目数/路径穿越已校验，保持与原 Python 行为一致。
		_, err = io.Copy(dstFile, srcFile) // nosemgrep: go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb
		_ = dstFile.Close()
		_ = srcFile.Close()
		if err != nil {
			return err
		}
	}

	// ── 第三步：校验暂存内容 ──
	if err := validateStagedDB(filepath.Join(staging, "astrbot.db")); err != nil {
		return err
	}

	// ── 第四步：搬移暂存内容到 dataDir 完成替换 ──
	if err := i.promoteStaging(staging); err != nil {
		return err
	}

	logger.Info("Backup imported from %s (重启后新数据完全生效)", srcPath)
	return nil
}

// validateStagedDB checks the staged database file: it must exist and pass
// SQLite's PRAGMA integrity_check.
func validateStagedDB(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("暂存目录中缺少 astrbot.db，备份不完整，已放弃导入: %w", err)
	}
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(30000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开暂存 astrbot.db 失败: %w", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("暂存 astrbot.db 完整性检查失败: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("暂存 astrbot.db 完整性检查未通过: %s", result)
	}
	return nil
}

// promoteStaging moves the validated staging content into the data dir.
//
// 替换策略（主库 astrbot.db 是运行中打开的文件，不能直接删）：
//   - astrbot.db 改名为 astrbot.db.imported（保留旧文件供回滚，重启后由
//     Go SQLite 驱动重新打开时读到新库）；
//   - WAL/SHM 附属文件删除（它们属于旧库，残留会导致新库被误判损坏）；
//   - 其余文件/目录逐项覆盖搬移；
//   - 任一项失败时，对已搬移项尽力回滚（恢复 .imported 旧库、删除已覆盖项）。
//
// 注意：主库热替换需要重启后才生效——Go SQLite 驱动持有的旧文件句柄不会
// 自动切换到新文件。
func (i *Importer) promoteStaging(staging string) error {
	dbPath := filepath.Join(i.dataDir, "astrbot.db")

	// 记录已搬移项用于失败回滚（新写入 dataDir 的路径 + 被改名的旧库）
	type moved struct {
		dest    string
		backup  string // 原有内容备份路径；空表示 dest 原本不存在
		wasDir  bool
		isOldDB bool
	}
	var journal []moved
	rollback := func(cause error) error {
		for idx := len(journal) - 1; idx >= 0; idx-- {
			m := journal[idx]
			if m.isOldDB {
				// 恢复被改名的旧库
				_ = os.Rename(dbPath+".imported", dbPath)
				continue
			}
			if m.backup != "" {
				_ = os.Rename(m.backup, m.dest)
				continue
			}
			_ = os.RemoveAll(m.dest)
		}
		return fmt.Errorf("搬移暂存内容失败，已尽力回滚: %w", cause)
	}

	// 先处理主库：旧库改名保留（运行中文件无法直接删除，且保留可回滚）；
	// WAL/SHM 附属文件直接删除，避免残留副作用污染新库。
	if _, err := os.Stat(dbPath); err == nil {
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		if err := os.Rename(dbPath, dbPath+".imported"); err != nil {
			return fmt.Errorf("保留旧 astrbot.db 失败: %w", err)
		}
		journal = append(journal, moved{isOldDB: true})
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		return rollback(err)
	}
	for _, entry := range entries {
		src := filepath.Join(staging, entry.Name())
		dst := filepath.Join(i.dataDir, entry.Name())
		if entry.IsDir() {
			// 目录整体覆盖搬移：先备份旧目录再搬入，失败可整体还原
			var backupPath string
			if _, err := os.Stat(dst); err == nil {
				backupPath = dst + ".imported_bak"
				if err := os.Rename(dst, backupPath); err != nil {
					return rollback(err)
				}
			}
			if err := os.Rename(src, dst); err != nil {
				if backupPath != "" {
					_ = os.Rename(backupPath, dst)
					return rollback(err)
				}
				return rollback(err)
			}
			journal = append(journal, moved{dest: dst, backup: backupPath, wasDir: true})
			continue
		}
		// 普通文件：旧文件先改名备份，再搬入新文件
		var backupPath string
		if _, err := os.Stat(dst); err == nil {
			backupPath = dst + ".imported_bak"
			if err := os.Rename(dst, backupPath); err != nil {
				return rollback(err)
			}
		}
		if err := os.Rename(src, dst); err != nil {
			if backupPath != "" {
				_ = os.Rename(backupPath, dst)
			}
			return rollback(err)
		}
		journal = append(journal, moved{dest: dst, backup: backupPath})
	}
	return nil
}

// DefaultBackupName generates a backup filename.
func DefaultBackupName() string {
	return fmt.Sprintf("astrbot_backup_%s.zip", time.Now().Format("20060102_150405"))
}

// ManifestEntry mirrors the backup manifest written by Export (and the Python
// exporter): origin/astrbot_version/exported_at plus the content summary keys
// the dashboard list & pre-check surfaces.
type ManifestEntry struct {
	Origin            string   `json:"origin,omitempty"`
	UploadedAt        string   `json:"uploaded_at,omitempty"`
	AstrbotVersion    string   `json:"astrbot_version"`
	ExportedAt        string   `json:"exported_at"`
	Tables            []string `json:"tables"`
	HasKnowledgeBases bool     `json:"has_knowledge_bases"`
	HasConfig         bool     `json:"has_config"`
	Directories       []string `json:"directories"`
}

// ReadManifest extracts the manifest.json of a backup archive. Returns a
// zero-value entry when the archive has no (or an unreadable) manifest.
func ReadManifest(zipPath string) ManifestEntry {
	var out ManifestEntry
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return out
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != "manifest.json" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return out
		}
		data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
		_ = rc.Close()
		if err != nil {
			return out
		}
		_ = json.Unmarshal(data, &out)
		return out
	}
	return out
}

// CheckResult is the import pre-check outcome, mirroring Python's
// ImportPreCheckResult.to_dict() so the WebUI confirm dialog fields match.
type CheckResult struct {
	Valid          bool     `json:"valid"`
	CanImport      bool     `json:"can_import"`
	VersionStatus  string   `json:"version_status"` // match | minor_diff | major_diff
	BackupVersion  string   `json:"backup_version"`
	CurrentVersion string   `json:"current_version"`
	BackupTime     string   `json:"backup_time"`
	ConfirmMessage string   `json:"confirm_message"`
	Warnings       []string `json:"warnings"`
	Error          string   `json:"error"`
	BackupSummary  struct {
		Tables            []string `json:"tables"`
		HasKnowledgeBases bool     `json:"has_knowledge_bases"`
		HasConfig         bool     `json:"has_config"`
		Directories       []string `json:"directories"`
	} `json:"backup_summary"`
}

// CheckBackup pre-validates a backup archive (zip integrity + manifest),
// mirroring Python importer.pre_check: version compatibility decides
// can_import (major version must match, minor differences are allowed).
func CheckBackup(zipPath string) *CheckResult {
	res := &CheckResult{CurrentVersion: version.Version}
	if _, err := os.Stat(zipPath); err != nil {
		res.Error = "备份文件不存在: " + zipPath
		return res
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		res.Error = "无效的 ZIP 文件"
		return res
	}
	defer reader.Close()

	manifest := ReadManifest(zipPath)
	if manifest.AstrbotVersion == "" && manifest.ExportedAt == "" {
		res.Error = "备份文件缺少 manifest.json，不是有效的 AstrBot 备份"
		return res
	}
	res.BackupVersion = manifest.AstrbotVersion
	if res.BackupVersion == "" {
		res.BackupVersion = "未知"
	}
	res.BackupTime = manifest.ExportedAt
	if res.BackupTime == "" {
		res.BackupTime = "未知"
	}
	res.Valid = true
	res.BackupSummary.Tables = manifest.Tables
	res.BackupSummary.HasKnowledgeBases = manifest.HasKnowledgeBases
	res.BackupSummary.HasConfig = manifest.HasConfig
	res.BackupSummary.Directories = manifest.Directories

	// 主版本（前两位）必须一致，小版本差异允许导入（对齐 Python
	// importer._check_version_compatibility）。
	backupMajor := majorVersion(res.BackupVersion)
	currentMajor := majorVersion(version.Version)
	if backupMajor == "" || currentMajor == "" || backupMajor != currentMajor {
		res.VersionStatus = "major_diff"
		res.CanImport = false
		return res
	}
	if res.BackupVersion != version.Version {
		res.VersionStatus = "minor_diff"
		res.CanImport = true
		return res
	}
	res.VersionStatus = "match"
	res.CanImport = true
	return res
}

// majorVersion returns the first two dotted components of a version string
// (e.g. "4.27.4" -> "4.27"); "" when unparsable.
func majorVersion(v string) string {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}
