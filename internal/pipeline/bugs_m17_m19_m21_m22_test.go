package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
)

// TestExecuteFutureTaskEditUpdatesInPlace: future_task edit mutates the job
// through the thread-safe UpdateJob path and persists the changes (M-17).
func TestExecuteFutureTaskEditUpdatesInPlace(t *testing.T) {
	m := cron.NewCronJobManager(nil)
	m.RegisterHandler("active_agent", func(ctx context.Context, j *cron.Job) error { return nil })

	created := executeFutureTask(m, "qq:group:1", "u1", map[string]interface{}{
		"action": "create", "name": "t", "cron_expression": "0 8 * * *", "note": "old",
	})
	if !strings.Contains(created, "task created: job_id=") {
		t.Fatalf("create failed: %q", created)
	}
	id := strings.TrimPrefix(strings.Split(created, "job_id=")[1], "job_")
	id = "job_" + strings.TrimSpace(id[:strings.Index(id, " ")])

	updated := executeFutureTask(m, "qq:group:1", "u1", map[string]interface{}{
		"action": "edit", "job_id": id, "note": "new", "cron_expression": "0 9 * * *",
	})
	if !strings.Contains(updated, "task updated: job_id="+id) {
		t.Fatalf("edit failed: %q", updated)
	}
	job := m.Get(id)
	if job == nil {
		t.Fatal("job must still exist after edit")
	}
	if job.Payload["note"] != "new" {
		t.Fatalf("expected note=new, got %v", job.Payload["note"])
	}
	if job.Description != "new" {
		t.Fatalf("expected description=new, got %q", job.Description)
	}
	if job.CronExpression != "0 9 * * *" {
		t.Fatalf("expected cron=0 9 * * *, got %q", job.CronExpression)
	}
	if job.RunOnce {
		t.Fatal("editing cron must clear run_once")
	}
	if job.NextRun.IsZero() {
		t.Fatalf("NextRun must be recomputed after edit, got %v", job.NextRun)
	}
}

// TestExecuteFutureTaskEditMissingJob: editing an unknown job reports an error
// and never creates a new job (M-17).
func TestExecuteFutureTaskEditMissingJob(t *testing.T) {
	m := cron.NewCronJobManager(nil)
	got := executeFutureTask(m, "qq:group:1", "u1", map[string]interface{}{
		"action": "edit", "job_id": "job_missing", "note": "x",
	})
	if !strings.Contains(got, "task not found") {
		t.Fatalf("expected task not found, got %q", got)
	}
}

// TestExecuteToolRuntimeNoneBlocksHostTools: with computer_use_runtime=none the
// host shell/python/file executors must not run even if the model emits the
// tool name directly (M-19).
func TestExecuteToolRuntimeNoneBlocksHostTools(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"}}
	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"astrbot_execute_shell", map[string]interface{}{"command": "echo hi"}},
		{"astrbot_shell_session", map[string]interface{}{"action": "list"}},
		{"astrbot_execute_python", map[string]interface{}{"code": "print(1)"}},
		{"astrbot_file_read_tool", map[string]interface{}{"path": "/etc/hostname"}},
		{"astrbot_grep_tool", map[string]interface{}{"pattern": ".", "path": "/etc"}},
	} {
		result := s.executeTool(context.Background(), event, "none", tc.name, tc.args)
		if !strings.Contains(result, "未启用") {
			t.Fatalf("runtime=none: %s must not execute on the host, got %q", tc.name, result)
		}
	}
}

// TestExecuteToolRuntimeLocalRunsShell: with the local runtime the host shell
// still works (M-19 must not regress the local path).
func TestExecuteToolRuntimeLocalRunsShell(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	event := &core.Event{Source: core.EventSource{Platform: "qq", ConvID: "group:1", SenderID: "u1"}}
	result := s.executeTool(context.Background(), event, "local", "astrbot_execute_shell", map[string]interface{}{"command": "echo hello"})
	if strings.Contains(result, "未启用") {
		t.Fatalf("runtime=local must run the host shell, got %q", result)
	}
	if !strings.Contains(result, "hello") {
		t.Fatalf("expected shell output, got %q", result)
	}
}

// TestResolveProviderCopiesSettings: writes to the returned provider_settings
// (e.g. persona) must not mutate the shared config (M-21a).
func TestResolveProviderCopiesSettings(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider": []interface{}{
			map[string]interface{}{"id": "p1", "type": "openai_chat_completion", "enable": true},
		},
		"provider_settings": map[string]interface{}{"existing": "v"},
	})
	_, settings, err := s.resolveProvider()
	if err != nil {
		t.Fatalf("resolveProvider: %v", err)
	}
	if settings["existing"] != "v" {
		t.Fatalf("settings should carry existing keys, got %v", settings)
	}
	settings["persona"] = "poisoned"
	shared, _ := s.config["provider_settings"].(map[string]interface{})
	if _, ok := shared["persona"]; ok {
		t.Fatal("mutating the returned settings must not touch the shared config")
	}
}

// TestConversationHistoryDeepCopy: the history snapshot handed to the LLM must
// not alias the conversation's live History (M-21b).
func TestConversationHistoryDeepCopy(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{})
	cm := conversation.NewManager(nil)
	s.convMgr = cm
	cm.AppendHistory("qq:group:1", "user", "hello")
	hist := s.conversationHistory("qq:group:1")
	if len(hist) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(hist))
	}
	hist[0]["content"] = "changed"
	conv := cm.GetConversation("qq:group:1")
	if conv == nil || len(conv.History) != 1 {
		t.Fatal("conversation missing after append")
	}
	if got := conv.History[0]["content"]; got != "hello" {
		t.Fatalf("history must be deep-copied, live entry mutated to %v", got)
	}
}

// TestSnapshotFileMutationDetectsPatch: the pre-execution tree hash captures the
// patch produced by a file-mutating tool, while a hash taken after the mutation
// (the old ordering) reports nothing (M-22).
func TestSnapshotFileMutationDetectsPatch(t *testing.T) {
	ws := t.TempDir()
	before := gitTreeHash(ws)
	if before == "" {
		t.Skip("git is not available")
	}
	if got := snapshotFileMutation(ws, before, "write", "ok"); got != "ok" {
		t.Fatalf("no mutation must not attach a patch, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := snapshotFileMutation(ws, before, "write", "ok")
	if !strings.Contains(got, "工作区快照") {
		t.Fatalf("expected a workspace snapshot patch note, got %q", got)
	}
	// A hash captured after the mutation must report no patch (the no-op bug).
	after := gitTreeHash(ws)
	if got := snapshotFileMutation(ws, after, "write", "ok"); got != "ok" {
		t.Fatalf("post-mutation hash must not report a patch, got %q", got)
	}
}
