package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
)

// futureTaskToolSchema mirrors Python's FutureTaskTool schema
// (astrbot/core/tools/cron_tools.py).
func futureTaskToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "future_task",
			"description": "Manage your future tasks. " +
				"Use action='create' to schedule a recurring cron task or one-time run_at task. " +
				"Use action='edit' to update an existing task. " +
				"Use action='list' to inspect existing tasks. " +
				"Use action='delete' to remove a task by job_id. " +
				"Group members can list all tasks in the current group, including their creators. " +
				"Only task creators can edit tasks. Group members can delete only their own tasks, " +
				"while AstrBot admins can also delete other members' tasks in the current group.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{
						"type":        "string",
						"enum":        []interface{}{"create", "edit", "delete", "list"},
						"description": "Action to perform. 'list' takes no parameters. 'delete' requires only 'job_id'. 'edit' requires 'job_id' plus the fields to change.",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional task label.",
					},
					"cron_expression": map[string]interface{}{
						"type":        "string",
						"description": "Cron expression for a recurring schedule, e.g. '0 8 * * *' or '0 23 * * mon-fri'.",
					},
					"note": map[string]interface{}{
						"type":        "string",
						"description": "Detailed instructions for your future agent to execute when it wakes.",
					},
					"run_once": map[string]interface{}{
						"type":        "boolean",
						"description": "Run only once and delete after execution. Use with run_at.",
					},
					"run_at": map[string]interface{}{
						"type":        "string",
						"description": "ISO datetime for one-time execution, e.g. 2026-02-02T08:00:00+08:00.",
					},
					"job_id": map[string]interface{}{
						"type":        "string",
						"description": "Task ID. Required for 'delete' and 'edit'.",
					},
				},
				"required": []interface{}{"action"},
			},
		},
	}
}

// executeFutureTask implements the future_task tool. isGroup/isAdmin drive the
// 4.29.4 group-sharing rules: group members see every task in the current
// group; only creators edit; admins may delete others' tasks in the group.
func executeFutureTask(mgr *cron.CronJobManager, umo, senderID string, isGroup, isAdmin bool, args map[string]interface{}) string {
	if mgr == nil {
		return "error: cron manager is not available."
	}
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	switch action {
	case "create":
		cronExpr := argString(args, "cron_expression")
		runAt := argString(args, "run_at")
		runOnce := argBool(args, "run_once")
		note := strings.TrimSpace(argString(args, "note"))
		name := strings.TrimSpace(argString(args, "name"))
		if name == "" {
			name = "active_agent_task"
		}
		if note == "" {
			return "error: note is required when action=create."
		}
		if runOnce && runAt == "" {
			return "error: run_at is required when run_once=true."
		}
		if !runOnce && cronExpr == "" {
			return "error: cron_expression is required when run_once=false."
		}
		if runOnce {
			cronExpr = ""
		}
		var runAtTime time.Time
		if runAt != "" {
			t, err := cron.ParseRunAt(runAt)
			if err != nil {
				return "error: run_at must be ISO datetime, e.g., 2026-02-02T08:00:00+08:00"
			}
			runAtTime = t
			runOnce = true
		}
		if runAtTime.IsZero() && cronExpr == "" {
			return "error: cron_expression is required when run_once=false."
		}
		payload := map[string]interface{}{
			"session":   umo,
			"sender_id": senderID,
			"note":      note,
			"origin":    "tool",
		}
		if runAt != "" {
			payload["run_at"] = runAt
		}
		job, err := mgr.AddActiveJob(name, cronExpr, payload, note, "", runOnce, runAtTime)
		if err != nil {
			return "error: failed to schedule task: " + err.Error()
		}
		next := job.NextRun
		if next.IsZero() {
			return fmt.Sprintf("task created: job_id=%s name=%q", job.ID, name)
		}
		return fmt.Sprintf("task created: job_id=%s name=%q next_run=%s", job.ID, name, next.Format(time.RFC3339))
	case "list":
		jobs := mgr.List()
		var lines []string
		for _, j := range jobs {
			// 私有会话仍按发送者隔离；群聊内可列出本会话的全部任务（含创建者），
			// 对齐 py 4.29.4。
			if !taskOwnedBy(j, umo, senderID) && !(isGroup && jobSession(j) == umo) {
				continue
			}
			next := ""
			if !j.NextRun.IsZero() {
				next = j.NextRun.Format(time.RFC3339)
			}
			line := fmt.Sprintf("- job_id=%s name=%q cron=%q run_once=%v next_run=%s note=%q",
				j.ID, j.Name, j.CronExpression, j.RunOnce, next, j.Payload["note"])
			if isGroup {
				creator := jobSender(j)
				if creator == "" {
					creator = "unknown"
				}
				line += fmt.Sprintf(" creator=%s", creator)
			}
			lines = append(lines, line)
		}
		// 私有会话隐藏他人任务时给出说明，避免模型把“被过滤”误判为“任务不存在”。
		hiddenNote := ""
		if !isGroup {
			for _, j := range jobs {
				if jobSession(j) != umo {
					continue
				}
				if taskOwnedBy(j, umo, senderID) {
					continue
				}
				hiddenNote = "\n\nNote: tasks in this chat that were not created by you are not listed here. Only their creators can edit or delete them."
				break
			}
		}
		if len(lines) == 0 {
			return "No cron jobs found." + hiddenNote
		}
		return strings.Join(lines, "\n") + hiddenNote
	case "delete":
		jobID := argString(args, "job_id")
		if jobID == "" {
			return "error: job_id is required for action=delete."
		}
		job := mgr.Get(jobID)
		if job == nil {
			return fmt.Sprintf("error: cron job %s not found.", jobID)
		}
		sameSession := jobSession(job) == umo
		canDelete := taskOwnedBy(job, umo, senderID) || (sameSession && isGroup && isAdmin)
		if !canDelete {
			if sameSession && jobSender(job) == "" {
				// Dashboard / legacy 行有会话但无成员创建者，归咎他人不正确。
				return fmt.Sprintf("error: cron job %s cannot be deleted through the tool.", jobID)
			}
			if sameSession && isGroup {
				return fmt.Sprintf("error: cron job %s was created by another member of this group chat, so you cannot delete it. Only the member who created it or an AstrBot admin can delete it; tell the user to ask that member.", jobID)
			}
			return fmt.Sprintf("error: cron job %s was not created by you, so you cannot delete it.", jobID)
		}
		mgr.Remove(jobID)
		return fmt.Sprintf("task deleted: job_id=%s", jobID)
	case "edit":
		jobID := argString(args, "job_id")
		if jobID == "" {
			return "error: job_id is required for action=edit."
		}
		// 归属校验：只能修改任务创建者本人的任务（4.29.0 细分错误文案）。
		job := mgr.Get(jobID)
		if job == nil {
			return "error: task not found: " + jobID
		}
		if !taskOwnedBy(job, umo, senderID) {
			sameSession := jobSession(job) == umo
			if sameSession && jobSender(job) == "" {
				return fmt.Sprintf("error: cron job %s has no chat member as its creator (it was created outside this chat, e.g. from the dashboard), so you cannot edit it here.", jobID)
			}
			if sameSession && isGroup {
				return fmt.Sprintf("error: cron job %s was created by another member of this group chat, so you cannot edit it. Only the member who created it can edit it; tell the user to ask that member.", jobID)
			}
			return fmt.Sprintf("error: cron job %s was not created by you, so you cannot edit it. Only whoever created it can edit it.", jobID)
		}
		note := strings.TrimSpace(argString(args, "note"))
		cronExpr := argString(args, "cron_expression")
		runAt := argString(args, "run_at")
		if note == "" && cronExpr == "" && runAt == "" {
			return "error: nothing to update for task " + jobID
		}
		var runAtTime time.Time
		if runAt != "" {
			t, err := time.Parse(time.RFC3339, runAt)
			if err != nil {
				return "error: run_at must be ISO datetime, e.g., 2026-02-02T08:00:00+08:00"
			}
			runAtTime = t
		}
		if cronExpr != "" {
			if _, err := cron.ParseCron(cronExpr); err != nil {
				return "error: invalid cron_expression (use 5 or 6 fields, e.g. '0 8 * * *'): " + err.Error()
			}
		}
		// Mutate under the manager lock so the edit never races the cron tick
		// goroutine or concurrent List/Serialize reads (M-17). The manager
		// re-arms and persists the job and recomputes its NextRun.
		updated := mgr.UpdateJob(jobID, func(job *cron.Job) {
			if note != "" {
				job.Payload["note"] = note
				job.Description = note
			}
			if cronExpr != "" {
				job.CronExpression = cronExpr
				job.RunOnce = false
			}
			if runAt != "" {
				job.RunAt = runAtTime
				job.RunOnce = true
				job.Payload["run_at"] = runAt
			}
		})
		if !updated {
			return "error: task not found: " + jobID
		}
		return fmt.Sprintf("task updated: job_id=%s", jobID)
	default:
		return fmt.Sprintf("error: unknown action %q. Valid actions: create, edit, delete, list.", action)
	}
}

// taskOwnedBy reports whether a cron job was created by the given user within
// the given session. Jobs created through the future_task tool record both in
// their payload; other jobs (system-scheduled, from other sessions) are not
// manageable through the tool.
func taskOwnedBy(job *cron.Job, umo, senderID string) bool {
	if job == nil {
		return false
	}
	session, _ := job.Payload["session"].(string)
	sender, _ := job.Payload["sender_id"].(string)
	return session == umo && sender == senderID
}

// jobSession returns the session recorded for a cron job ("" when absent).
func jobSession(job *cron.Job) string {
	if job == nil {
		return ""
	}
	s, _ := job.Payload["session"].(string)
	return s
}

// jobSender returns the creator sender id recorded for a cron job ("" when absent).
func jobSender(job *cron.Job) string {
	if job == nil {
		return ""
	}
	s, _ := job.Payload["sender_id"].(string)
	return s
}
