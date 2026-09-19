package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
)

func namesOf(schemas []map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	for _, sc := range schemas {
		fn, _ := sc["function"].(map[string]interface{})
		if n, ok := fn["name"].(string); ok {
			out[n] = true
		}
	}
	return out
}

func TestNeoToolExposure(t *testing.T) {
	base := namesOf(collectComputerTools(true, false, false))
	if base["astrbot_execute_browser"] || base["astrbot_get_execution_history"] {
		t.Fatal("browser/neo tools must not be exposed when caps absent")
	}
	browserOnly := namesOf(collectComputerTools(true, true, false))
	if !browserOnly["astrbot_execute_browser"] || !browserOnly["astrbot_run_browser_skill"] {
		t.Fatal("browser tools must be exposed when browser cap present")
	}
	if browserOnly["astrbot_create_skill_payload"] {
		t.Fatal("neo lifecycle tools must stay hidden when neo flag false")
	}
	all := namesOf(collectComputerTools(true, true, true))
	for _, n := range []string{"astrbot_execute_browser_batch", "astrbot_get_execution_history", "astrbot_annotate_execution", "astrbot_create_skill_payload", "astrbot_get_skill_payload", "astrbot_create_skill_candidate", "astrbot_list_skill_candidates", "astrbot_evaluate_skill_candidate", "astrbot_promote_skill_candidate", "astrbot_list_skill_releases", "astrbot_rollback_skill_release", "astrbot_sync_skill_release"} {
		if !all[n] {
			t.Fatalf("expected tool %s exposed for neo sandbox", n)
		}
	}
}

func TestComputerAdminDenied(t *testing.T) {
	member := &core.Event{}
	member.Role = "member"
	s := &ProcessStage{}
	if msg := s.computerAdminDenied(member, "Shell execution"); !strings.Contains(msg, "only allowed for admin") {
		t.Fatalf("member with default require_admin must be denied, got %q", msg)
	}
	admin := &core.Event{}
	admin.Role = "admin"
	if msg := s.computerAdminDenied(admin, "Shell execution"); msg != "" {
		t.Fatalf("admin must pass, got %q", msg)
	}
	tr := true
	s.providerConf = &ProviderSettings{ComputerUseRequireAdmin: &tr}
	if msg := s.computerAdminDenied(admin, "Shell execution"); msg != "" {
		t.Fatalf("admin still passes with require_admin=true, got %q", msg)
	}
}

func TestBrowserBodyDefaults(t *testing.T) {
	b := browserBody(map[string]interface{}{"cmd": "open https://example.com"}, map[string]interface{}{"timeout": 30, "learn": false, "include_trace": false})
	if b["cmd"] != "open https://example.com" || b["timeout"] != 30 {
		t.Fatalf("body = %v", b)
	}
	if _, ok := b["description"]; ok {
		t.Fatal("empty optional strings must be omitted")
	}
	if b["learn"] != false || b["include_trace"] != false {
		t.Fatalf("learn/include_trace must always be sent: %v", b)
	}
	batch := browserBody(map[string]interface{}{"commands": []interface{}{"a", " ", "b"}}, map[string]interface{}{"timeout": 60, "stop_on_error": true})
	cmds, _ := batch["commands"].([]string)
	if len(cmds) != 2 || cmds[0] != "a" || cmds[1] != "b" {
		t.Fatalf("commands must drop blanks: %v", batch["commands"])
	}
	if batch["stop_on_error"] != true {
		t.Fatal("stop_on_error default must survive")
	}
}

func TestNeoLifecycleEndToEnd(t *testing.T) {
	s := &ProcessStage{neoStore: skills.NewNeoStore(t.TempDir())}
	payload := map[string]interface{}{"payload": map[string]interface{}{"skill_markdown": "# demo skill"}, "kind": "skill"}
	res := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_create_skill_payload", payload)
	var pr map[string]interface{}
	if err := json.Unmarshal([]byte(res), &pr); err != nil {
		t.Fatalf("payload result not JSON: %q", res)
	}
	ref, _ := pr["payload_ref"].(string)
	if ref == "" {
		t.Fatalf("missing payload_ref: %v", pr)
	}
	got := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_get_skill_payload", map[string]interface{}{"payload_ref": ref})
	if !strings.Contains(got, "demo skill") {
		t.Fatalf("get_skill_payload mismatch: %q", got)
	}
	cand := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_create_skill_candidate", map[string]interface{}{
		"skill_key": "demo", "payload_ref": ref, "source_execution_ids": []interface{}{"e1"},
	})
	var cm map[string]interface{}
	if err := json.Unmarshal([]byte(cand), &cm); err != nil {
		t.Fatalf("candidate result not JSON: %q", cand)
	}
	cid, _ := cm["id"].(string)
	if cid == "" {
		t.Fatalf("missing candidate id: %v", cm)
	}
	ev := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_evaluate_skill_candidate", map[string]interface{}{"candidate_id": cid, "passed": true, "score": 0.9})
	if strings.HasPrefix(ev, "Error") {
		t.Fatalf("evaluate failed: %q", ev)
	}
	prom := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_promote_skill_candidate", map[string]interface{}{"candidate_id": cid, "stage": "canary"})
	if strings.HasPrefix(prom, "Error") {
		t.Fatalf("promote failed: %q", prom)
	}
	rel := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_list_skill_releases", map[string]interface{}{"skill_key": "demo"})
	if !strings.Contains(rel, "demo") {
		t.Fatalf("list releases mismatch: %q", rel)
	}
	lst := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_list_skill_candidates", map[string]interface{}{})
	if !strings.Contains(lst, cid) {
		t.Fatalf("list candidates mismatch: %q", lst)
	}
	if msg := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_promote_skill_candidate", map[string]interface{}{"candidate_id": "nope"}); !strings.HasPrefix(msg, "Error") {
		t.Fatalf("unknown candidate must error, got %q", msg)
	}
}

func TestNeoLifecycleNoStore(t *testing.T) {
	s := &ProcessStage{}
	msg := s.executeNeoLifecycleTool(context.Background(), "g:1", "astrbot_create_skill_payload", map[string]interface{}{"payload": map[string]interface{}{}})
	if !strings.Contains(msg, "not initialized") {
		t.Fatalf("no-store guard mismatch: %q", msg)
	}
}
