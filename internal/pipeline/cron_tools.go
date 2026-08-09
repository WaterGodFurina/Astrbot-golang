package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/cron"
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
				"Use action='delete' to remove a task by job_id.",
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

// executeFutureTask implements the future_task tool.
func executeFutureTask(mgr *cron.CronJobManager, umo, senderID string, args map[string]interface{}) string {
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
		if len(jobs) == 0 {
			return "no future tasks."
		}
		var lines []string
		for _, j := range jobs {
			next := ""
			if !j.NextRun.IsZero() {
				next = j.NextRun.Format(time.RFC3339)
			}
			lines = append(lines, fmt.Sprintf("- job_id=%s name=%q cron=%q run_once=%v next_run=%s note=%q",
				j.ID, j.Name, j.CronExpression, j.RunOnce, next, j.Payload["note"]))
		}
		return strings.Join(lines, "\n")
	case "delete":
		jobID := argString(args, "job_id")
		if jobID == "" {
			return "error: job_id is required for action=delete."
		}
		mgr.Remove(jobID)
		return fmt.Sprintf("task deleted: job_id=%s", jobID)
	case "edit":
		jobID := argString(args, "job_id")
		if jobID == "" {
			return "error: job_id is required for action=edit."
		}
		job := mgr.Get(jobID)
		if job == nil {
			return "error: task not found: " + jobID
		}
		// Recreate with the same identity: simplest correct edit is to update
		// the note/cron and re-arm.
		changed := false
		if n := strings.TrimSpace(argString(args, "note")); n != "" {
			job.Payload["note"] = n
			job.Description = n
			changed = true
		}
		if e := argString(args, "cron_expression"); e != "" {
			job.CronExpression = e
			job.RunOnce = false
			changed = true
		}
		if r := argString(args, "run_at"); r != "" {
			if t, err := time.Parse(time.RFC3339, r); err == nil {
				job.RunAt = t
				job.RunOnce = true
				changed = true
			}
		}
		if changed {
			mgr.Add(job) // re-arms + persists
		}
		return fmt.Sprintf("task updated: job_id=%s", jobID)
	default:
		return fmt.Sprintf("error: unknown action %q. Valid actions: create, edit, delete, list.", action)
	}
}
