// Package storage implements on-disk storage cleanup.
// Ported from astrbot/core/utils/storage_cleaner.py (4.29.2): cleans logs,
// cache and trace history under data/, protecting the active log files and the
// trace database (and any SQLite file) from deletion.
package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/tracestore"
)

var logger = log.GetDefault().WithComponent("Storage")

const (
	TargetLogs   = "logs"
	TargetCache  = "cache"
	TargetTraces = "traces"
)

// Cleaner removes disposable files under the data directory.
type Cleaner struct {
	config     map[string]interface{}
	dataDir    string
	tempDir    string
	traceStore *tracestore.TraceStore
}

// NewCleaner creates a storage cleaner. traceStore may be nil when trace
// persistence is disabled.
func NewCleaner(config map[string]interface{}, dataDir, tempDir string, traceStore *tracestore.TraceStore) *Cleaner {
	return &Cleaner{config: config, dataDir: dataDir, tempDir: tempDir, traceStore: traceStore}
}

// GetStatus reports the size of each cleanup target.
func (c *Cleaner) GetStatus() map[string]interface{} {
	logs := c.buildStatus(TargetLogs)
	cache := c.buildStatus(TargetCache)
	traces := c.buildStatus(TargetTraces)
	return map[string]interface{}{
		TargetLogs:    logs,
		TargetCache:   cache,
		TargetTraces:  traces,
		"total_bytes": sizeOf(logs) + sizeOf(cache) + sizeOf(traces),
	}
}

// Cleanup deletes (or truncates) files for the given target ("logs"|"cache"|
// "traces"|"all").
func (c *Cleaner) Cleanup(target string) (map[string]interface{}, error) {
	normalized := strings.ToLower(strings.TrimSpace(target))
	if normalized == "" {
		normalized = "all"
	}
	valid := map[string]bool{TargetLogs: true, TargetCache: true, TargetTraces: true, "all": true}
	if !valid[normalized] {
		return nil, fmt.Errorf("Unsupported cleanup target: %s", target)
	}
	targets := []string{TargetTraces, TargetLogs, TargetCache}
	if normalized != "all" {
		targets = []string{normalized}
	}

	results := map[string]interface{}{}
	aggregate := map[string]int64{
		"removed_bytes": 0, "processed_files": 0, "deleted_files": 0,
		"truncated_files": 0, "failed_files": 0, "removed_traces": 0,
	}
	for _, name := range targets {
		res := c.cleanupTarget(name)
		results[name] = res
		aggregate["removed_bytes"] += int64Of(res["removed_bytes"])
		aggregate["processed_files"] += int64Of(res["processed_files"])
		aggregate["deleted_files"] += int64Of(res["deleted_files"])
		aggregate["truncated_files"] += int64Of(res["truncated_files"])
		aggregate["failed_files"] += int64Of(res["failed_files"])
		aggregate["removed_traces"] += int64Of(res["removed_traces"])
	}
	out := map[string]interface{}{
		"target":  normalized,
		"results": results,
		"status":  c.GetStatus(),
	}
	for k, v := range aggregate {
		out[k] = v
	}
	return out, nil
}

func (c *Cleaner) buildStatus(target string) map[string]interface{} {
	var files []string
	var primary string
	switch target {
	case TargetLogs:
		files = c.collectLogFiles()
		primary = filepath.Join(c.dataDir, "logs")
	case TargetCache:
		files = c.collectCacheFiles()
		primary = c.tempDir
	case TargetTraces:
		primary = c.resolveLogPath(c.optionalStr("trace_db_path"), "logs/traces.db")
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			files = append(files, primary+suffix)
		}
	}
	sizeBytes, fileCount := summarizeFiles(files)
	_, err := os.Stat(primary)
	return map[string]interface{}{
		"size_bytes": sizeBytes,
		"file_count": fileCount,
		"path":       primary,
		"exists":     err == nil,
	}
}

func (c *Cleaner) cleanupTarget(target string) map[string]interface{} {
	if target == TargetTraces {
		before := c.buildStatus(TargetTraces)
		removedTraces := 0
		if int64Of(before["size_bytes"]) > 0 && c.traceStore != nil {
			if n, err := c.traceStore.ClearHistory(); err == nil {
				removedTraces = n
			} else {
				logger.Warn("清理 trace 历史失败: %v", err)
			}
		}
		after := c.buildStatus(TargetTraces)
		removed := int64Of(before["size_bytes"]) - int64Of(after["size_bytes"])
		if removed < 0 {
			removed = 0
		}
		return map[string]interface{}{
			"removed_bytes": removed, "removed_traces": removedTraces,
			"processed_files": 0, "deleted_files": 0, "truncated_files": 0, "failed_files": 0,
		}
	}

	var files []string
	active := map[string]bool{}
	switch target {
	case TargetLogs:
		files = c.collectLogFiles()
		for _, lc := range c.logFileConfigs() {
			if lc.enabled {
				active[lc.path] = true
			}
		}
	case TargetCache:
		files = c.collectCacheFiles()
	}

	var removedBytes, deletedFiles, truncatedFiles, failedFiles int64
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		size := info.Size()
		if active[path] {
			if err := os.Truncate(path, 0); err != nil {
				failedFiles++
				continue
			}
			truncatedFiles++
			removedBytes += size
		} else {
			if err := os.Remove(path); err != nil {
				failedFiles++
				continue
			}
			deletedFiles++
			removedBytes += size
		}
	}

	if target == TargetCache {
		removeEmptyDirs(c.tempDir)
		_ = os.MkdirAll(c.tempDir, 0o755)
	}
	logger.Info("存储清理完成: target=%s removed_bytes=%d deleted=%d truncated=%d failed=%d",
		target, removedBytes, deletedFiles, truncatedFiles, failedFiles)
	return map[string]interface{}{
		"removed_bytes": removedBytes, "removed_traces": 0,
		"processed_files": deletedFiles + truncatedFiles,
		"deleted_files":   deletedFiles, "truncated_files": truncatedFiles, "failed_files": failedFiles,
	}
}

// collectLogFiles returns log files under data/logs plus rotated families of
// the configured log/trace-log paths, excluding SQLite files.
func (c *Cleaner) collectLogFiles() []string {
	set := map[string]bool{}
	for _, p := range iterFiles(filepath.Join(c.dataDir, "logs")) {
		set[p] = true
	}
	for _, lc := range c.logFileConfigs() {
		for _, p := range iterLogFamilyFiles(lc.path) {
			set[p] = true
		}
	}
	tracePath := c.resolveLogPath(c.optionalStr("trace_db_path"), "logs/traces.db")
	protected := map[string]bool{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		protected[absPath(tracePath+suffix)] = true
	}

	var out []string
	for path := range set {
		abs := absPath(path)
		if protected[abs] {
			continue
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".db" || ext == ".sqlite" || ext == ".sqlite3" ||
			strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-journal") {
			continue
		}
		if !isRegularNonSQLite(path) {
			continue
		}
		out = append(out, path)
	}
	return out
}

func (c *Cleaner) collectCacheFiles() []string {
	set := map[string]bool{}
	for _, p := range iterFiles(c.tempDir) {
		set[p] = true
	}
	if matches, err := filepath.Glob(filepath.Join(c.dataDir, "plugins_custom_*.json")); err == nil {
		for _, m := range matches {
			set[m] = true
		}
	}
	for _, extra := range []string{
		filepath.Join(c.dataDir, "plugins.json"),
		filepath.Join(c.dataDir, "sandbox_skills_cache.json"),
	} {
		if info, err := os.Stat(extra); err == nil && !info.IsDir() {
			set[extra] = true
		}
	}
	tracePath := c.resolveLogPath(c.optionalStr("trace_db_path"), "logs/traces.db")
	protected := map[string]bool{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		protected[absPath(tracePath+suffix)] = true
	}
	var out []string
	for path := range set {
		if protected[absPath(path)] {
			continue
		}
		out = append(out, path)
	}
	return out
}

type logFileConfig struct {
	path    string
	enabled bool
}

func (c *Cleaner) logFileConfigs() []logFileConfig {
	return []logFileConfig{
		{path: c.resolveLogPath(c.optionalStr("log_file_path"), "logs/astrbot.log"), enabled: c.boolOpt("log_file_enable")},
		{path: c.resolveLogPath(c.optionalStr("trace_log_path"), "logs/astrbot.trace.log"), enabled: c.boolOpt("trace_log_enable")},
	}
}

func (c *Cleaner) optionalStr(key string) string {
	if v, ok := c.config[key].(string); ok {
		return v
	}
	return ""
}

func (c *Cleaner) boolOpt(key string) bool {
	v, _ := c.config[key].(bool)
	return v
}

func (c *Cleaner) resolveLogPath(configured, defaultRel string) string {
	value := configured
	if value == "" {
		value = defaultRel
	}
	if filepath.IsAbs(value) {
		return absPath(value)
	}
	return absPath(filepath.Join(c.dataDir, value))
}

// iterFiles yields regular files under path (recursively for directories).
func iterFiles(path string) []string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return []string{path}
	}
	var out []string
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// iterLogFamilyFiles returns a configured log file and its rotated siblings
// (e.g. astrbot.log.1).
func iterLogFamilyFiles(logPath string) []string {
	var out []string
	if info, err := os.Stat(logPath); err == nil && !info.IsDir() {
		out = append(out, logPath)
	}
	dir := filepath.Dir(logPath)
	if _, err := os.Stat(dir); err != nil {
		return out
	}
	base := filepath.Base(logPath)
	ext := filepath.Ext(logPath)
	stem := strings.TrimSuffix(base, ext)
	pattern := filepath.Join(dir, stem+".*")
	if ext != "" {
		pattern = filepath.Join(dir, stem+".*"+ext)
	}
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		if m == logPath {
			continue
		}
		if info, err := os.Stat(m); err == nil && !info.IsDir() {
			out = append(out, m)
		}
	}
	return out
}

// isRegularNonSQLite reports whether path is a regular file that is not a
// SQLite database (identified by its 16-byte magic header).
func isRegularNonSQLite(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, 16)
	n, _ := f.Read(header)
	return string(header[:n]) != "SQLite format 3\x00"
}

func summarizeFiles(files []string) (int64, int) {
	var total int64
	count := 0
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		total += info.Size()
		count++
	}
	return total, count
}

func removeEmptyDirs(root string) {
	// Remove deepest-first to prune empty directories.
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func int64Of(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func sizeOf(status map[string]interface{}) int64 {
	return int64Of(status["size_bytes"])
}
