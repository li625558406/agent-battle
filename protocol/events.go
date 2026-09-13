package protocol

// 事件类型常量。跨端契约，只增不改。
const (
	EventToolCall = "tool_call"
	EventFileEdit = "file_edit"
	EventMessage  = "message"
	EventError    = "error"
	EventResult   = "result"
)

// Event 是事件流的最小单元。字段顺序即 JSON 序列化顺序，
// hash 链依赖 canonical 序列化，勿调整字段顺序。
// 红线：任何字段不得存放文件内容明文；Note 只允许存路径等摘要信息。
type Event struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"` // unix 毫秒
	Type       string `json:"type"`
	Tool       string `json:"tool,omitempty"`
	ArgsHash   string `json:"args_hash,omitempty"` // sha256(args)，防内容泄漏
	DurationMS int64  `json:"duration_ms,omitempty"`
	Tokens     int    `json:"tokens,omitempty"`
	Note       string `json:"note,omitempty"`
	PrevHash   string `json:"prev_hash,omitempty"`
	Hash       string `json:"hash"`
}
