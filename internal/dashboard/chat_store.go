// Package dashboard - chat session persistence.
package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type chatSession struct {
	SessionID   string                   `json:"session_id"`
	DisplayName string                   `json:"display_name,omitempty"`
	PlatformID  string                   `json:"platform_id"`
	Creator     string                   `json:"creator,omitempty"`
	IsGroup     int                      `json:"is_group"`
	CreatedAt   string                   `json:"created_at"`
	UpdatedAt   string                   `json:"updated_at"`
	Messages    []map[string]interface{} `json:"messages"`
}

type chatData struct {
	Sessions []*chatSession `json:"sessions"`
}

type chatStore struct {
	mu   sync.Mutex
	path string
	data *chatData
}

func newChatStore(dataDir string) *chatStore {
	cs := &chatStore{
		path: filepath.Join(dataDir, "chat_sessions.json"),
		data: &chatData{Sessions: []*chatSession{}},
	}
	cs.load()
	return cs
}

func (cs *chatStore) load() {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, cs.data)
	if cs.data.Sessions == nil {
		cs.data.Sessions = []*chatSession{}
	}
}

func (cs *chatStore) save() error {
	data, err := json.MarshalIndent(cs.data, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：先写临时文件并 Sync，再 Rename，避免中途崩溃留下半截 JSON。
	// 0600：聊天记录含完整对话，仅当前用户可读。
	return writeFileAtomic(cs.path, data, 0600)
}

// writeFileAtomic 将 data 原子地写入 path（临时文件 + fsync + rename）。
// 供 chat_store / mcp_store 等 JSON 持久化使用，防止非原子 WriteFile
// 在崩溃或并发读时读到不完整文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// createSession creates a new chat session and returns it.
func (cs *chatStore) createSession(platformID string) (*chatSession, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now()
	session := &chatSession{
		SessionID:   now.Format("20060102150405") + randomSuffix(),
		PlatformID:  platformID,
		CreatedAt:   now.Format(time.RFC3339),
		UpdatedAt:   now.Format(time.RFC3339),
		Messages:    []map[string]interface{}{},
		DisplayName: "",
	}
	cs.data.Sessions = append(cs.data.Sessions, session)
	if err := cs.save(); err != nil {
		return nil, err
	}
	return session, nil
}

// listSessions returns all sessions ordered by updated_at desc.
func (cs *chatStore) listSessions() []map[string]interface{} {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(cs.data.Sessions))
	for _, s := range cs.data.Sessions {
		result = append(result, sessionView(s))
	}
	return result
}

func sessionView(s *chatSession) map[string]interface{} {
	return map[string]interface{}{
		"session_id":   s.SessionID,
		"display_name": s.DisplayName,
		"updated_at":   s.UpdatedAt,
		"platform_id":  s.PlatformID,
		"creator":      s.Creator,
		"is_group":     s.IsGroup,
		"created_at":   s.CreatedAt,
	}
}

// getSession returns a session view or nil.
func (cs *chatStore) getSession(id string) *chatSession {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			return s
		}
	}
	return nil
}

// updateSession patches a session (display_name etc.).
func (cs *chatStore) updateSession(id string, patch map[string]interface{}) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			if name, ok := patch["display_name"].(string); ok {
				s.DisplayName = name
			}
			s.UpdatedAt = time.Now().Format(time.RFC3339)
			return cs.save()
		}
	}
	return nil
}

// deleteSessions removes sessions by id and returns the deleted count.
func (cs *chatStore) deleteSessions(ids []string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	target := map[string]bool{}
	for _, id := range ids {
		target[id] = true
	}
	next := make([]*chatSession, 0, len(cs.data.Sessions))
	deleted := 0
	for _, s := range cs.data.Sessions {
		if target[s.SessionID] {
			deleted++
			continue
		}
		next = append(next, s)
	}
	cs.data.Sessions = next
	_ = cs.save()
	return deleted
}

// appendMessage adds a message to a session.
func (cs *chatStore) appendMessage(id string, msg map[string]interface{}) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID == id {
			s.Messages = append(s.Messages, msg)
			s.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = cs.save()
			return true
		}
	}
	return false
}

// sessionDetail returns the session detail payload for GET /chat/sessions/{id}.
func (cs *chatStore) sessionDetail(id string) map[string]interface{} {
	s := cs.getSession(id)
	if s == nil {
		return nil
	}
	cs.mu.Lock()
	messages := make([]map[string]interface{}, len(s.Messages))
	copy(messages, s.Messages)
	cs.mu.Unlock()
	return map[string]interface{}{
		"session_id":  s.SessionID,
		"history":     messages,
		"threads":     []interface{}{},
		"project":     nil,
		"active_runs": []interface{}{},
	}
}

// findMessage returns the index of a session message by id.
func (cs *chatStore) findMessage(sessionID, messageID string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID != sessionID {
			continue
		}
		for i, m := range s.Messages {
			if mid, _ := m["id"].(string); mid == messageID {
				return i
			}
		}
	}
	return -1
}

// truncateMessagesAfter removes every message after index keepIndex (keeps
// [0, keepIndex]) and persists. Returns false when the session is unknown.
func (cs *chatStore) truncateMessagesAfter(sessionID string, keepIndex int) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID != sessionID {
			continue
		}
		if keepIndex < 0 || keepIndex >= len(s.Messages) {
			return false
		}
		if keepIndex+1 < len(s.Messages) {
			s.Messages = s.Messages[:keepIndex+1]
			s.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = cs.save()
		}
		return true
	}
	return false
}

// updateMessageContent replaces a session message's content (only user-type
// messages; only the latest user message may be edited, mirroring Python
// chat_service.update_message). The truncated list of messages after the
// edited one is returned so the caller can drop them from the UI.
func (cs *chatStore) updateMessageContent(sessionID, messageID string, content map[string]interface{}) (map[string]interface{}, []map[string]interface{}, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, s := range cs.data.Sessions {
		if s.SessionID != sessionID {
			continue
		}
		// 找到目标消息并校验：必须为 user 类型，且是会话中最后一条 user 消息。
		targetIdx := -1
		for i, m := range s.Messages {
			if mid, _ := m["id"].(string); mid == messageID {
				targetIdx = i
			}
		}
		if targetIdx < 0 {
			return nil, nil, false
		}
		target := s.Messages[targetIdx]
		if t, _ := target["type"].(string); t != "user" {
			return nil, nil, false
		}
		if t, _ := target["content"].(map[string]interface{})["type"].(string); t != "user" {
			return nil, nil, false
		}
		for i := targetIdx + 1; i < len(s.Messages); i++ {
			if t, _ := s.Messages[i]["type"].(string); t == "user" {
				return nil, nil, false // 目标不是最新 user 消息
			}
		}
		contentType, _ := content["type"].(string)
		if oldType, _ := target["content"].(map[string]interface{})["type"].(string); contentType != "" && oldType != "" && contentType != oldType {
			return nil, nil, false // 消息类型不可改变（Python 语义）
		}
		target["content"] = content
		var truncated []map[string]interface{}
		if targetIdx+1 < len(s.Messages) {
			truncated = append([]map[string]interface{}{}, s.Messages[targetIdx+1:]...)
			s.Messages = s.Messages[:targetIdx+1]
		}
		s.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = cs.save()
		return target, truncated, true
	}
	return nil, nil, false
}

func randomSuffix() string {
	ts := time.Now().UnixNano()
	return string(rune('a'+ts%26)) + string(rune('a'+(ts/26)%26)) + string(rune('a'+(ts/676)%26))
}

// ── Chat threads store ─────────────────────────────────────────

// chatThread is a WebChat side thread created from a selected bot response.
// 对齐 Python webchat_threads 表字段（thread_id/creator/parent_session_id/
// parent_message_id/base_checkpoint_id/selected_text），消息存于 Messages。
type chatThread struct {
	ThreadID         string                   `json:"thread_id"`
	Creator          string                   `json:"creator"`
	ParentSessionID  string                   `json:"parent_session_id"`
	ParentMessageID  string                   `json:"parent_message_id"`
	BaseCheckpointID string                   `json:"base_checkpoint_id"`
	SelectedText     string                   `json:"selected_text"`
	CreatedAt        string                   `json:"created_at"`
	UpdatedAt        string                   `json:"updated_at"`
	Messages         []map[string]interface{} `json:"messages"`
}

type threadData struct {
	Threads []*chatThread `json:"threads"`
}

type threadStore struct {
	mu   sync.Mutex
	path string
	data *threadData
}

func newThreadStore(dataDir string) *threadStore {
	ts := &threadStore{
		path: filepath.Join(dataDir, "threads.json"),
		data: &threadData{Threads: []*chatThread{}},
	}
	data, err := os.ReadFile(ts.path)
	if err == nil {
		_ = json.Unmarshal(data, ts.data)
	}
	if ts.data.Threads == nil {
		ts.data.Threads = []*chatThread{}
	}
	return ts
}

func (ts *threadStore) save() error {
	data, err := json.MarshalIndent(ts.data, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ts.path, data, 0600)
}

// createThread persists a new thread and returns it.
func (ts *threadStore) createThread(creator, sessionID, parentMessageID, checkpointID, selectedText string) *chatThread {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := time.Now()
	thread := &chatThread{
		ThreadID:         "t_" + fmt.Sprintf("%d", now.UnixNano()),
		Creator:          creator,
		ParentSessionID:  sessionID,
		ParentMessageID:  parentMessageID,
		BaseCheckpointID: checkpointID,
		SelectedText:     selectedText,
		CreatedAt:        now.Format(time.RFC3339),
		UpdatedAt:        now.Format(time.RFC3339),
		Messages:         []map[string]interface{}{},
	}
	ts.data.Threads = append(ts.data.Threads, thread)
	_ = ts.save()
	return thread
}

func (ts *threadStore) getThread(id string) *chatThread {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.data.Threads {
		if t.ThreadID == id {
			return t
		}
	}
	return nil
}

// listThreadsBySession returns the threads of a parent session (ordered by
// creation time, mirroring get_webchat_threads_by_parent_session).
func (ts *threadStore) listThreadsBySession(sessionID string) []map[string]interface{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]map[string]interface{}, 0)
	for _, t := range ts.data.Threads {
		if t.ParentSessionID != sessionID {
			continue
		}
		out = append(out, serializeThread(t))
	}
	return out
}

// deleteThread removes a thread by id.
func (ts *threadStore) deleteThread(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, t := range ts.data.Threads {
		if t.ThreadID == id {
			ts.data.Threads = append(ts.data.Threads[:i], ts.data.Threads[i+1:]...)
			_ = ts.save()
			return true
		}
	}
	return false
}

// appendThreadMessage appends a message to a thread's history.
func (ts *threadStore) appendThreadMessage(id string, msg map[string]interface{}) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.data.Threads {
		if t.ThreadID == id {
			t.Messages = append(t.Messages, msg)
			t.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = ts.save()
			return true
		}
	}
	return false
}

// threadDetail mirrors Python chat_service.get_thread: thread + history +
// running flags.
func (ts *threadStore) threadDetail(id string) map[string]interface{} {
	t := ts.getThread(id)
	if t == nil {
		return nil
	}
	ts.mu.Lock()
	messages := make([]map[string]interface{}, len(t.Messages))
	copy(messages, t.Messages)
	ts.mu.Unlock()
	return map[string]interface{}{
		"thread":      serializeThread(t),
		"history":     messages,
		"is_running":  false,
		"active_runs": []interface{}{},
	}
}

// serializeThread mirrors Python serialize_thread.
func serializeThread(t *chatThread) map[string]interface{} {
	return map[string]interface{}{
		"thread_id":          t.ThreadID,
		"parent_session_id":  t.ParentSessionID,
		"parent_message_id":  t.ParentMessageID,
		"base_checkpoint_id": t.BaseCheckpointID,
		"selected_text":      t.SelectedText,
		"created_at":         t.CreatedAt,
		"updated_at":         t.UpdatedAt,
	}
}

// ── Chat projects store ────────────────────────────────────────

// chatProject mirrors the Python chatui_projects row (creator/title/emoji/
// description/workspace_type/workspace_path) plus the joined session ids.
type chatProject struct {
	ProjectID     string   `json:"project_id"`
	Creator       string   `json:"creator"`
	Title         string   `json:"title"`
	Emoji         string   `json:"emoji"`
	Description   string   `json:"description"`
	WorkspaceType string   `json:"workspace_type"` // session | project | custom
	WorkspacePath string   `json:"workspace_path"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	SessionIDs    []string `json:"session_ids"`
}

type projectData struct {
	Projects []*chatProject `json:"projects"`
}

type projectStore struct {
	mu   sync.Mutex
	path string
	data *projectData
}

func newProjectStore(dataDir string) *projectStore {
	ps := &projectStore{
		path: filepath.Join(dataDir, "projects.json"),
		data: &projectData{Projects: []*chatProject{}},
	}
	data, err := os.ReadFile(ps.path)
	if err == nil {
		_ = json.Unmarshal(data, ps.data)
	}
	if ps.data.Projects == nil {
		ps.data.Projects = []*chatProject{}
	}
	return ps
}

func (ps *projectStore) save() error {
	data, err := json.MarshalIndent(ps.data, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ps.path, data, 0600)
}

// createProject persists a new project (workspace_type normalized to
// session/project/custom, mirroring normalize_project_workspace_type).
func (ps *projectStore) createProject(creator, title, emoji, description, workspaceType, workspacePath string) *chatProject {
	if workspaceType != "session" && workspaceType != "project" && workspaceType != "custom" {
		workspaceType = "session"
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	project := &chatProject{
		ProjectID:     "p_" + fmt.Sprintf("%d", now.UnixNano()),
		Creator:       creator,
		Title:         title,
		Emoji:         emoji,
		Description:   description,
		WorkspaceType: workspaceType,
		WorkspacePath: workspacePath,
		CreatedAt:     now.Format(time.RFC3339),
		UpdatedAt:     now.Format(time.RFC3339),
		SessionIDs:    []string{},
	}
	ps.data.Projects = append(ps.data.Projects, project)
	_ = ps.save()
	return project
}

func (ps *projectStore) getProject(id string) *chatProject {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Projects {
		if p.ProjectID == id {
			return p
		}
	}
	return nil
}

func (ps *projectStore) listProjectsByCreator(creator string) []map[string]interface{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]map[string]interface{}, 0)
	for _, p := range ps.data.Projects {
		if creator != "" && p.Creator != creator {
			continue
		}
		out = append(out, serializeProject(p))
	}
	return out
}

// updateProject patches title/emoji/description/workspace fields.
func (ps *projectStore) updateProject(id string, patch map[string]interface{}) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Projects {
		if p.ProjectID != id {
			continue
		}
		if v, ok := patch["title"].(string); ok {
			p.Title = v
		}
		if v, ok := patch["emoji"].(string); ok {
			p.Emoji = v
		}
		if v, ok := patch["description"].(string); ok {
			p.Description = v
		}
		if v, ok := patch["workspace_type"].(string); ok {
			if v == "session" || v == "project" || v == "custom" {
				p.WorkspaceType = v
			}
		}
		if v, ok := patch["workspace_path"].(string); ok {
			p.WorkspacePath = v
		}
		p.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = ps.save()
		return true
	}
	return false
}

func (ps *projectStore) deleteProject(id string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, p := range ps.data.Projects {
		if p.ProjectID == id {
			ps.data.Projects = append(ps.data.Projects[:i], ps.data.Projects[i+1:]...)
			_ = ps.save()
			return true
		}
	}
	return false
}

// addSessionToProject / removeSessionFromProject manage session membership.
func (ps *projectStore) addSessionToProject(projectID, sessionID string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Projects {
		if p.ProjectID != projectID {
			continue
		}
		for _, s := range p.SessionIDs {
			if s == sessionID {
				return true
			}
		}
		p.SessionIDs = append(p.SessionIDs, sessionID)
		p.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = ps.save()
		return true
	}
	return false
}

func (ps *projectStore) removeSessionFromProject(sessionID string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	removed := false
	for _, p := range ps.data.Projects {
		next := p.SessionIDs[:0]
		for _, s := range p.SessionIDs {
			if s != sessionID {
				next = append(next, s)
			}
		}
		if len(next) != len(p.SessionIDs) {
			p.SessionIDs = next
			p.UpdatedAt = time.Now().Format(time.RFC3339)
			removed = true
		}
	}
	if removed {
		_ = ps.save()
	}
	return removed
}

func (ps *projectStore) projectSessionIDs(projectID string) []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Projects {
		if p.ProjectID == projectID {
			out := make([]string, len(p.SessionIDs))
			copy(out, p.SessionIDs)
			return out
		}
	}
	return nil
}

// projectBySession returns the project a session belongs to (Python
// get_project_by_session), or nil.
func (ps *projectStore) projectBySession(sessionID string) *chatProject {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.data.Projects {
		for _, s := range p.SessionIDs {
			if s == sessionID {
				return p
			}
		}
	}
	return nil
}

// serializeProject mirrors Python chatui_project_service._serialize_project.
// resolved_workspace_path is filled by the caller (needs the data dir).
func serializeProject(p *chatProject) map[string]interface{} {
	return map[string]interface{}{
		"project_id":              p.ProjectID,
		"title":                   p.Title,
		"emoji":                   p.Emoji,
		"description":             p.Description,
		"workspace_type":          p.WorkspaceType,
		"workspace_path":          p.WorkspacePath,
		"resolved_workspace_path": nil,
		"created_at":              p.CreatedAt,
		"updated_at":              p.UpdatedAt,
	}
}

// ── API key store ─────────────────────────────────────────────

// apiKeyRecord 是 api_key 的持久化记录（data/api_keys.json）。明文 key 只在
// 创建时返回，落盘仅存 PBKDF2 哈希（对齐 Python api_key_service.hash_key）。
type apiKeyRecord struct {
	KeyID      string   `json:"key_id"`
	Name       string   `json:"name"`
	KeyHash    string   `json:"key_hash"`
	KeyPrefix  string   `json:"key_prefix"`
	Scopes     []string `json:"scopes"`
	CreatedBy  string   `json:"created_by"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
}

type apiKeyData struct {
	Keys []*apiKeyRecord `json:"keys"`
}

type apiKeyStore struct {
	mu   sync.Mutex
	path string
	data *apiKeyData
}

func newAPIKeyStore(dataDir string) *apiKeyStore {
	ks := &apiKeyStore{
		path: filepath.Join(dataDir, "api_keys.json"),
		data: &apiKeyData{Keys: []*apiKeyRecord{}},
	}
	data, err := os.ReadFile(ks.path)
	if err == nil {
		_ = json.Unmarshal(data, ks.data)
	}
	if ks.data.Keys == nil {
		ks.data.Keys = []*apiKeyRecord{}
	}
	return ks
}

func (ks *apiKeyStore) save() error {
	data, err := json.MarshalIndent(ks.data, "", "  ")
	if err != nil {
		return err
	}
	// 0600：key 哈希/元数据敏感，仅当前用户可读。
	return writeFileAtomic(ks.path, data, 0600)
}

func (ks *apiKeyStore) list() []*apiKeyRecord {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	out := make([]*apiKeyRecord, 0, len(ks.data.Keys))
	for _, k := range ks.data.Keys {
		cp := *k
		out = append(out, &cp)
	}
	return out
}

func (ks *apiKeyStore) get(keyID string) *apiKeyRecord {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.data.Keys {
		if k.KeyID == keyID {
			cp := *k
			return &cp
		}
	}
	return nil
}

func (ks *apiKeyStore) insert(rec *apiKeyRecord) {
	ks.mu.Lock()
	ks.data.Keys = append(ks.data.Keys, rec)
	_ = ks.save()
	ks.mu.Unlock()
}

// revoke marks a key revoked (kept for audit, mirrors Python revoke_api_key).
func (ks *apiKeyStore) revoke(keyID string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.data.Keys {
		if k.KeyID == keyID {
			k.RevokedAt = time.Now().Format(time.RFC3339)
			k.UpdatedAt = k.RevokedAt
			_ = ks.save()
			return true
		}
	}
	return false
}

func (ks *apiKeyStore) delete(keyID string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for i, k := range ks.data.Keys {
		if k.KeyID == keyID {
			ks.data.Keys = append(ks.data.Keys[:i], ks.data.Keys[i+1:]...)
			_ = ks.save()
			return true
		}
	}
	return false
}
