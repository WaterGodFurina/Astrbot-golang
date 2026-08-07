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
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Database wraps a SQLite connection with AstrBot's schema.
type Database struct {
	mu    sync.RWMutex
	db    *sql.DB
	path  string
}

// New opens (or creates) a SQLite database at the given path.
func New(dbPath string) (*Database, error) {
	// WAL mode enables concurrent readers + one writer without "database is locked"
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=30000&_foreign_keys=on", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Connection pool settings.
	// Unlike Python's SQLAlchemy where a single async connection pool caused
	// cross-event-loop issues (#9572), Go's database/sql pool is inherently
	// goroutine-safe and has no event loop binding.
	// SetMaxOpenConns(1) for SQLite writes (single writer), but with WAL mode,
	// reads can proceed concurrently from other connections.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(0) // No max lifetime for SQLite
	db.SetConnMaxIdleTime(5 * time.Minute)

	d := &Database{db: db, path: dbPath}
	if err := d.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return d, nil
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
	return nil
}
