package pipeline

import (
	"container/list"
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// GroupChatContext implements the group-chat context awareness feature
// (provider_ltm_settings): in-context group message records injected into
// LLM requests, image captioning, and probabilistic active replies.
// Ported 1:1 from astrbot/builtin_stars/astrbot/group_chat_context.py.
type GroupChatContext struct {
	config map[string]interface{}

	mu         sync.Mutex
	locks      map[string]*sync.Mutex
	rawRecords map[string]*list.List // umo -> records (front = oldest)
	recordIDs  map[string]*list.List // umo -> record ids
}

// NewGroupChatContext creates a group chat context tracker.
func NewGroupChatContext(config map[string]interface{}) *GroupChatContext {
	return &GroupChatContext{
		config:     config,
		locks:      make(map[string]*sync.Mutex),
		rawRecords: make(map[string]*list.List),
		recordIDs:  make(map[string]*list.List),
	}
}

// groupContextCfg resolves provider_ltm_settings for a session (cfg()).
type groupContextCfg struct {
	groupMessageMaxCnt     int
	imageCaption           bool
	imageCaptionPrompt     string
	imageCaptionProviderID string
	enableActiveReply      bool
	arMethod               string
	arPossibility          float64
	arPrompt               string
	arWhitelist            []string
}

func (g *GroupChatContext) getLock(umo string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.locks[umo]
	if l == nil {
		l = &sync.Mutex{}
		g.locks[umo] = l
	}
	return l
}

func positiveInt(v interface{}, def int) int {
	switch n := v.(type) {
	case int:
		if n > 0 {
			return n
		}
	case float64:
		if n > 0 {
			return int(n)
		}
	}
	return def
}

func stringSlice(v interface{}) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// cfg resolves the per-session LTM settings (mirrors GroupChatContext.cfg()).
func (g *GroupChatContext) cfg(umo string) groupContextCfg {
	settings := map[string]interface{}{}
	if ps, ok := g.config["provider_ltm_settings"].(map[string]interface{}); ok {
		settings = ps
	}
	providerSettings := map[string]interface{}{}
	if ps, ok := g.config["provider_settings"].(map[string]interface{}); ok {
		providerSettings = ps
	}

	c := groupContextCfg{
		groupMessageMaxCnt: positiveInt(settings["group_message_max_cnt"], 1000),
		arMethod:           "possibility_reply",
		arPossibility:      0.1,
	}
	imageCaptionProviderID, _ := settings["image_caption_provider_id"].(string)
	imageCaption, _ := settings["image_caption"].(bool)
	c.imageCaption = imageCaption && imageCaptionProviderID != ""
	c.imageCaptionProviderID = imageCaptionProviderID
	c.imageCaptionPrompt, _ = providerSettings["image_caption_prompt"].(string)

	if ar, ok := settings["active_reply"].(map[string]interface{}); ok {
		c.enableActiveReply, _ = ar["enable"].(bool)
		if m, ok := ar["method"].(string); ok && m != "" {
			c.arMethod = m
		}
		if p, ok := ar["possibility_reply"].(float64); ok {
			c.arPossibility = p
		}
		c.arPrompt, _ = ar["prompt"].(string)
		c.arWhitelist = stringSlice(ar["whitelist"])
	}
	return c
}

// NeedActiveReply mirrors need_active_reply: enabled + group message +
// not woken + (no whitelist or umo/group in whitelist) + probability.
func (g *GroupChatContext) NeedActiveReply(event *core.Event) bool {
	c := g.cfg(event.UnifiedMsgOrigin())
	if !c.enableActiveReply {
		return false
	}
	if !event.Source.IsGroup {
		return false
	}
	if event.IsAtOrWakeCommand {
		return false
	}
	if len(c.arWhitelist) > 0 {
		umo := event.UnifiedMsgOrigin()
		groupID := event.Source.ConvID
		allowed := false
		for _, w := range c.arWhitelist {
			if w == umo || (groupID != "" && w == groupID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	switch c.arMethod {
	case "possibility_reply":
		return rand.Float64() < c.arPossibility
	}
	return false
}

// RemoveSession clears all records for a umo (mirrors remove_session). The
// lock-table entry and the records are removed under the global lock while
// holding the per-umo lock: the global lock stops new getLock callers from
// racing a fresh lock into existence, and the per-umo lock serializes with any
// in-flight HandleMessage/OnReqLLM critical section (L-20).
func (g *GroupChatContext) RemoveSession(umo string) int {
	g.mu.Lock()
	lock := g.locks[umo]
	if lock != nil {
		lock.Lock()
	}
	n := 0
	if l := g.rawRecords[umo]; l != nil {
		n = l.Len()
	}
	delete(g.rawRecords, umo)
	delete(g.recordIDs, umo)
	delete(g.locks, umo)
	if lock != nil {
		lock.Unlock()
	}
	g.mu.Unlock()
	return n
}

// HandleMessage records an incoming group message (mirrors handle_message).
func (g *GroupChatContext) HandleMessage(event *core.Event) {
	if !event.Source.IsGroup {
		return
	}
	umo := event.UnifiedMsgOrigin()
	c := g.cfg(umo)
	finalMessage := g.formatMessage(event, c)

	lock := g.getLock(umo)
	lock.Lock()
	defer lock.Unlock()
	records := g.rawRecords[umo]
	if records == nil {
		records = list.New()
		g.rawRecords[umo] = records
	}
	ids := g.recordIDs[umo]
	if ids == nil {
		ids = list.New()
		g.recordIDs[umo] = ids
	}
	recordID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
	records.PushBack(finalMessage)
	ids.PushBack(recordID)
	for records.Len() > c.groupMessageMaxCnt {
		records.Remove(records.Front())
		ids.Remove(ids.Front())
	}
	event.SetExtra("_group_context_record_id", recordID)
	event.SetExtra("_group_context_raw_idx", records.Len()-1)
	logger.Debug("group_chat_context | %s | %s", umo, finalMessage)
}

// OnReqLLM injects the group context recorded before this message into the
// request's extra user content (mirrors on_req_llm). After injection the
// consumed records are dropped so they are not re-injected.
func (g *GroupChatContext) OnReqLLM(event *core.Event, req *provider.ProviderRequest) {
	umo := event.UnifiedMsgOrigin()
	recordID, _ := event.GetExtra("_group_context_record_id").(string)
	promptIdx, _ := event.GetExtra("_group_context_raw_idx").(int)

	lock := g.getLock(umo)
	lock.Lock()
	records := g.rawRecords[umo]
	if records == nil {
		lock.Unlock()
		return
	}
	ids := g.recordIDs[umo]

	// Locate the current record index.
	idx := promptIdx
	if recordID != "" && ids != nil {
		i := 0
		for e := ids.Front(); e != nil; e = e.Next() {
			if e.Value.(string) == recordID {
				idx = i
				break
			}
			i++
		}
	}
	if idx < 0 || idx >= records.Len() {
		lock.Unlock()
		return
	}

	// Collect records before the current one.
	toInject := []string{}
	i := 0
	for e := records.Front(); e != nil; e = e.Next() {
		if i >= idx {
			break
		}
		toInject = append(toInject, e.Value.(string))
		i++
	}

	// Keep only the current + later records.
	remaining := []interface{}{}
	remainingIDs := []interface{}{}
	i = 0
	for e := records.Front(); e != nil; {
		next := e.Next()
		if i >= idx {
			remaining = append(remaining, e.Value)
		}
		e = next
		i++
	}
	if ids != nil {
		i = 0
		for e := ids.Front(); e != nil; {
			next := e.Next()
			if i >= idx {
				remainingIDs = append(remainingIDs, e.Value)
			}
			e = next
			i++
		}
	}
	records.Init()
	ids.Init()
	for _, v := range remaining {
		records.PushBack(v)
	}
	for _, v := range remainingIDs {
		ids.PushBack(v)
	}
	lock.Unlock()

	if len(toInject) > 0 {
		req.ExtraUserContentParts = append(req.ExtraUserContentParts, map[string]interface{}{
			"type": "text",
			"text": formatGroupHistoryBlock(toInject),
		})
	}
}

// formatMessage builds the "[nickname/time]: content" record with optional
// image captioning (mirrors _format_message).
func (g *GroupChatContext) formatMessage(event *core.Event, c groupContextCfg) string {
	now := time.Now().Format("15:04:05")
	nickname := event.Source.SenderName
	if nickname == "" {
		nickname = event.Source.SenderID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s/%s]: ", nickname, now)

	for _, comp := range event.Message.Chain {
		switch v := comp.(type) {
		case *message.Plain:
			b.WriteString(" " + v.Text)
		case *message.Image:
			if c.imageCaption {
				caption, err := g.imageCaption(v, c)
				if err != nil {
					logger.I18nWarn("获取图片描述失败: %v", err)
					b.WriteString(" [Image]")
				} else {
					fmt.Fprintf(&b, " [Image: %s]", caption)
				}
			} else {
				b.WriteString(" [Image]")
			}
		case *message.At:
			fmt.Fprintf(&b, " @%s", v.Name)
			if v.Name == "" {
				fmt.Fprintf(&b, " @%s", v.TargetID)
			}
		case *message.Record:
			b.WriteString(" [Audio]")
		case *message.Video:
			b.WriteString(" [Video]")
		case *message.File:
			fmt.Fprintf(&b, " [File: %s]", v.Name)
		}
	}
	return b.String()
}

// imageCaption invokes the configured image-caption provider to describe an
// image (mirrors get_image_caption).
func (g *GroupChatContext) imageCaption(img *message.Image, c groupContextCfg) (string, error) {
	url := img.URL
	if url == "" {
		url = img.Path
	}
	if url == "" {
		return "", fmt.Errorf("图片 URL 为空")
	}
	prompt := c.imageCaptionPrompt
	if prompt == "" {
		prompt = "Please describe the image using Chinese."
	}
	return callImageCaptionProvider(g.config, c.imageCaptionProviderID, url, prompt)
}

// groupHistoryHeader/footer mirror GROUP_HISTORY_HEADER/FOOTER.
const groupHistoryHeader = "<system_reminder>" +
	"You are in a group chat. " +
	"Belows are group chat context after your last reply:\n" +
	"--- BEGIN CONTEXT---\n"
const groupHistoryFooter = "\n--- END CONTEXT ---\n</system_reminder>"

// callImageCaptionProvider invokes a chat provider (the session-configured
// image caption provider id, else the default chat provider) to describe an
// image (mirrors Python get_image_caption).
func callImageCaptionProvider(cfg map[string]interface{}, providerID, imageURL, prompt string) (string, error) {
	var pc map[string]interface{}
	if providerID != "" {
		pc = findProviderByID(cfg, providerID)
		if pc == nil {
			return "", fmt.Errorf("没有找到 ID 为 %s 的提供商", providerID)
		}
	} else {
		var err error
		pc, _, err = resolveProviderFromConfig(cfg)
		if err != nil {
			return "", err
		}
	}
	providerType, _ := pc["type"].(string)
	if providerType == "" {
		providerType, _ = pc["provider"].(string)
	}
	if providerType == "" {
		return "", fmt.Errorf("模型提供商配置缺少 type 字段")
	}
	providerSettings, _ := cfg["provider_settings"].(map[string]interface{})
	mergedCfg := mergeProviderSource(pc, cfg["provider_sources"])
	inst, err := provider.CreateProvider(providerType, mergedCfg, providerSettings)
	if err != nil {
		return "", fmt.Errorf("初始化模型提供商失败: %w", err)
	}
	chatInst, ok := inst.(provider.ChatProvider)
	if !ok {
		return "", fmt.Errorf("提供商 %s 不支持聊天能力", providerType)
	}
	req := &provider.ProviderRequest{
		Prompt:    prompt,
		ImageURLs: []string{imageURL},
		SessionID: "image_caption",
		Contexts:  []map[string]interface{}{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := chatInst.TextChat(ctx, req)
	if err != nil {
		return "", err
	}
	if resp.Role == "err" {
		return "", fmt.Errorf("%s", resp.CompletionText)
	}
	return resp.CompletionText, nil
}

// formatGroupHistoryBlock renders the injected history block.
func formatGroupHistoryBlock(records []string) string {
	var b strings.Builder
	b.WriteString(groupHistoryHeader)
	for _, r := range records {
		b.WriteString(r + "\n")
	}
	b.WriteString(groupHistoryFooter)
	return b.String()
}

// groupChatContextEnabled mirrors main.py group_context_enabled: active when
// group_icl_enable or active_reply.enable is on.
func (s *ProcessStage) groupChatContextEnabled(event *core.Event) bool {
	settings := map[string]interface{}{}
	if ps, ok := s.config["provider_ltm_settings"].(map[string]interface{}); ok {
		settings = ps
	}
	if v, _ := settings["group_icl_enable"].(bool); v {
		return true
	}
	if ar, ok := settings["active_reply"].(map[string]interface{}); ok {
		if v, _ := ar["enable"].(bool); v {
			return true
		}
	}
	return false
}

// groupLTMSetting reads a boolean provider_ltm_settings key.
func (s *ProcessStage) groupLTMSetting(event *core.Event, key string) bool {
	settings := map[string]interface{}{}
	if ps, ok := s.config["provider_ltm_settings"].(map[string]interface{}); ok {
		settings = ps
	}
	v, _ := settings[key].(bool)
	return v
}
