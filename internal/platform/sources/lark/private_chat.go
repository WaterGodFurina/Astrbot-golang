package lark

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
)

// larkPrivateChatMapFile 是私聊 open_id → chat_id 路由的持久化文件名，位于宿主
// 数据目录（如 data/lark_private_chat_map.json）。对齐 py v4.28.2（commit
// 32a75139）用 shared_preferences 持久化 "private_chat:{open_id}" 的语义；
// Go 侧以 dataDir 下的 JSON 文件承载（按适配器作用域 "{id}:{appid}" 分桶）。
//
// 若数据目录不可读写（无法创建/写入文件），则降级为仅内存：本次运行仍可回退
// 重发，重启后映射丢失（与 py sp 不可用时的降级语义一致）。
const larkPrivateChatMapFile = "lark_private_chat_map.json"

// larkPrivateChatFileMu 串行化同一进程内多个 Lark 适配器实例对映射文件的
// 读-改-写，避免相互覆盖（逐作用域合并 + 原子写）。
var larkPrivateChatFileMu sync.Mutex

// privateChatScope 返回当前适配器的持久化作用域，对齐 py
// f"{self.meta().id}:{self.appid}"，避免不同 bot/平台实例互相污染。
func (a *Adapter) privateChatScope() string {
	return fmt.Sprintf("%s:%s", a.ID(), a.appID)
}

// privateChatKey 返回 open_id 对应的存储键（对齐 py "private_chat:{open_id}"）。
func privateChatKey(openID string) string {
	return "private_chat:" + openID
}

// loadPrivateChatRoutes 从数据目录加载当前作用域的 open_id → chat_id 映射。
// 文件不存在/解析失败/目录不可用时静默降级（仅内存），不影响适配器启动。
func (a *Adapter) loadPrivateChatRoutes() {
	path := utils.DataPath(larkPrivateChatMapFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(data, &all); err != nil {
		logger.Warn("[Lark] 解析私聊路由映射失败，降级为仅内存: %v", err)
		return
	}
	routes := all[a.privateChatScope()]
	if len(routes) == 0 {
		return
	}
	a.privateChatMu.Lock()
	for k, v := range routes {
		a.privateChat[k] = v
	}
	a.privateChatMu.Unlock()
}

// savePrivateChatRoute 记录 open_id → chat_id 映射并尽力持久化（对齐 py
// convert_msg 的 sp.put_async，失败仅告警并保留内存映射）。
func (a *Adapter) savePrivateChatRoute(openID, chatID string) {
	if openID == "" || chatID == "" {
		return
	}
	a.privateChatMu.Lock()
	a.privateChat[privateChatKey(openID)] = chatID
	a.privateChatMu.Unlock()
	if err := a.persistPrivateChatRoutes(); err != nil {
		logger.Warn("[Lark] 保存私聊路由失败，降级为仅内存: %v", err)
	}
}

// persistPrivateChatRoutes 将当前适配器作用域的映射原子写入 dataDir。写入前
// 重新读取文件并合并其它作用域，避免多个适配器实例互相覆盖。
func (a *Adapter) persistPrivateChatRoutes() error {
	a.privateChatMu.RLock()
	local := make(map[string]string, len(a.privateChat))
	for k, v := range a.privateChat {
		local[k] = v
	}
	a.privateChatMu.RUnlock()

	larkPrivateChatFileMu.Lock()
	defer larkPrivateChatFileMu.Unlock()

	path := utils.DataPath(larkPrivateChatMapFile)
	all := map[string]map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	scope := a.privateChatScope()
	merged := map[string]string{}
	for k, v := range all[scope] {
		merged[k] = v
	}
	for k, v := range local {
		merged[k] = v
	}
	all[scope] = merged
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return writeLarkDataFileAtomic(path, data, 0o600)
}

// lookupPrivateChatID 查询私聊 open_id 对应的 chat_id（没有则返回空串）。
func (a *Adapter) lookupPrivateChatID(openID string) string {
	if openID == "" {
		return ""
	}
	a.privateChatMu.RLock()
	defer a.privateChatMu.RUnlock()
	return a.privateChat[privateChatKey(openID)]
}

// writeLarkDataFileAtomic 将 data 原子地写入 path（临时文件 + fsync + rename），
// 防止崩溃或并发读时读到半截 JSON（与 dashboard.writeFileAtomic 同策略）。
func writeLarkDataFileAtomic(path string, data []byte, perm os.FileMode) error {
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
	if err := tmp.Chmod(perm); err != nil {
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
