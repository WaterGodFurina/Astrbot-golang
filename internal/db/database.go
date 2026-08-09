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
	mu   sync.RWMutex
	db   *sql.DB
	path string
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
	_, err := d.db.Exec(
		`INSERT INTO cron_jobs (job_id, name, description, job_type, cron_expression, timezone, payload, enabled, persistent, run_once, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1, ?, 'scheduled')`,
		jobID, name, description, jobType, cronExpr, timezone, payload, boolInt(runOnce),
	)
	return err
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

// UpdateCronJob patches a cron job's mutable fields.
func (d *Database) UpdateCronJob(jobID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	set := ""
	args := []interface{}{}
	for k, v := range fields {
		if set != "" {
			set += ", "
		}
		switch k {
		case "enabled":
			set += k + " = ?"
			args = append(args, boolInt(v.(bool)))
		case "run_once":
			set += k + " = ?"
			args = append(args, boolInt(v.(bool)))
		default:
			set += k + " = ?"
			args = append(args, v)
		}
	}
	set += ", updated_at = CURRENT_TIMESTAMP"
	args = append(args, jobID)
	_, err := d.db.Exec(`UPDATE cron_jobs SET `+set+` WHERE job_id = ?`, args...)
	return err
}

// SetCronJobNextRun updates next_run_time.
func (d *Database) SetCronJobNextRun(jobID, nextRunTime string) error {
	_, err := d.db.Exec(
		`UPDATE cron_jobs SET next_run_time = ?, updated_at = CURRENT_TIMESTAMP WHERE job_id = ?`,
		nextRunTime, jobID,
	)
	return err
}

// DeleteCronJob removes a cron job.
func (d *Database) DeleteCronJob(jobID string) error {
	_, err := d.db.Exec(`DELETE FROM cron_jobs WHERE job_id = ?`, jobID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordPlatformMessage inserts a received platform message for statistics.
func (d *Database) RecordPlatformMessage(platformID, userID, senderID, content string) error {
	_, err := d.db.Exec(
		`INSERT INTO platform_message_history (platform_id, user_id, sender_id, content)
		 VALUES (?, ?, ?, ?)`,
		platformID, userID, senderID, content,
	)
	return err
}

// TotalMessageCount returns the total number of recorded platform messages.
func (d *Database) TotalMessageCount() int {
	var n int
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM platform_message_history`).Scan(&n)
	return n
}

// RecordProviderCall inserts a provider call record for statistics.
func (d *Database) RecordProviderCall(umo, providerID, model string, inputOther, inputCached, output int, start, end float64) error {
	_, err := d.db.Exec(
		`INSERT INTO provider_stats (umo, provider_id, provider_model, token_input_other, token_input_cached, token_output, start_time, end_time)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		umo, providerID, model, inputOther, inputCached, output, start, end,
	)
	return err
}

// TodayProviderCalls returns the number of provider calls recorded today.
func (d *Database) TodayProviderCalls() int {
	var n int
	_ = d.db.QueryRow(
		`SELECT COUNT(*) FROM provider_stats WHERE date(created_at) = date('now', 'localtime')`,
	).Scan(&n)
	return n
}

// TodayProviderTokens returns total tokens consumed today.
func (d *Database) TodayProviderTokens() int {
	var n int
	_ = d.db.QueryRow(
		`SELECT COALESCE(SUM(token_input_other + token_input_cached + token_output), 0) FROM provider_stats
		 WHERE date(created_at) = date('now', 'localtime')`,
	).Scan(&n)
	return n
}
