// Package db implements AstrBot's SQLite database layer.
// Ported from astrbot/core/db/
//
// Bug fix for issue #9572: SQLAlchemy async engine connection pool issue.
// The Python code used a single async engine whose internal asyncio.Queue
// was bound to the first event loop that awaited it. When SharedPreferences
// ran DB ops on a separate background loop (_sync_loop), concurrent pool
// exhaustion caused "Queue is bound to a different event loop" errors.
//
// In Go, database/sql manages a thread-safe connection pool with no event
// loop affinity. Connections are acquired/released across goroutines without
// any cross-loop binding. This entirely eliminates the root cause of #9572.
// Additionally, we use WAL mode + busy_timeout for SQLite concurrency.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	lib "modernc.org/sqlite/lib"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// logger 供统计查询等路径记录错误。
var logger = log.GetDefault().WithComponent("DB")

// schemaVersion 是数据库 schema 的当前版本，配合 PRAGMA user_version 做迁移。
const schemaVersion = 1

// Database wraps a SQLite connection with AstrBot's schema.
type Database struct {
	db   *sql.DB
	path string
}

// New opens (or creates) a SQLite database at the given path.
//
// 使用纯 Go 驱动 modernc.org/sqlite（注册名 "sqlite"），消除对 mattn/
// go-sqlite3 的 CGO 依赖，使主程序能在 CGO_ENABLED=0、无 C 编译器的
// 环境下构建和运行。DSN 参数写法与 mattn 不同（_pragma=...）。
func New(dbPath string) (*Database, error) {
	// WAL mode enables concurrent readers + one writer without "database is locked"
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Connection pool settings.
	// Unlike Python's SQLAlchemy where a single async connection pool caused
	// cross-event-loop issues (#9572), Go's database/sql pool is inherently
	// goroutine-safe and has no event loop binding.
	// WAL 模式下任意时刻只有一个写者，这里允许 10 个连接：读取可并发进行，
	// 写入靠 busy_timeout 排队 + withRetry 兜底，而非把连接数压到 1。
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(0) // No max lifetime for SQLite
	db.SetConnMaxIdleTime(5 * time.Minute)

	d := &Database{db: db, path: dbPath}
	if err := d.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := d.quickCheck(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// quickCheck 运行 PRAGMA quick_check 做数据库完整性自检，结果非 "ok" 视为致命错误。
func (d *Database) quickCheck() error {
	var result string
	if err := d.db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check failed: %s", result)
	}
	return nil
}

// isBusyErr 判断错误是否为 SQLite 的写锁冲突（SQLITE_BUSY / SQLITE_LOCKED），
// 供 withRetry 决定是否需要重试。modernc.org/sqlite 的错误是 *sqlite.Error，
// 用 errors.As 提取后比对 lib.SQLITE_BUSY / lib.SQLITE_LOCKED 错误码。
func isBusyErr(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == lib.SQLITE_BUSY || se.Code() == lib.SQLITE_LOCKED
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") || strings.Contains(msg, "locked")
}

// withRetry 对写操作做 SQLITE_BUSY 重试（初始 1 次 + 重试 3 次，递增退避
// 50ms/200ms/500ms），作为 WAL + busy_timeout 之外的兜底：高并发写入下单个
// 连接仍可能遇到 "database is locked"。
func (d *Database) withRetry(fn func() error) error {
	backoff := []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond}
	var lastErr error
	for attempt := 0; attempt <= len(backoff); attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isBusyErr(err) {
			return err
		}
		if attempt < len(backoff) {
			time.Sleep(backoff[attempt])
		}
	}
	return lastErr
}

// Close closes the database.
func (d *Database) Close() error {
	return d.db.Close()
}

// DB returns the underlying *sql.DB for direct queries.
func (d *Database) DB() *sql.DB { return d.db }

// initSchema creates tables if they don't exist.
func (d *Database) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS platform_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			platform_id TEXT NOT NULL,
			platform_type TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			UNIQUE(timestamp, platform_id, platform_type)
		)`,
		`CREATE INDEX IF NOT EXISTS ix_platform_stats_timestamp ON platform_stats(timestamp)`,

		`CREATE TABLE IF NOT EXISTS provider_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			agent_type TEXT NOT NULL DEFAULT 'internal',
			status TEXT NOT NULL DEFAULT 'completed',
			umo TEXT NOT NULL,
			conversation_id TEXT,
			provider_id TEXT NOT NULL,
			provider_model TEXT,
			token_input_other INTEGER NOT NULL DEFAULT 0,
			token_input_cached INTEGER NOT NULL DEFAULT 0,
			token_output INTEGER NOT NULL DEFAULT 0,
			start_time REAL NOT NULL DEFAULT 0,
			end_time REAL NOT NULL DEFAULT 0,
			time_to_first_token REAL NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS ix_provider_stats_umo ON provider_stats(umo)`,
		`CREATE INDEX IF NOT EXISTS ix_provider_stats_status ON provider_stats(status)`,
		`CREATE INDEX IF NOT EXISTS ix_provider_stats_agent_type ON provider_stats(agent_type)`,

		`CREATE TABLE IF NOT EXISTS conversations (
			inner_conversation_id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT NOT NULL UNIQUE,
			platform_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			content TEXT,
			title TEXT,
			persona_id TEXT,
			token_usage INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS ix_conversations_created ON conversations(created_at DESC, inner_conversation_id DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_conversations_platform ON conversations(platform_id, created_at DESC, inner_conversation_id DESC)`,

		`CREATE TABLE IF NOT EXISTS personas (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			persona_id TEXT NOT NULL UNIQUE,
			system_prompt TEXT NOT NULL,
			begin_dialogs TEXT,
			tools TEXT,
			skills TEXT,
			custom_error_message TEXT,
			folder_id TEXT,
			sort_order INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS persona_folders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			folder_id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			parent_id TEXT,
			description TEXT,
			sort_order INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS cron_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT,
			job_type TEXT NOT NULL,
			cron_expression TEXT,
			timezone TEXT,
			payload TEXT DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 1,
			persistent INTEGER NOT NULL DEFAULT 1,
			run_once INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'scheduled',
			last_run_at DATETIME,
			next_run_time DATETIME,
			last_error TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS preferences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(scope, scope_id, key)
		)`,

		`CREATE TABLE IF NOT EXISTS platform_message_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			sender_id TEXT,
			sender_name TEXT,
			content TEXT NOT NULL,
			llm_checkpoint_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS ix_pmh_platform_user ON platform_message_history(platform_id, user_id, id)`,
		`CREATE INDEX IF NOT EXISTS ix_pmh_checkpoint ON platform_message_history(llm_checkpoint_id)`,

		`CREATE TABLE IF NOT EXISTS knowledge_bases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kb_id TEXT NOT NULL UNIQUE,
			kb_name TEXT NOT NULL,
			description TEXT,
			emoji TEXT,
			embedding_provider_id TEXT,
			rerank_provider_id TEXT,
			chunk_size INTEGER NOT NULL DEFAULT 512,
			chunk_overlap INTEGER NOT NULL DEFAULT 50,
			top_k_dense INTEGER NOT NULL DEFAULT 50,
			top_k_sparse INTEGER NOT NULL DEFAULT 50,
			top_m_final INTEGER NOT NULL DEFAULT 5,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS ix_kb_name ON knowledge_bases(kb_name)`,

		`CREATE TABLE IF NOT EXISTS knowledge_base_chunks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chunk_id TEXT NOT NULL UNIQUE,
			kb_id TEXT NOT NULL,
			doc_id TEXT NOT NULL,
			doc_name TEXT,
			content TEXT NOT NULL,
			chunk_index INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS ix_kb_chunks_kb ON knowledge_base_chunks(kb_id, doc_id)`,

		`CREATE TABLE IF NOT EXISTS platform_sessions (
			inner_id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL UNIQUE,
			platform_id TEXT NOT NULL DEFAULT 'webchat',
			creator TEXT NOT NULL,
			display_name TEXT,
			is_group INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS umo_aliases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			umo TEXT NOT NULL UNIQUE,
			creator_sender_id TEXT NOT NULL,
			auto_name TEXT,
			user_alias TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS attachments (
			inner_attachment_id INTEGER PRIMARY KEY AUTOINCREMENT,
			attachment_id TEXT NOT NULL UNIQUE,
			path TEXT NOT NULL,
			type TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS api_keys (
			inner_id INTEGER PRIMARY KEY AUTOINCREMENT,
			key_id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			scopes TEXT,
			created_by TEXT NOT NULL,
			last_used_at DATETIME,
			expires_at DATETIME,
			revoked_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS webchat_threads (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			thread_id TEXT NOT NULL UNIQUE,
			creator TEXT NOT NULL,
			parent_session_id TEXT NOT NULL,
			parent_message_id INTEGER NOT NULL,
			base_checkpoint_id TEXT NOT NULL,
			selected_text TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS ix_webchat_threads_creator ON webchat_threads(creator)`,
		`CREATE INDEX IF NOT EXISTS ix_webchat_threads_parent_session ON webchat_threads(parent_session_id)`,
		`CREATE INDEX IF NOT EXISTS ix_webchat_threads_parent_msg ON webchat_threads(parent_message_id)`,

		`CREATE TABLE IF NOT EXISTS chatui_projects (
			inner_id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id TEXT NOT NULL UNIQUE,
			creator TEXT NOT NULL,
			emoji TEXT DEFAULT '📁',
			title TEXT NOT NULL,
			description TEXT,
			workspace_type TEXT NOT NULL DEFAULT 'session',
			workspace_path TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS session_project_relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL UNIQUE,
			project_id TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS command_configs (
			handler_full_name TEXT PRIMARY KEY,
			plugin_name TEXT NOT NULL,
			module_path TEXT NOT NULL,
			original_command TEXT NOT NULL,
			resolved_command TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			keep_original_alias INTEGER NOT NULL DEFAULT 0,
			conflict_key TEXT,
			resolution_strategy TEXT,
			note TEXT,
			extra_data TEXT,
			auto_managed INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS command_conflicts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conflict_key TEXT NOT NULL,
			handler_full_name TEXT NOT NULL,
			plugin_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			resolution TEXT,
			resolved_command TEXT,
			note TEXT,
			extra_data TEXT,
			auto_generated INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(conflict_key, handler_full_name)
		)`,

		`CREATE TABLE IF NOT EXISTS dashboard_trusted_devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token_hash TEXT NOT NULL UNIQUE,
			totp_secret_hash TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}

	for _, stmt := range statements {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec schema: %w\nSQL: %s", err, stmt)
		}
	}

	// 迁移机制：以 PRAGMA user_version 记录 schema 版本。当前版本尚无迁移脚本，
	// 仅确保版本号写入（后续 schema 变更在此处按版本递增执行 ALTER 等迁移）。
	var version int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version < schemaVersion {
		if _, err := d.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil { // nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- #nosec G201: schemaVersion 为编译期常量，非用户输入
			return fmt.Errorf("set user_version: %w", err)
		}
		logger.Info("SQLite schema migrated from version %d to %d", version, schemaVersion)
	}
	return nil
}

// ConversationRow is one row from the conversations table.
type ConversationRow struct {
	InnerID        int64
	ConversationID string
	PlatformID     string
	UserID         string
	Content        string
	Title          string
	PersonaID      string
	CreatedAt      string
	UpdatedAt      string
}

// CreateConversation inserts a new conversation.
func (d *Database) CreateConversation(convID, userID, platformID, content, title, personaID string) error {
	_, err := d.db.Exec(
		`INSERT INTO conversations (conversation_id, platform_id, user_id, content, title, persona_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		convID, platformID, userID, content, title, personaID,
	)
	return err
}

// UpsertConversation atomically persists a conversation: it checks existence
// and performs INSERT-or-UPDATE inside a single transaction, so concurrent
// persists cannot both decide to INSERT (unique-violation) or clobber each
// other's history. Callers that previously did GetConversationByID + Update/
// Create (a classic TOCTOU race) should switch to this.
func (d *Database) UpsertConversation(convID, userID, platformID, content, title, personaID string) error {
	return d.withRetry(func() error {
		tx, err := d.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var exists int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM conversations WHERE conversation_id = ?`, convID,
		).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			if _, err := tx.Exec(
				`UPDATE conversations SET content = ?, updated_at = CURRENT_TIMESTAMP WHERE conversation_id = ?`,
				content, convID,
			); err != nil {
				return err
			}
			if title != "" {
				if _, err := tx.Exec(
					`UPDATE conversations SET title = ? WHERE conversation_id = ?`, title, convID,
				); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO conversations (conversation_id, platform_id, user_id, content, title, persona_id)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				convID, platformID, userID, content, title, personaID,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// GetConversationByUserID returns the most recent conversation for a user
// (unified_msg_origin), or an empty row with found=false.
func (d *Database) GetConversationByUserID(userID string) (ConversationRow, bool, error) {
	var row ConversationRow
	err := d.db.QueryRow(
		`SELECT inner_conversation_id, conversation_id, platform_id, user_id, content, title, persona_id, created_at, updated_at
		 FROM conversations WHERE user_id = ? ORDER BY updated_at DESC, inner_conversation_id DESC LIMIT 1`,
		userID,
	).Scan(&row.InnerID, &row.ConversationID, &row.PlatformID, &row.UserID, &row.Content, &row.Title, &row.PersonaID, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
}

// GetConversationByID returns a conversation by its id.
func (d *Database) GetConversationByID(convID string) (ConversationRow, bool, error) {
	var row ConversationRow
	err := d.db.QueryRow(
		`SELECT inner_conversation_id, conversation_id, platform_id, user_id, content, title, persona_id, created_at, updated_at
		 FROM conversations WHERE conversation_id = ?`,
		convID,
	).Scan(&row.InnerID, &row.ConversationID, &row.PlatformID, &row.UserID, &row.Content, &row.Title, &row.PersonaID, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
}

// ListConversations returns all conversations ordered by updated_at desc.
func (d *Database) ListConversations() ([]ConversationRow, error) {
	rows, err := d.db.Query(
		`SELECT inner_conversation_id, conversation_id, platform_id, user_id, content, title, persona_id, created_at, updated_at
		 FROM conversations ORDER BY updated_at DESC, inner_conversation_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ConversationRow
	for rows.Next() {
		var row ConversationRow
		if err := rows.Scan(&row.InnerID, &row.ConversationID, &row.PlatformID, &row.UserID, &row.Content, &row.Title, &row.PersonaID, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// UpdateConversationContent updates a conversation's history JSON.
func (d *Database) UpdateConversationContent(convID, content string) error {
	_, err := d.db.Exec(
		`UPDATE conversations SET content = ?, updated_at = CURRENT_TIMESTAMP WHERE conversation_id = ?`,
		content, convID,
	)
	return err
}

// ConversationFilter mirrors the dashboard conversation list query params
// (aligned with Python sqlite.get_filtered_conversations).
type ConversationFilter struct {
	Platforms        []string // platform_id IN (...)
	MessageTypes     []string // user_id LIKE '%:<type>:%' (UMO segment match)
	Search           string   // title/user_id/conversation_id/content LIKE %q%
	ExcludeIDs       []string // user_id NOT LIKE '<id>%'
	ExcludePlatforms []string // platform_id NOT IN (...)
	Page             int
	PageSize         int
}

// GetFilteredConversations returns a filtered, paginated conversation list
// plus the total count matching the filter.
func (d *Database) GetFilteredConversations(f ConversationFilter) ([]ConversationRow, int, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	size := f.PageSize
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}

	where := []string{"1=1"}
	var args []interface{}
	if len(f.Platforms) > 0 {
		where = append(where, fmt.Sprintf("platform_id IN (%s)", placeholders(len(f.Platforms), &args, f.Platforms)))
	}
	if len(f.MessageTypes) > 0 {
		conds := make([]string, 0, len(f.MessageTypes))
		for _, mt := range f.MessageTypes {
			conds = append(conds, "user_id LIKE ?")
			args = append(args, "%:"+mt+":%")
		}
		where = append(where, "("+strings.Join(conds, " OR ")+")")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + s + "%"
		where = append(where, "(title LIKE ? OR user_id LIKE ? OR conversation_id LIKE ? OR content LIKE ?)")
		for i := 0; i < 4; i++ {
			args = append(args, like)
		}
	}
	for _, ex := range f.ExcludeIDs {
		if ex == "" {
			continue
		}
		where = append(where, "user_id NOT LIKE ?")
		args = append(args, ex+"%")
	}
	if len(f.ExcludePlatforms) > 0 {
		where = append(where, fmt.Sprintf("platform_id NOT IN (%s)", placeholders(len(f.ExcludePlatforms), &args, f.ExcludePlatforms)))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM conversations WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT inner_conversation_id, conversation_id, platform_id, user_id, content, title, persona_id, created_at, updated_at
		 FROM conversations WHERE ` + whereSQL + ` ORDER BY updated_at DESC, inner_conversation_id DESC LIMIT ? OFFSET ?`
	args = append(args, size, (page-1)*size)
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var result []ConversationRow
	for rows.Next() {
		var row ConversationRow
		if err := rows.Scan(&row.InnerID, &row.ConversationID, &row.PlatformID, &row.UserID, &row.Content, &row.Title, &row.PersonaID, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, 0, err
		}
		result = append(result, row)
	}
	return result, total, rows.Err()
}

// placeholders expands n bind placeholders and appends values to args.
func placeholders(n int, args *[]interface{}, vals []string) string {
	out := make([]string, n)
	for i, v := range vals {
		out[i] = "?"
		*args = append(*args, v)
	}
	return strings.Join(out, ",")
}

// UpdateConversationPersona updates a conversation's persona id.
func (d *Database) UpdateConversationPersona(convID, personaID string) error {
	_, err := d.db.Exec(
		`UPDATE conversations SET persona_id = ?, updated_at = CURRENT_TIMESTAMP WHERE conversation_id = ?`,
		personaID, convID,
	)
	return err
}

// UpdateConversationTitle updates a conversation's title.
func (d *Database) UpdateConversationTitle(convID, title string) error {
	_, err := d.db.Exec(
		`UPDATE conversations SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE conversation_id = ?`,
		title, convID,
	)
	return err
}

// DeleteConversationByID deletes a conversation row.
func (d *Database) DeleteConversationByID(convID string) error {
	_, err := d.db.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, convID)
	return err
}

// PreferenceRow is one row from the preferences table.
type PreferenceRow struct {
	Scope   string
	ScopeID string
	Key     string
	Value   string
}

// GetPreference returns the value for (scope, scope_id, key) and whether it
// exists. Mirrors the Python session preference read (`sp.session_get`).
func (d *Database) GetPreference(scope, scopeID, key string) (string, bool, error) {
	var val string
	err := d.db.QueryRow(
		`SELECT value FROM preferences WHERE scope = ? AND scope_id = ? AND key = ?`,
		scope, scopeID, key,
	).Scan(&val)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SetPreference upserts a preference value (`sp.session_put`).
func (d *Database) SetPreference(scope, scopeID, key, value string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`INSERT INTO preferences (scope, scope_id, key, value)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(scope, scope_id, key)
			 DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
			scope, scopeID, key, value,
		)
		return err
	})
}

// DeletePreference removes a preference row.
func (d *Database) DeletePreference(scope, scopeID, key string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`DELETE FROM preferences WHERE scope = ? AND scope_id = ? AND key = ?`,
			scope, scopeID, key,
		)
		return err
	})
}

// ListPreferencesByScope returns every row for a scope.
func (d *Database) ListPreferencesByScope(scope string) ([]PreferenceRow, error) {
	rows, err := d.db.Query(
		`SELECT scope, scope_id, key, value FROM preferences WHERE scope = ? ORDER BY scope_id, key`,
		scope,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreferenceRow
	for rows.Next() {
		var r PreferenceRow
		if err := rows.Scan(&r.Scope, &r.ScopeID, &r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPreferencesByScopeID returns every row for a scope + scope_id.
func (d *Database) ListPreferencesByScopeID(scope, scopeID string) ([]PreferenceRow, error) {
	rows, err := d.db.Query(
		`SELECT scope, scope_id, key, value FROM preferences WHERE scope = ? AND scope_id = ? ORDER BY key`,
		scope, scopeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreferenceRow
	for rows.Next() {
		var r PreferenceRow
		if err := rows.Scan(&r.Scope, &r.ScopeID, &r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeletePreferencesByScopeID removes every preference row for a scope + id
// (e.g. all rules of a session when deleting "all rules").
func (d *Database) DeletePreferencesByScopeID(scope, scopeID string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`DELETE FROM preferences WHERE scope = ? AND scope_id = ?`,
			scope, scopeID,
		)
		return err
	})
}

// CronJobRow is one row from the cron_jobs table.
type CronJobRow struct {
	JobID          string
	Name           string
	Description    string
	JobType        string
	CronExpression string
	Timezone       string
	Payload        string
	Enabled        bool
	Persistent     bool
	RunOnce        bool
	Status         string
	NextRunTime    string
}

// CreateCronJob inserts a new cron job.
func (d *Database) CreateCronJob(jobID, name, description, jobType, cronExpr, timezone, payload string, runOnce bool) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`INSERT INTO cron_jobs (job_id, name, description, job_type, cron_expression, timezone, payload, enabled, persistent, run_once, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1, ?, 'scheduled')`,
			jobID, name, description, jobType, cronExpr, timezone, payload, boolInt(runOnce),
		)
		return err
	})
}

// ListCronJobs returns all cron jobs ordered by created_at.
func (d *Database) ListCronJobs() ([]CronJobRow, error) {
	rows, err := d.db.Query(
		`SELECT job_id, name, description, job_type, cron_expression, timezone, payload, enabled, persistent, run_once, status, next_run_time
		 FROM cron_jobs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CronJobRow
	for rows.Next() {
		var row CronJobRow
		var enabled, persistent, runOnce int
		if err := rows.Scan(&row.JobID, &row.Name, &row.Description, &row.JobType, &row.CronExpression, &row.Timezone, &row.Payload, &enabled, &persistent, &runOnce, &row.Status, &row.NextRunTime); err != nil {
			return nil, err
		}
		row.Enabled = enabled != 0
		row.Persistent = persistent != 0
		row.RunOnce = runOnce != 0
		result = append(result, row)
	}
	return result, rows.Err()
}

// GetCronJob returns a single cron job by id.
func (d *Database) GetCronJob(jobID string) (CronJobRow, bool, error) {
	var row CronJobRow
	var enabled, persistent, runOnce int
	err := d.db.QueryRow(
		`SELECT job_id, name, description, job_type, cron_expression, timezone, payload, enabled, persistent, run_once, status, next_run_time
		 FROM cron_jobs WHERE job_id = ?`, jobID,
	).Scan(&row.JobID, &row.Name, &row.Description, &row.JobType, &row.CronExpression, &row.Timezone, &row.Payload, &enabled, &persistent, &runOnce, &row.Status, &row.NextRunTime)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	row.Enabled = enabled != 0
	row.Persistent = persistent != 0
	row.RunOnce = runOnce != 0
	return row, true, nil
}

// cronJobWritableFields is the allowlist of columns UpdateCronJob may touch.
// Using a fixed allowlist (rather than interpolating map keys) prevents SQL
// injection through arbitrary field names.
var cronJobWritableFields = map[string]bool{
	"name":            true,
	"description":     true,
	"job_type":        true,
	"cron_expression": true,
	"timezone":        true,
	"payload":         true,
	"status":          true,
	"persistent":      true,
	"enabled":         true,
	"run_once":        true,
	"next_run_time":   true,
}

// UpdateCronJob patches a cron job's mutable fields.
func (d *Database) UpdateCronJob(jobID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	set := ""
	args := []interface{}{}
	for k, v := range fields {
		if !cronJobWritableFields[k] {
			return fmt.Errorf("UpdateCronJob: unknown field %q", k)
		}
		if set != "" {
			set += ", "
		}
		switch k {
		case "enabled", "run_once", "persistent":
			// Accept bool or JSON-ish int; reject other types instead of
			// panicking on an unsafe type assertion.
			b, err := asBool(v)
			if err != nil {
				return fmt.Errorf("UpdateCronJob: field %q: %w", k, err)
			}
			set += k + " = ?"
			args = append(args, boolInt(b))
		default:
			set += k + " = ?"
			args = append(args, v)
		}
	}
	set += ", updated_at = CURRENT_TIMESTAMP"
	args = append(args, jobID)
	// #nosec G202 -- `set` is assembled only from keys validated against the
	// cronJobWritableFields whitelist; all values go through placeholders.
	query := `UPDATE cron_jobs SET ` + set + ` WHERE job_id = ?` // nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query
	return d.withRetry(func() error {
		_, err := d.db.Exec(query, args...)
		return err
	})
}

// asBool coerces a config value to bool without panicking on the wrong type.
func asBool(v interface{}) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case int:
		return t != 0, nil
	case int64:
		return t != 0, nil
	case float64:
		return t != 0, nil
	case string:
		b, err := strconv.ParseBool(t)
		if err != nil {
			return false, fmt.Errorf("invalid bool value %q", t)
		}
		return b, nil
	default:
		return false, fmt.Errorf("unexpected type %T", v)
	}
}

// SetCronJobNextRun updates next_run_time.
func (d *Database) SetCronJobNextRun(jobID, nextRunTime string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`UPDATE cron_jobs SET next_run_time = ?, updated_at = CURRENT_TIMESTAMP WHERE job_id = ?`,
			nextRunTime, jobID,
		)
		return err
	})
}

// DeleteCronJob removes a cron job.
func (d *Database) DeleteCronJob(jobID string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(`DELETE FROM cron_jobs WHERE job_id = ?`, jobID)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordPlatformMessage inserts a received platform message for statistics.
func (d *Database) RecordPlatformMessage(platformID, userID, senderID, content string) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`INSERT INTO platform_message_history (platform_id, user_id, sender_id, content)
			 VALUES (?, ?, ?, ?)`,
			platformID, userID, senderID, content,
		)
		return err
	})
}

// TrimPlatformMessageHistory keeps only the most recent `keep` rows for a
// platform session (mirrors Python insert_message_chain max_messages trimming).
func (d *Database) TrimPlatformMessageHistory(platformID, userID string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`DELETE FROM platform_message_history WHERE platform_id=? AND user_id=?
			 AND id NOT IN (SELECT id FROM platform_message_history
			   WHERE platform_id=? AND user_id=? ORDER BY id DESC LIMIT ?)`,
			platformID, userID, platformID, userID, keep,
		)
		return err
	})
}

// PlatformMessageRow is one row from platform_message_history.
type PlatformMessageRow struct {
	ID         int64
	SenderID   string
	SenderName string
	Content    string
	CreatedAt  string
}

// GetPlatformMessageHistory returns the most recent messages for a platform
// session (used by the get_group_message_history tool).
func (d *Database) GetPlatformMessageHistory(platformID, userID string, limit int) ([]PlatformMessageRow, error) {
	rows, err := d.db.Query(
		`SELECT id, COALESCE(sender_id,''), COALESCE(sender_name,''), content, COALESCE(created_at,'')
		 FROM platform_message_history WHERE platform_id=? AND user_id=? ORDER BY id DESC LIMIT ?`,
		platformID, userID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformMessageRow
	for rows.Next() {
		var r PlatformMessageRow
		if err := rows.Scan(&r.ID, &r.SenderID, &r.SenderName, &r.Content, &r.CreatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TotalMessageCount returns the total number of recorded platform messages.
func (d *Database) TotalMessageCount() int {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM platform_message_history`).Scan(&n); err != nil {
		logger.Error("TotalMessageCount: %v", err)
		return 0
	}
	return n
}

// RecordProviderCall inserts a provider call record for statistics.
// 高频写点：经 withRetry 处理 SQLITE_BUSY。
func (d *Database) RecordProviderCall(umo, providerID, model string, inputOther, inputCached, output int, start, end float64) error {
	return d.withRetry(func() error {
		_, err := d.db.Exec(
			`INSERT INTO provider_stats (umo, provider_id, provider_model, token_input_other, token_input_cached, token_output, start_time, end_time)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			umo, providerID, model, inputOther, inputCached, output, start, end,
		)
		return err
	})
}

// TodayProviderCalls returns the number of provider calls recorded today.
func (d *Database) TodayProviderCalls() int {
	var n int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM provider_stats WHERE date(created_at) = date('now', 'localtime')`,
	).Scan(&n); err != nil {
		logger.Error("TodayProviderCalls: %v", err)
		return 0
	}
	return n
}

// TodayProviderTokens returns total tokens consumed today.
func (d *Database) TodayProviderTokens() int {
	var n int
	if err := d.db.QueryRow(
		`SELECT COALESCE(SUM(token_input_other + token_input_cached + token_output), 0) FROM provider_stats
		 WHERE date(created_at) = date('now', 'localtime')`,
	).Scan(&n); err != nil {
		logger.Error("TodayProviderTokens: %v", err)
		return 0
	}
	return n
}

// MessageBucket is one point in a message/time series.
type MessageBucket struct {
	Timestamp int64 // unix seconds (bucket start)
	Count     int
}

// PlatformMessageRank returns messages grouped by platform within the given
// lookback window (offsetSec seconds). Ordered by count descending.
func (d *Database) PlatformMessageRank(offsetSec int) []map[string]interface{} {
	rows, err := d.db.Query(
		`SELECT platform_id, COUNT(*) FROM platform_message_history
		 WHERE created_at >= datetime('now', '-' || ? || ' seconds')
		 GROUP BY platform_id ORDER BY COUNT(*) DESC`,
		offsetSec,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"name":  name,
			"count": count,
		})
	}
	return out
}

// MessageTimeSeries buckets message history into `bucketSec`-wide windows over
// the last offsetSec seconds, returning [timestamp_seconds, count] pairs.
func (d *Database) MessageTimeSeries(offsetSec, bucketSec int) [][]int {
	rows, err := d.db.Query(
		`SELECT CAST(strftime('%s', created_at) AS INTEGER) FROM platform_message_history
		 WHERE created_at >= datetime('now', '-' || ? || ' seconds')`,
		offsetSec,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	now := time.Now().Unix()
	start := now - int64(offsetSec)
	counts := map[int64]int{}
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			continue
		}
		bucket := (ts / int64(bucketSec)) * int64(bucketSec)
		if bucket < start {
			continue
		}
		counts[bucket]++
	}

	var out [][]int
	for t := start - start%int64(bucketSec); t <= now; t += int64(bucketSec) {
		out = append(out, []int{int(t), counts[t]})
	}
	return out
}

// ProviderStatRow is a single provider_stats record used for token statistics.
type ProviderStatRow struct {
	UMO         string
	ProviderID  string
	Model       string
	InputOther  int
	InputCached int
	Output      int
	CreatedAt   time.Time
}

// ProviderStatsSince returns provider_stats records with created_at >= the
// given time, ordered ascending.
func (d *Database) ProviderStatsSince(since time.Time) ([]ProviderStatRow, error) {
	rows, err := d.db.Query(
		`SELECT umo, provider_id, provider_model, token_input_other, token_input_cached,
		        token_output, created_at FROM provider_stats WHERE created_at >= ?
		 ORDER BY created_at ASC`,
		since.UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderStatRow
	for rows.Next() {
		var r ProviderStatRow
		var created string
		if err := rows.Scan(&r.UMO, &r.ProviderID, &r.Model, &r.InputOther, &r.InputCached, &r.Output, &created); err != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			r.CreatedAt = t
		} else if t, err := time.ParseInLocation("2006-01-02 15:04:05", created, time.UTC); err == nil {
			r.CreatedAt = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KBRow is a knowledge base record persisted in SQLite.
type KBRow struct {
	KBID                string
	KBName              string
	Description         string
	Emoji               string
	EmbeddingProviderID string
	RerankProviderID    string
	ChunkSize           int
	ChunkOverlap        int
	TopKDense           int
	TopKSparse          int
	TopMFinal           int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// CreateKB persists a new knowledge base row.
func (d *Database) CreateKB(r KBRow) error {
	_, err := d.db.Exec(
		`INSERT INTO knowledge_bases (kb_id, kb_name, description, emoji, embedding_provider_id,
			rerank_provider_id, chunk_size, chunk_overlap, top_k_dense, top_k_sparse, top_m_final)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.KBID, r.KBName, r.Description, r.Emoji, r.EmbeddingProviderID,
		r.RerankProviderID, r.ChunkSize, r.ChunkOverlap, r.TopKDense, r.TopKSparse, r.TopMFinal,
	)
	return err
}

// UpdateKB updates an existing knowledge base row by kb_id.
func (d *Database) UpdateKB(kbID string, r KBRow) error {
	_, err := d.db.Exec(
		`UPDATE knowledge_bases SET kb_name=?, description=?, emoji=?, embedding_provider_id=?,
			rerank_provider_id=?, chunk_size=?, chunk_overlap=?, top_k_dense=?, top_k_sparse=?,
			top_m_final=?, updated_at=CURRENT_TIMESTAMP WHERE kb_id=?`,
		r.KBName, r.Description, r.Emoji, r.EmbeddingProviderID,
		r.RerankProviderID, r.ChunkSize, r.ChunkOverlap, r.TopKDense, r.TopKSparse, r.TopMFinal,
		kbID,
	)
	return err
}

// DeleteKB removes a knowledge base row by kb_id.
func (d *Database) DeleteKB(kbID string) error {
	_, err := d.db.Exec(`DELETE FROM knowledge_bases WHERE kb_id=?`, kbID)
	return err
}

// GetKB returns a knowledge base row by kb_id.
func (d *Database) GetKB(kbID string) (*KBRow, error) {
	rows, err := d.db.Query(
		`SELECT kb_id, kb_name, COALESCE(description,''), COALESCE(emoji,''),
			COALESCE(embedding_provider_id,''), COALESCE(rerank_provider_id,''),
			chunk_size, chunk_overlap, top_k_dense, top_k_sparse, top_m_final,
			COALESCE(created_at,''), COALESCE(updated_at,'')
		 FROM knowledge_bases WHERE kb_id=?`, kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("knowledge base %s not found", kbID)
	}
	return scanKBRow(rows)
}

// ListKBs returns all persisted knowledge base rows.
func (d *Database) ListKBs() ([]KBRow, error) {
	rows, err := d.db.Query(
		`SELECT kb_id, kb_name, COALESCE(description,''), COALESCE(emoji,''),
			COALESCE(embedding_provider_id,''), COALESCE(rerank_provider_id,''),
			chunk_size, chunk_overlap, top_k_dense, top_k_sparse, top_m_final,
			COALESCE(created_at,''), COALESCE(updated_at,'')
		 FROM knowledge_bases ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KBRow
	for rows.Next() {
		r, err := scanKBRow(rows)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanKBRow(rows *sql.Rows) (*KBRow, error) {
	var r KBRow
	var created, updated string
	if err := rows.Scan(&r.KBID, &r.KBName, &r.Description, &r.Emoji, &r.EmbeddingProviderID,
		&r.RerankProviderID, &r.ChunkSize, &r.ChunkOverlap, &r.TopKDense, &r.TopKSparse, &r.TopMFinal,
		&created, &updated); err != nil {
		return nil, err
	}
	// SQLite CURRENT_TIMESTAMP stores UTC; parse as UTC so timezone conversion
	// happens at render time (kbRowToMap outputs local time).
	if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
		r.CreatedAt = t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", updated); err == nil {
		r.UpdatedAt = t
	}
	return &r, nil
}

// KBChunk is a persisted knowledge-base chunk record.
type KBChunk struct {
	ChunkID  string
	KBID     string
	DocID    string
	DocName  string
	Content  string
	ChunkIdx int
}

// InsertKBChunk persists one chunk record.
func (d *Database) InsertKBChunk(c KBChunk) error {
	_, err := d.db.Exec(
		`INSERT INTO knowledge_base_chunks (chunk_id, kb_id, doc_id, doc_name, content, chunk_index)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.ChunkID, c.KBID, c.DocID, c.DocName, c.Content, c.ChunkIdx,
	)
	return err
}

// ListKBChunks returns chunk records for a KB, optionally filtered by doc_id.
func (d *Database) ListKBChunks(kbID, docID string) ([]KBChunk, error) {
	return d.ListKBChunksPage(kbID, docID, 0, 0)
}

// ListKBChunksPage returns one page of chunk records for a KB (limit<=0 =
// no limit), optionally filtered by doc_id.
func (d *Database) ListKBChunksPage(kbID, docID string, limit, offset int) ([]KBChunk, error) {
	query := `SELECT chunk_id, kb_id, doc_id, COALESCE(doc_name,''), content, chunk_index
		FROM knowledge_base_chunks WHERE kb_id=?`
	args := []any{kbID}
	if docID != "" {
		query += ` AND doc_id=?`
		args = append(args, docID)
	}
	query += ` ORDER BY chunk_index ASC`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KBChunk
	for rows.Next() {
		var c KBChunk
		if err := rows.Scan(&c.ChunkID, &c.KBID, &c.DocID, &c.DocName, &c.Content, &c.ChunkIdx); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountKBChunks returns the number of chunks for a KB (optionally per doc).
func (d *Database) CountKBChunks(kbID, docID string) (int, error) {
	query := `SELECT COUNT(*) FROM knowledge_base_chunks WHERE kb_id=?`
	args := []any{kbID}
	if docID != "" {
		query += ` AND doc_id=?`
		args = append(args, docID)
	}
	var n int
	err := d.db.QueryRow(query, args...).Scan(&n)
	return n, err
}

// CountKBChunksByDoc returns chunk counts grouped by doc_id for a KB in a
// single query（文档列表统计的聚合版，替代逐文档 N+1 的 CountKBChunks）。
func (d *Database) CountKBChunksByDoc(kbID string) (map[string]int, error) {
	rows, err := d.db.Query(
		`SELECT doc_id, COUNT(*) FROM knowledge_base_chunks WHERE kb_id=? GROUP BY doc_id`,
		kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var docID string
		var n int
		if err := rows.Scan(&docID, &n); err != nil {
			return nil, err
		}
		out[docID] = n
	}
	return out, rows.Err()
}

// DeleteKBChunks removes all chunks for a KB (optionally per doc).
func (d *Database) DeleteKBChunks(kbID, docID string) error {
	query := `DELETE FROM knowledge_base_chunks WHERE kb_id=?`
	args := []any{kbID}
	if docID != "" {
		query += ` AND doc_id=?`
		args = append(args, docID)
	}
	_, err := d.db.Exec(query, args...)
	return err
}

// DeleteKBDoc removes a document's file records. Documents are stored on disk;
// this clears their chunk records.
func (d *Database) DeleteKBDoc(kbID, docID string) error {
	return d.DeleteKBChunks(kbID, docID)
}
