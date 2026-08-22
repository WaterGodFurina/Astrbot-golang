package kook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// 角色缓存相关常量 (对应 Python kook_roles_record.py)。
const (
	userViewRequestTimeout = 3 * time.Second // 请求超时, 避免阻塞消息接收太久
	rolesCacheMaxSize      = 2000            // 缓存最大条目数 (LRU)
	maxRetryTimes          = 3               // 最大失败重试次数
	retryIntervalSecond    = 60              // 失败后的重试间隔(秒)
)

// rolesCacheEntry 对应 Python 的 RolesCache 数据类。
type rolesCacheEntry struct {
	Roles            map[int64]bool // 角色 id 集合, nil 表示获取失败
	FailedCount      int
	LatestUpdateTime time.Time
}

// update 更新缓存内容 (对应 Python RolesCache.update)。
func (e *rolesCacheEntry) update(roles map[int64]bool) {
	if roles != nil {
		e.FailedCount = 0
	}
	e.Roles = roles
	e.LatestUpdateTime = time.Now()
}

// addFailed 记录一次失败 (对应 Python RolesCache.add_failed)。
func (e *rolesCacheEntry) addFailed() {
	e.FailedCount++
}

// pendingFetch 用于去重同一频道的并发查询 (对应 Python 的 _pending_tasks)。
type pendingFetch struct {
	done  chan struct{}
	roles map[int64]bool
}

// RolesRecord 自动缓存机器人所需响应的消息频道的 role 信息。
// 对应 Python kook_roles_record.py 的 KookRolesRecord。
type RolesRecord struct {
	mu sync.Mutex

	botID string
	token string

	httpClient *http.Client

	cache   map[int64]*rolesCacheEntry // 频道 id -> 缓存 (LRU)
	pending map[int64]*pendingFetch    // 频道 id -> 进行中的查询
	order   []int64                    // LRU 顺序 (队尾为最近使用)
}

// NewRolesRecord 创建角色记录缓存。
func NewRolesRecord(httpClient *http.Client) *RolesRecord {
	return &RolesRecord{
		httpClient: httpClient,
		cache:      make(map[int64]*rolesCacheEntry),
		pending:    make(map[int64]*pendingFetch),
	}
}

// SetBotID 设置机器人账号 id (对应 Python set_bot_id)。
func (r *RolesRecord) SetBotID(botID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.botID = botID
}

// SetToken 设置机器人 token (请求 /user/view 时携带 Authorization: Bot <token>)。
func (r *RolesRecord) SetToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token = token
}

// ClearGuildRolesCache 清除指定频道的角色缓存 (对应 Python clear_guild_roles_cache)。
func (r *RolesRecord) ClearGuildRolesCache(guildID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, guildID)
	for i, id := range r.order {
		if id == guildID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	// 进行中的查询不做取消 (与 Python 一致), 完成后结果自然丢弃
	delete(r.pending, guildID)
}

// moveToEnd 将频道 id 移动到 LRU 队尾 (最近使用)。
func (r *RolesRecord) moveToEnd(guildID int64) {
	for i, id := range r.order {
		if id == guildID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.order = append(r.order, guildID)
}

// fetchRolesByGuildID 调用 /user/view 接口获取机器人在指定频道的角色 id 集合。
// 由于需要判断 bot 账号是否属于某个角色才会回复消息, 而同一个频道的消息在首次
// 查询时会阻塞消息接收, 所以这里特意调低了超时时间, 避免阻塞太久。
func (r *RolesRecord) fetchRolesByGuildID(ctx context.Context, guildID int64) map[int64]bool {
	url := fmt.Sprintf("%s?guild_id=%d&user_id=%s", apiUserView, guildID, r.botID)
	reqCtx, cancel := context.WithTimeout(ctx, userViewRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		logger.I18nError("[KOOK] 获取机器人在频道 %q 的角色id信息时请求异常: %v", fmt.Sprintf("%d", guildID), err)
		return nil
	}
	req.Header.Set("Authorization", "Bot "+r.token)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		logger.I18nError("[KOOK] 获取机器人在频道 %q 的角色id信息时请求异常: %v", fmt.Sprintf("%d", guildID), err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.I18nError("[KOOK] 获取机器人在频道 %q 的角色id信息失败，状态码: %d", fmt.Sprintf("%d", guildID), resp.StatusCode)
		return nil
	}
	var apiResp struct {
		Code int              `json:"code"`
		Data kookUserViewData `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		logger.I18nError("[KOOK] 获取机器人在频道 %q 的角色id信息失败, 响应数据格式错误: %v", fmt.Sprintf("%d", guildID), err)
		return nil
	}
	if apiResp.Code != 0 {
		logger.I18nError("[KOOK] 获取机器人在频道 %q 的角色id信息失败", fmt.Sprintf("%d", guildID))
		return nil
	}
	logger.I18nInfo("[KOOK] 获取机器人在频道 %q 的角色id成功", fmt.Sprintf("%d", guildID))
	roles := make(map[int64]bool, len(apiResp.Data.Roles))
	for _, id := range apiResp.Data.Roles {
		roles[id] = true
	}
	return roles
}

// HasRoleInChannel 判断机器人是否拥有频道中的指定角色。
// 对应 Python 的 has_role_in_channel (带缓存 + 同频道并发去重)。
func (r *RolesRecord) HasRoleInChannel(ctx context.Context, roleID, guildID int64) bool {
	r.mu.Lock()
	if cache, ok := r.cache[guildID]; ok {
		r.moveToEnd(guildID)
		if cache.Roles != nil {
			result := cache.Roles[roleID]
			r.mu.Unlock()
			return result
		}
	}
	// 若同一频道已有查询在进行中, 直接等待其结果
	if p, ok := r.pending[guildID]; ok {
		r.mu.Unlock()
		select {
		case <-p.done:
			if p.roles == nil {
				return false
			}
			return p.roles[roleID]
		case <-ctx.Done():
			return false
		}
	}
	p := &pendingFetch{done: make(chan struct{})}
	r.pending[guildID] = p
	r.mu.Unlock()

	roles := r.doFetch(ctx, guildID, roleID)

	r.mu.Lock()
	delete(r.pending, guildID)
	p.roles = roles
	close(p.done)
	r.mu.Unlock()
	if roles == nil {
		return false
	}
	return roles[roleID]
}

// doFetch 执行实际的查询并维护缓存 (对应 Python has_role_in_channel 中的缓存逻辑)。
func (r *RolesRecord) doFetch(ctx context.Context, guildID, roleID int64) map[int64]bool {
	r.mu.Lock()
	if cache, ok := r.cache[guildID]; ok {
		// 失败次数过多且在重试间隔内, 直接放弃本次查询
		if cache.FailedCount > maxRetryTimes && time.Since(cache.LatestUpdateTime) < retryIntervalSecond {
			r.mu.Unlock()
			return nil
		}
	}
	// 简单的容量控制 (LRU)
	if len(r.cache)+1 > rolesCacheMaxSize && len(r.order) > 0 {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.cache, oldest)
	}
	r.mu.Unlock()

	roles := r.fetchRolesByGuildID(ctx, guildID)

	r.mu.Lock()
	defer r.mu.Unlock()
	var cache *rolesCacheEntry
	if existing, ok := r.cache[guildID]; ok {
		cache = existing
		cache.update(roles)
		r.moveToEnd(guildID)
	} else {
		cache = &rolesCacheEntry{Roles: roles, LatestUpdateTime: time.Now()}
		r.cache[guildID] = cache
		r.order = append(r.order, guildID)
	}
	if roles == nil {
		cache.addFailed()
	}
	return roles
}
