// Neo 技能生命周期存储。
//
// 本体（AstrBot-py）的 dashboard Neo API（skills_service.py 的
// get_neo_candidates / get_neo_releases / get_neo_payload / evaluate /
// promote / rollback / sync / delete_*）全部通过 BayClient 转发到沙盒内
// shipyard-neo 服务，dashboard 自身不持久化。宿主侧没有沙盒 Neo 服务，
// 因此按 shipyard-neo SDK（shipyard_neo/types.py）的数据模型做等价本地
// 存储：data/neo_skills/ 下的 JSON 文件，字段结构与 SDK 模型一一对应。
//
// 操作语义对齐 dashboard/services/skills_service.py + core/skills/
// neo_skill_sync.py（promote 可选同步本地 SKILL.md、sync 读写
// neo_skill_map.json 等）。
package skills

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Neo 候选/发布状态与阶段（对齐 shipyard_neo.types.SkillCandidateStatus /
// SkillReleaseStage 枚举值）。
const (
	NeoStatusDraft          = "draft"
	NeoStatusEvaluating     = "evaluating"
	NeoStatusPromoted       = "promoted"
	NeoStatusPromotedCanary = "promoted_canary"
	NeoStatusPromotedStable = "promoted_stable"
	NeoStatusRejected       = "rejected"
	NeoStatusRolledBack     = "rolled_back"

	NeoStageCanary = "canary"
	NeoStageStable = "stable"

	neoMapVersion    = 1
	neoMapFilename   = "neo_skill_map.json"
	neoDefaultSkill  = "code"
	neoReleaseManual = "manual"
)

// NeoCandidate 对齐 shipyard_neo.types.SkillCandidateInfo。
type NeoCandidate struct {
	ID                  string                 `json:"id"`
	SkillKey            string                 `json:"skill_key"`
	ScenarioKey         string                 `json:"scenario_key,omitempty"`
	PayloadRef          string                 `json:"payload_ref,omitempty"`
	SkillType           string                 `json:"skill_type"`
	AutoReleaseEligible bool                   `json:"auto_release_eligible"`
	AutoReleaseReason   string                 `json:"auto_release_reason,omitempty"`
	Summary             string                 `json:"summary,omitempty"`
	UsageNotes          string                 `json:"usage_notes,omitempty"`
	Preconditions       map[string]interface{} `json:"preconditions,omitempty"`
	Postconditions      map[string]interface{} `json:"postconditions,omitempty"`
	SourceExecutionIDs  []string               `json:"source_execution_ids"`
	Status              string                 `json:"status"`
	LatestScore         *float64               `json:"latest_score"`
	LatestPass          *bool                  `json:"latest_pass"`
	LastEvaluatedAt     string                 `json:"last_evaluated_at,omitempty"`
	PromotionReleaseID  string                 `json:"promotion_release_id,omitempty"`
	CreatedBy           string                 `json:"created_by,omitempty"`
	CreatedAt           string                 `json:"created_at"`
	UpdatedAt           string                 `json:"updated_at"`
	IsDeleted           bool                   `json:"is_deleted"`
	DeletedAt           string                 `json:"deleted_at,omitempty"`
	DeletedBy           string                 `json:"deleted_by,omitempty"`
	DeleteReason        string                 `json:"delete_reason,omitempty"`
}

// NeoRelease 对齐 shipyard_neo.types.SkillReleaseInfo。
type NeoRelease struct {
	ID                 string `json:"id"`
	SkillKey           string `json:"skill_key"`
	CandidateID        string `json:"candidate_id"`
	Version            int    `json:"version"`
	Stage              string `json:"stage"`
	IsActive           bool   `json:"is_active"`
	ReleaseMode        string `json:"release_mode"`
	PromotedBy         string `json:"promoted_by,omitempty"`
	PromotedAt         string `json:"promoted_at"`
	RollbackOf         string `json:"rollback_of,omitempty"`
	AutoPromotedFrom   string `json:"auto_promoted_from,omitempty"`
	HealthWindowEndAt  string `json:"health_window_end_at,omitempty"`
	UpgradeOfReleaseID string `json:"upgrade_of_release_id,omitempty"`
	UpgradeReason      string `json:"upgrade_reason,omitempty"`
	ChangeSummary      string `json:"change_summary,omitempty"`
	IsDeleted          bool   `json:"is_deleted"`
	DeletedAt          string `json:"deleted_at,omitempty"`
	DeletedBy          string `json:"deleted_by,omitempty"`
	DeleteReason       string `json:"delete_reason,omitempty"`
}

// NeoEvaluation 对齐 shipyard_neo.types.SkillEvaluationInfo。
type NeoEvaluation struct {
	ID          string   `json:"id"`
	CandidateID string   `json:"candidate_id"`
	BenchmarkID string   `json:"benchmark_id,omitempty"`
	Score       *float64 `json:"score"`
	Passed      bool     `json:"passed"`
	Report      string   `json:"report,omitempty"`
	EvaluatedBy string   `json:"evaluated_by,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

// NeoPayload 对齐 shipyard_neo.types.SkillPayloadInfo（含 create 响应的
// payload_ref/kind 两字段）。
type NeoPayload struct {
	PayloadRef string      `json:"payload_ref"`
	Kind       string      `json:"kind"`
	Payload    interface{} `json:"payload"`
}

// neoPayloadFile 是 payloads/<ref>.json 的落盘结构。
type neoPayloadFile struct {
	PayloadRef string      `json:"payload_ref"`
	Kind       string      `json:"kind"`
	Payload    interface{} `json:"payload"`
	CreatedAt  string      `json:"created_at"`
}

// neoMapItem / neoMap 对齐本体 neo_skill_map.json
// （core/skills/neo_skill_sync.py 的 _load_map/_save_map）。
type neoMapItem struct {
	LocalSkillName    string `json:"local_skill_name"`
	LatestReleaseID   string `json:"latest_release_id"`
	LatestCandidateID string `json:"latest_candidate_id"`
	LatestPayloadRef  string `json:"latest_payload_ref"`
	UpdatedAt         string `json:"updated_at"`
}

type neoMap struct {
	Version int                   `json:"version"`
	Items   map[string]neoMapItem `json:"items"`
}

type neoCandidateData struct {
	Items []*NeoCandidate `json:"items"`
}

type neoReleaseData struct {
	Items []*NeoRelease `json:"items"`
}

// NeoStore 是 Neo 技能生命周期的宿主侧等价存储。所有方法互斥锁串行 +
// writeFileAtomic 原子落盘。
type NeoStore struct {
	mu         sync.Mutex
	dir        string // <data>/neo_skills
	skillsRoot string // <data>/skills（sync 的本地写入目标）
	skillMgr   *SkillManager
}

// NewNeoStore creates the store rooted at <dataDir>/neo_skills.
func NewNeoStore(dataDir string) *NeoStore {
	dir := filepath.Join(dataDir, "neo_skills")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "payloads"), 0o755)
	return &NeoStore{
		dir:        dir,
		skillsRoot: filepath.Join(dataDir, "skills"),
	}
}

// SetSkillManager 注入 SkillManager（sync 本地 SKILL.md 后据此激活技能）。
func (ns *NeoStore) SetSkillManager(mgr *SkillManager) {
	ns.mu.Lock()
	ns.skillMgr = mgr
	ns.mu.Unlock()
}

// neoNowISO 对齐本体 _now_iso 的 UTC ISO 时间戳。
func neoNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// neoRandomID 生成 <prefix>_<hex12> 形态的 id。
func neoRandomID(prefix string) string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return prefix + "_" + hex.EncodeToString(buf)
}

// ── 磁盘读写（调用方必须持有 ns.mu）──────────────────────────────

func (ns *NeoStore) readCandidatesLocked() []*NeoCandidate {
	path := filepath.Join(ns.dir, "candidates.json")
	data, err := os.ReadFile(path) // #nosec G304 -- path 由本包固定拼接
	if err != nil {
		return []*NeoCandidate{}
	}
	var parsed neoCandidateData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return []*NeoCandidate{}
	}
	if parsed.Items == nil {
		return []*NeoCandidate{}
	}
	return parsed.Items
}

func (ns *NeoStore) saveCandidatesLocked(items []*NeoCandidate) error {
	data, err := json.MarshalIndent(neoCandidateData{Items: items}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicNeo(filepath.Join(ns.dir, "candidates.json"), data)
}

func (ns *NeoStore) readReleasesLocked() []*NeoRelease {
	path := filepath.Join(ns.dir, "releases.json")
	data, err := os.ReadFile(path) // #nosec G304 -- path 由本包固定拼接
	if err != nil {
		return []*NeoRelease{}
	}
	var parsed neoReleaseData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return []*NeoRelease{}
	}
	if parsed.Items == nil {
		return []*NeoRelease{}
	}
	return parsed.Items
}

func (ns *NeoStore) saveReleasesLocked(items []*NeoRelease) error {
	data, err := json.MarshalIndent(neoReleaseData{Items: items}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicNeo(filepath.Join(ns.dir, "releases.json"), data)
}

// ── Candidates ──────────────────────────────────────────────────

// ListCandidates 返回候选列表（{items, total}，对齐 SkillCandidateList）。
// 过滤软删记录；status / skill_key 为空表示不过滤。
func (ns *NeoStore) ListCandidates(status, skillKey string, limit, offset int) map[string]interface{} {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readCandidatesLocked()
	items := make([]*NeoCandidate, 0)
	for _, c := range all {
		if c.IsDeleted {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		if skillKey != "" && c.SkillKey != skillKey {
			continue
		}
		items = append(items, c)
	}
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return map[string]interface{}{
		"items": items[offset:end],
		"total": total,
	}
}

// AddCandidate 创建候选（对齐 client.skills.create_candidate 的必填字段
// skill_key + source_execution_ids，可选 scenario_key/payload_ref）。
func (ns *NeoStore) AddCandidate(skillKey string, sourceExecutionIDs []string, scenarioKey, payloadRef, summary, usageNotes string) (*NeoCandidate, error) {
	if strings.TrimSpace(skillKey) == "" {
		return nil, fmt.Errorf("Missing skill_key")
	}
	if len(sourceExecutionIDs) == 0 {
		return nil, fmt.Errorf("Missing source_execution_ids")
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readCandidatesLocked()
	now := neoNowISO()
	c := &NeoCandidate{
		ID:                 neoRandomID("cand"),
		SkillKey:           skillKey,
		ScenarioKey:        scenarioKey,
		PayloadRef:         payloadRef,
		SkillType:          neoDefaultSkill,
		SourceExecutionIDs: sourceExecutionIDs,
		Status:             NeoStatusDraft,
		CreatedAt:          now,
		UpdatedAt:          now,
		Summary:            summary,
		UsageNotes:         usageNotes,
	}
	all = append(all, c)
	if err := ns.saveCandidatesLocked(all); err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteCandidate 软删候选（对齐 delete_candidate 的 is_deleted 语义）。
func (ns *NeoStore) DeleteCandidate(candidateID, reason string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readCandidatesLocked()
	for _, c := range all {
		if c.ID == candidateID && !c.IsDeleted {
			c.IsDeleted = true
			c.DeletedAt = neoNowISO()
			c.DeleteReason = reason
			c.UpdatedAt = c.DeletedAt
			return ns.saveCandidatesLocked(all)
		}
	}
	return fmt.Errorf("Candidate not found: %s", candidateID)
}

// EvaluateCandidate 评估候选：写入评估记录并回填候选的
// latest_score/latest_pass/last_evaluated_at（对齐 evaluate_candidate）。
// 返回 SkillEvaluationInfo 形态的 dict。
func (ns *NeoStore) EvaluateCandidate(candidateID string, passed bool, score *float64, benchmarkID, report string) (map[string]interface{}, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readCandidatesLocked()
	var target *NeoCandidate
	for _, c := range all {
		if c.ID == candidateID && !c.IsDeleted {
			target = c
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("Candidate not found: %s", candidateID)
	}
	now := neoNowISO()
	evaluation := &NeoEvaluation{
		ID:          neoRandomID("eval"),
		CandidateID: candidateID,
		BenchmarkID: benchmarkID,
		Score:       score,
		Passed:      passed,
		Report:      report,
		CreatedAt:   now,
	}
	target.LatestScore = score
	target.LatestPass = &passed
	target.LastEvaluatedAt = now
	target.UpdatedAt = now
	if err := ns.saveCandidatesLocked(all); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":           evaluation.ID,
		"candidate_id": evaluation.CandidateID,
		"benchmark_id": evaluation.BenchmarkID,
		"score":        evaluation.Score,
		"passed":       evaluation.Passed,
		"report":       evaluation.Report,
		"created_at":   evaluation.CreatedAt,
	}, nil
}

// ── Releases ────────────────────────────────────────────────────

// ListReleases 返回发布列表（{items, total}，对齐 SkillReleaseList）。
func (ns *NeoStore) ListReleases(skillKey, stage string, activeOnly bool, limit, offset int) map[string]interface{} {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readReleasesLocked()
	items := make([]*NeoRelease, 0)
	for _, r := range all {
		if r.IsDeleted {
			continue
		}
		if activeOnly && !r.IsActive {
			continue
		}
		if stage != "" && r.Stage != stage {
			continue
		}
		if skillKey != "" && r.SkillKey != skillKey {
			continue
		}
		items = append(items, r)
	}
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return map[string]interface{}{
		"items": items[offset:end],
		"total": total,
	}
}

// nextReleaseVersionLocked 返回某 skill_key 当前最大 version + 1。
func nextReleaseVersionLocked(all []*NeoRelease, skillKey string) int {
	max := 0
	for _, r := range all {
		if r.SkillKey == skillKey && r.Version > max {
			max = r.Version
		}
	}
	return max + 1
}

// deactivateReleasesLocked 停用某 skill_key + stage 下所有 active 发布。
func deactivateReleasesLocked(all []*NeoRelease, skillKey, stage string) {
	for _, r := range all {
		if r.SkillKey == skillKey && r.Stage == stage && r.IsActive {
			r.IsActive = false
		}
	}
}

// PromoteCandidate 将候选提升为 canary/stable 发布（对齐
// promote_candidate + promote_with_optional_sync）：stage=stable 且
// sync_to_local=true 时同步本地 SKILL.md；同步失败则自动回滚。
// 返回 {release, sync, rollback, sync_error}（sync/rollback 可为 nil）。
func (ns *NeoStore) PromoteCandidate(candidateID, stage string, syncToLocal bool) (map[string]interface{}, error) {
	if stage == "" {
		stage = NeoStageCanary
	}
	if stage != NeoStageCanary && stage != NeoStageStable {
		return nil, fmt.Errorf("Invalid stage, must be canary/stable")
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	cands := ns.readCandidatesLocked()
	var target *NeoCandidate
	for _, c := range cands {
		if c.ID == candidateID && !c.IsDeleted {
			target = c
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("Candidate not found: %s", candidateID)
	}

	rels := ns.readReleasesLocked()
	now := neoNowISO()
	release := &NeoRelease{
		ID:          neoRandomID("rel"),
		SkillKey:    target.SkillKey,
		CandidateID: target.ID,
		Version:     nextReleaseVersionLocked(rels, target.SkillKey),
		Stage:       stage,
		IsActive:    true,
		ReleaseMode: neoReleaseManual,
		PromotedAt:  now,
	}
	// 同 skill_key 同 stage 只保留一个 active 发布（active_only 查询语义）。
	deactivateReleasesLocked(rels, target.SkillKey, stage)
	rels = append(rels, release)

	target.Status = NeoStatusPromoted
	if stage == NeoStageCanary {
		target.Status = NeoStatusPromotedCanary
	} else {
		target.Status = NeoStatusPromotedStable
	}
	target.PromotionReleaseID = release.ID
	target.UpdatedAt = now

	if err := ns.saveReleasesLocked(rels); err != nil {
		return nil, err
	}
	if err := ns.saveCandidatesLocked(cands); err != nil {
		return nil, err
	}

	// 对齐 promote_with_optional_sync：stable + sync_to_local 时同步本地，
	// 失败自动回滚该发布。
	syncJSON := map[string]interface{}(nil)
	var rollbackJSON interface{}
	var syncErr string
	if stage == NeoStageStable && syncToLocal {
		syncResult, err := ns.syncReleaseLocked(release.ID, true)
		if err != nil {
			syncErr = err.Error()
			rb, rbErr := ns.rollbackReleaseLocked(release.ID)
			if rbErr != nil {
				return nil, fmt.Errorf(
					"stable release synced failed and auto rollback also failed; sync_error=%s; rollback_error=%s",
					syncErr, rbErr.Error())
			}
			rollbackJSON = rb
		} else {
			syncJSON = syncResult
		}
	}

	result := map[string]interface{}{
		"release": release,
		"sync":    syncJSON,
	}
	if rollbackJSON != nil {
		result["rollback"] = rollbackJSON
	}
	if syncErr != "" {
		result["sync_error"] = syncErr
	}
	return result, nil
}

// RollbackRelease 回滚一个发布：停用目标发布并生成一条指向相同候选的
// 回滚发布（rollback_of=目标 id，version 递增），同 skill_key 同 stage 的
// 其它 active 发布一并停用；对应候选状态置为 rolled_back。
func (ns *NeoStore) RollbackRelease(releaseID string) (*NeoRelease, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.rollbackReleaseLocked(releaseID)
}

func (ns *NeoStore) rollbackReleaseLocked(releaseID string) (*NeoRelease, error) {
	rels := ns.readReleasesLocked()
	var target *NeoRelease
	for _, r := range rels {
		if r.ID == releaseID && !r.IsDeleted {
			target = r
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("Release not found: %s", releaseID)
	}
	now := neoNowISO()
	rollback := &NeoRelease{
		ID:          neoRandomID("rel"),
		SkillKey:    target.SkillKey,
		CandidateID: target.CandidateID,
		Version:     nextReleaseVersionLocked(rels, target.SkillKey),
		Stage:       target.Stage,
		IsActive:    true,
		ReleaseMode: neoReleaseManual,
		PromotedAt:  now,
		RollbackOf:  target.ID,
	}
	deactivateReleasesLocked(rels, target.SkillKey, target.Stage)
	rels = append(rels, rollback)
	if err := ns.saveReleasesLocked(rels); err != nil {
		return nil, err
	}
	// 候选状态置为 rolled_back（对齐 SkillCandidateStatus 枚举）。
	cands := ns.readCandidatesLocked()
	for _, c := range cands {
		if c.ID == target.CandidateID {
			c.Status = NeoStatusRolledBack
			c.UpdatedAt = now
		}
	}
	if err := ns.saveCandidatesLocked(cands); err != nil {
		return nil, err
	}
	return rollback, nil
}

// DeleteRelease 软删发布。
func (ns *NeoStore) DeleteRelease(releaseID, reason string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	all := ns.readReleasesLocked()
	for _, r := range all {
		if r.ID == releaseID && !r.IsDeleted {
			r.IsDeleted = true
			r.DeletedAt = neoNowISO()
			r.DeleteReason = reason
			return ns.saveReleasesLocked(all)
		}
	}
	return fmt.Errorf("Release not found: %s", releaseID)
}

// ── Payload ─────────────────────────────────────────────────────

// PutPayload 存储不可变 payload 内容并返回 payload_ref（对齐
// create_payload：只存内容，不创建候选/发布）。
func (ns *NeoStore) PutPayload(payload interface{}, kind string) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("Missing payload")
	}
	if kind == "" {
		kind = "generic"
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ref := neoRandomID("pl")
	file := neoPayloadFile{
		PayloadRef: ref,
		Kind:       kind,
		Payload:    payload,
		CreatedAt:  neoNowISO(),
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(ns.dir, "payloads", ref+".json")
	if err := writeFileAtomicNeo(path, data); err != nil {
		return nil, err
	}
	return map[string]interface{}{"payload_ref": ref, "kind": kind}, nil
}

// GetPayload 读取 payload（对齐 get_payload：返回 {payload_ref, kind,
// payload}）。
func (ns *NeoStore) GetPayload(payloadRef string) (map[string]interface{}, error) {
	if payloadRef == "" {
		return nil, fmt.Errorf("Missing payload_ref")
	}
	// 防穿越：ref 只允许安全字符。
	if strings.ContainsAny(payloadRef, "/\\.") {
		return nil, fmt.Errorf("Invalid payload_ref")
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	path := filepath.Join(ns.dir, "payloads", payloadRef+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- ref 已拒绝路径分隔符
	if err != nil {
		return nil, fmt.Errorf("Payload not found: %s", payloadRef)
	}
	var file neoPayloadFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("Payload not found: %s", payloadRef)
	}
	return map[string]interface{}{
		"payload_ref": file.PayloadRef,
		"kind":        file.Kind,
		"payload":     file.Payload,
	}, nil
}

// ── Sync（对齐 core/skills/neo_skill_sync.py）────────────────────

var neoSkillNameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// NeoNormalizeSkillName 对齐 NeoSkillSyncManager.normalize_skill_name。
func NeoNormalizeSkillName(skillKey string) string {
	normalized := neoSkillNameRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(skillKey)), "-")
	normalized = strings.Trim(normalized, "._-")
	if normalized == "" {
		normalized = "skill"
	}
	return "neo_" + normalized
}

// neoReadMap / neoWriteMap 对齐 _load_map/_save_map。
func (ns *NeoStore) neoReadMapLocked() *neoMap {
	path := filepath.Join(ns.dir, neoMapFilename)
	data, err := os.ReadFile(path) // #nosec G304 -- path 由本包固定拼接
	if err != nil {
		return &neoMap{Version: neoMapVersion, Items: map[string]neoMapItem{}}
	}
	var m neoMap
	if err := json.Unmarshal(data, &m); err != nil || m.Items == nil {
		return &neoMap{Version: neoMapVersion, Items: map[string]neoMapItem{}}
	}
	if m.Version < 1 {
		m.Version = neoMapVersion
	}
	return &m
}

func (ns *NeoStore) neoWriteMapLocked(m *neoMap) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicNeo(filepath.Join(ns.dir, neoMapFilename), data)
}

// neoResolveLocalSkillName 对齐 _resolve_local_skill_name：优先复用映射中
// 已有的本地名，冲突时追加 sha1(skillKey)[:8] 后缀。
func (ns *NeoStore) neoResolveLocalSkillNameLocked(m *neoMap, skillKey string) string {
	if item, ok := m.Items[skillKey]; ok && item.LocalSkillName != "" {
		return item.LocalSkillName
	}
	base := NeoNormalizeSkillName(skillKey)
	for _, item := range m.Items {
		if item.LocalSkillName == base {
			suffix := sha1.Sum([]byte(skillKey))
			return fmt.Sprintf("%s-%x", base, suffix[:4])
		}
	}
	return base
}

// neoEnsureSkillFrontmatter 对齐 _ensure_skill_frontmatter：确保 SKILL.md
// 带 name/description frontmatter（description 缺省取正文首个非标题行）。
func neoEnsureSkillFrontmatter(markdown, skillName, skillKey string) string {
	name, description, body := neoParseFrontmatter(markdown)
	if name == "" {
		name = skillName
	}
	if description == "" {
		description = neoDeriveDescription(body)
	}
	if description == "" {
		description = fmt.Sprintf("Synced skill for `%s`.", skillKey)
	}
	trimmed := strings.TrimLeft(strings.TrimRight(body, "\n"), "\n")
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, trimmed)
}

// neoParseFrontmatter 解析 --- 包裹的 name/description。
func neoParseFrontmatter(markdown string) (name, description, body string) {
	if !strings.HasPrefix(markdown, "---") {
		return "", "", markdown
	}
	lines := strings.Split(markdown, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", markdown
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", markdown
	}
	for _, line := range lines[1:end] {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
		switch key {
		case "name":
			if value != "" && name == "" {
				name = value
			}
		case "description":
			if value != "" && description == "" {
				description = value
			}
		}
	}
	rest := lines[end+1:]
	// 去掉开头的空行（对齐 lstrip("\n") 的效果）。
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	return name, description, strings.Join(rest, "\n")
}

// neoDeriveDescription 对齐 _derive_description：优先 "## 描述/描述" 小节
// 首行，否则取正文首个非标题非空行。
func neoDeriveDescription(body string) string {
	lines := strings.Split(body, "\n")
	headingIdx := -1
	for i, line := range lines {
		normalized := strings.ToLower(strings.TrimSpace(line))
		if normalized == "## 描述" || normalized == "## description" {
			headingIdx = i
			break
		}
	}
	if headingIdx >= 0 {
		for _, line := range lines[headingIdx+1:] {
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			if strings.HasPrefix(text, "#") {
				break
			}
			return text
		}
	}
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		return text
	}
	return ""
}

// SyncRelease 把 stable 发布的 payload.skill_markdown 同步为本地
// SKILL.md 并更新映射（对齐 NeoSkillSyncManager.sync_release）。返回
// {skill_key, local_skill_name, release_id, candidate_id, payload_ref,
// map_path, synced_at}。
func (ns *NeoStore) SyncRelease(releaseID, skillKey string, requireStable bool) (map[string]interface{}, error) {
	if releaseID == "" && skillKey == "" {
		return nil, fmt.Errorf("release_id or skill_key is required for sync.")
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.syncReleaseLocked(releaseID, requireStable)
}

func (ns *NeoStore) syncReleaseLocked(releaseID string, requireStable bool) (map[string]interface{}, error) {
	rels := ns.readReleasesLocked()
	var release *NeoRelease
	if releaseID != "" {
		for _, r := range rels {
			if r.ID == releaseID && !r.IsDeleted {
				release = r
				break
			}
		}
		if release == nil {
			return nil, fmt.Errorf("Release not found: %s", releaseID)
		}
	} else {
		return nil, fmt.Errorf("release_id or skill_key is required for sync.")
	}
	if release.ID == "" || release.SkillKey == "" || release.CandidateID == "" {
		return nil, fmt.Errorf("Release payload is incomplete.")
	}
	if requireStable && release.Stage != NeoStageStable {
		return nil, fmt.Errorf(
			"Only stable releases can be synced to local SKILL.md (got: %s).", release.Stage)
	}

	cands := ns.readCandidatesLocked()
	var candidate *NeoCandidate
	for _, c := range cands {
		if c.ID == release.CandidateID {
			candidate = c
			break
		}
	}
	if candidate == nil || candidate.PayloadRef == "" {
		return nil, fmt.Errorf("Candidate payload_ref is missing.")
	}
	payloadResp, err := ns.GetPayload(candidate.PayloadRef)
	if err != nil {
		return nil, err
	}
	payload, ok := payloadResp["payload"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Skill payload must be a JSON object.")
	}
	markdown, _ := payload["skill_markdown"].(string)
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("payload.skill_markdown is required for stable sync to local skill.")
	}

	mapping := ns.neoReadMapLocked()
	localName := ns.neoResolveLocalSkillNameLocked(mapping, release.SkillKey)
	skillDir := filepath.Join(ns.skillsRoot, localName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return nil, err
	}
	finalMarkdown := neoEnsureSkillFrontmatter(markdown, localName, release.SkillKey)
	if err := writeFileAtomicNeo(filepath.Join(skillDir, "SKILL.md"), []byte(finalMarkdown)); err != nil {
		return nil, err
	}

	now := neoNowISO()
	mapping.Version = neoMapVersion
	mapping.Items[release.SkillKey] = neoMapItem{
		LocalSkillName:    localName,
		LatestReleaseID:   release.ID,
		LatestCandidateID: release.CandidateID,
		LatestPayloadRef:  candidate.PayloadRef,
		UpdatedAt:         now,
	}
	if err := ns.neoWriteMapLocked(mapping); err != nil {
		return nil, err
	}
	// 本地技能对技能管理器可见（对齐 SkillManager().set_skill_active(name, true)）。
	// SkillManager 内部有自己的锁，与 NeoStore.mu 无循环依赖，持锁调用安全。
	if ns.skillMgr != nil {
		_ = ns.skillMgr.SetSkillActive(localName, true)
	}

	return map[string]interface{}{
		"skill_key":        release.SkillKey,
		"local_skill_name": localName,
		"release_id":       release.ID,
		"candidate_id":     release.CandidateID,
		"payload_ref":      candidate.PayloadRef,
		"map_path":         filepath.Join(ns.dir, neoMapFilename),
		"synced_at":        now,
	}, nil
}

// writeFileAtomicNeo 原子写文件（临时文件 + rename），perm 0644。
func writeFileAtomicNeo(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
