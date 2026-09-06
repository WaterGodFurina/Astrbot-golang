// Package kook implements a KOOK (开黑啦) platform adapter.
// 从 astrbot/core/platform/sources/kook/ (Python) 1:1 移植。
// 协议: REST (https://www.kookapp.cn/api/v3/) + WebSocket 长连接 (gateway/index)。
// 参考文件: kook_config.py (配置) / kook_client.py (REST+WS 客户端) /
//
//	kook_event.py (发送) / kook_types.py (类型) / kook_roles_record.py (角色缓存)。
package kook

import "encoding/json"

// KookApiPaths 对应 Python kook_types.py 的 KookApiPaths。
const (
	kookBaseURL    = "https://www.kookapp.cn"
	kookAPIVersion = "/api/v3"

	// 初始化相关
	apiUserMe       = kookBaseURL + kookAPIVersion + "/user/me"
	apiUserView     = kookBaseURL + kookAPIVersion + "/user/view"
	apiGatewayIndex = kookBaseURL + kookAPIVersion + "/gateway/index"

	// 消息相关
	apiAssetCreate      = kookBaseURL + kookAPIVersion + "/asset/create"
	apiChannelMsgCreate = kookBaseURL + kookAPIVersion + "/message/create"        // 频道消息
	apiDirectMsgCreate  = kookBaseURL + kookAPIVersion + "/direct-message/create" // 私聊消息
	apiReactionAdd      = kookBaseURL + kookAPIVersion + "/message/reaction/add"  // 表情回应

	// KOOK metadata APIs (aligned with Python kook_event.py get_group)
	apiChannelView   = kookBaseURL + kookAPIVersion + "/channel/view"
	apiGuildView     = kookBaseURL + kookAPIVersion + "/guild/view"
	apiGuildUserList = kookBaseURL + kookAPIVersion + "/guild/user-list"
	apiGuildRoleList = kookBaseURL + kookAPIVersion + "/guild-role/list"
)

// KookMessageType 对应 Python kook_types.py 的 KookMessageType (IntEnum)。
// 定义参见 KOOK 事件结构文档: https://developer.kookapp.cn/doc/event/event-introduction
type KookMessageType int

const (
	KookMsgText      KookMessageType = 1   // 文本
	KookMsgImage     KookMessageType = 2   // 图片
	KookMsgVideo     KookMessageType = 3   // 视频
	KookMsgFile      KookMessageType = 4   // 文件
	KookMsgAudio     KookMessageType = 8   // 音频
	KookMsgKMarkdown KookMessageType = 9   // kmarkdown
	KookMsgCard      KookMessageType = 10  // 卡片
	KookMsgSystem    KookMessageType = 255 // 系统消息
)

// KookModuleType 对应 Python kook_types.py 的 KookModuleType (str Enum)。
type KookModuleType string

const (
	ModulePlainText  KookModuleType = "plain-text"
	ModuleKMarkdown  KookModuleType = "kmarkdown"
	ModuleImage      KookModuleType = "image"
	ModuleHeader     KookModuleType = "header"
	ModuleSection    KookModuleType = "section"
	ModuleImageGroup KookModuleType = "image-group"
	ModuleContainer  KookModuleType = "container"
	ModuleFile       KookModuleType = "file"
	ModuleAudio      KookModuleType = "audio"
	ModuleVideo      KookModuleType = "video"
	ModuleCard       KookModuleType = "card"
)

// KookChannelType 对应 Python kook_types.py 的 KookChannelType。
type KookChannelType string

const (
	KookChannelGroup     KookChannelType = "GROUP"     // 频道(群聊)
	KookChannelPerson    KookChannelType = "PERSON"    // 私聊
	KookChannelBroadcast KookChannelType = "BROADCAST" // 广播频道
)

// KookRoleExtraType 对应 Python kook_types.py 的 KookRoleExtraType (角色更新系统通知类型)。
type KookRoleExtraType string

const (
	KookRoleAdded   KookRoleExtraType = "added_role"
	KookRoleDeleted KookRoleExtraType = "deleted_role"
	KookRoleUpdated KookRoleExtraType = "updated_role"
)

// KookMessageSignal 对应 Python kook_types.py 的 KookMessageSignal (WebSocket 信令)。
// WS 文档: https://developer.kookapp.cn/doc/websocket
const (
	signalMessage   = 0 // server->client  消息(包含聊天和通知消息)
	signalHello     = 1 // server->client  客户端连接 ws 时, 服务端返回握手结果
	signalPing      = 2 // client->server  心跳, ping
	signalPong      = 3 // server->client  心跳, pong
	signalResume    = 4 // client->server  resume, 恢复会话
	signalReconnect = 5 // server->client  reconnect, 要求客户端断开当前连接重新连接
	signalResumeAck = 6 // server->client  resume ack
)

// kookWSFrame 对应 Python kook_types.py 的 KookWebsocketEvent。
// 原始推送结构: {"s": 信令, "d": 数据, "sn": 消息序号}
type kookWSFrame struct {
	Signal int             `json:"s"`
	Data   json.RawMessage `json:"d"`
	SN     *int64          `json:"sn"`
}

// kookHelloEventData 对应 Python 的 KookHelloEventData (HELLO 握手数据)。
type kookHelloEventData struct {
	Code      int    `json:"code"`
	SessionID string `json:"session_id"`
}

// kookResumeAckEventData 对应 Python 的 KookResumeAckEventData。
type kookResumeAckEventData struct {
	SessionID string `json:"session_id"`
}

// kookMessageEventData 对应 Python 的 KookMessageEventData。
// 字段定义参见: https://developer.kookapp.cn/doc/event/message
type kookMessageEventData struct {
	ChannelType  KookChannelType `json:"channel_type"`
	Type         KookMessageType `json:"type"`
	TargetID     string          `json:"target_id"`
	AuthorID     string          `json:"author_id"`
	Content      json.RawMessage `json:"content"` // kmarkdown/卡片为字符串; 道具消息为 dict
	MsgID        string          `json:"msg_id"`
	MsgTimestamp int64           `json:"msg_timestamp"`
	Nonce        string          `json:"nonce"`
	FromType     int             `json:"from_type"`
	Extra        kookExtra       `json:"extra"`
}

// kookExtra 对应 Python 的 KookExtra。
type kookExtra struct {
	Type           json.RawMessage `json:"type"` // 非系统消息时为 int, 系统消息(255)时为 str
	Code           string          `json:"code"`
	Body           json.RawMessage `json:"body"`
	Author         *kookAuthor     `json:"author"`
	KMarkdown      *kookKMarkdown  `json:"kmarkdown"`
	GuildID        string          `json:"guild_id"`
	LastMsgContent string          `json:"last_msg_content"`
	Mention        []string        `json:"mention"`
	MentionAll     bool            `json:"mention_all"`
	ChannelName    string          `json:"channel_name"`
	GuildType      int             `json:"guild_type"`
	MentionRoles   []int64         `json:"mention_roles"`
}

// kookAuthor 对应 Python 的 KookAuthor。
type kookAuthor struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Nickname string  `json:"nickname"`
	Avatar   string  `json:"avatar"`
	Bot      bool    `json:"bot"`
	Roles    []int64 `json:"roles"`
}

// kookKMarkdown 对应 Python 的 KookKMarkdown。
type kookKMarkdown struct {
	RawContent      string                        `json:"raw_content"`
	MentionPart     []kookMarkdownMentionPart     `json:"mention_part"`
	MentionRolePart []kookMarkdownMentionRolePart `json:"mention_role_part"`
}

// kookMarkdownMentionPart 对应 Python 的 KookMarkdownMentionPart。
type kookMarkdownMentionPart struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Avatar   string `json:"avatar"`
}

// kookMarkdownMentionRolePart 对应 Python 的 KookMarkdownMentionRolePart。
type kookMarkdownMentionRolePart struct {
	RoleID      int64  `json:"role_id"`
	Name        string `json:"name"`
	Color       int    `json:"color"`
	ColorType   int    `json:"color_type"`
	Position    *int   `json:"position"`
	Hoist       *int   `json:"hoist"`
	Mentionable *int   `json:"mentionable"`
	Permissions *int   `json:"permissions"`
}

// kookUserMeData 对应 Python 的 KookUserMeData (/user/me 接口 data 字段)。
type kookUserMeData struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Bot      bool   `json:"bot"`
	Status   int    `json:"status"`
}

// kookGatewayIndexData 对应 Python 的 KookGatewayIndexData。
type kookGatewayIndexData struct {
	URL string `json:"url"`
}

// kookUserViewData 对应 Python 的 KookUserMeViewData (含 roles)。
type kookUserViewData struct {
	Roles []int64 `json:"roles"`
}

// ---------- 卡片消息类型 (对应 Python kook_types.py 的卡片模型) ----------

// kookCardMessage 对应 Python 的 KookCardMessage。
type kookCardMessage struct {
	Type    string            `json:"type"`
	Theme   string            `json:"theme"`
	Size    string            `json:"size"`
	Modules []json.RawMessage `json:"modules"`
}

// kookCardModuleMeta 用于按 type 字段分派模块类型。
type kookCardModuleMeta struct {
	Type KookModuleType `json:"type"`
}

// kookCardTextElement 对应 Python 的 PlainTextElement / KmarkdownElement 的公共字段。
type kookCardTextElement struct {
	Content string `json:"content"`
	Emoji   bool   `json:"emoji"`
}

// kookCardSectionModule 对应 Python 的 SectionModule。
type kookCardSectionModule struct {
	Text json.RawMessage `json:"text"`
}

// kookCardHeaderModule 对应 Python 的 HeaderModule。
type kookCardHeaderModule struct {
	Text kookCardTextElement `json:"text"`
}

// kookCardImageElement 对应 Python 的 ImageElement。
type kookCardImageElement struct {
	Src  string `json:"src"`
	Alt  string `json:"alt"`
	Size string `json:"size"`
}

// kookCardImageGroupModule 对应 Python 的 ImageGroupModule / ContainerModule (图片组)。
type kookCardImageGroupModule struct {
	Elements []kookCardImageElement `json:"elements"`
}

// kookCardFileModule 对应 Python 的 FileModule (文件/音频/视频)。
type kookCardFileModule struct {
	Type  KookModuleType `json:"type"`
	Title string         `json:"title"`
	Src   string         `json:"src"`
}
