// Package adapter 定义"召唤本机 coding agent"的统一契约。
package adapter

import "context"

// RawEvent 是 adapter 产出的未加工事件，由 collector 打上
// 序号/时间戳/hash 链后成为 protocol.Event。
type RawEvent struct {
	Type       string // 同 protocol 的事件类型常量
	Tool       string
	ArgsHash   string
	DurationMS int64
	Tokens     int
	Note       string // 仅路径/摘要，红线禁止内容明文
}

// Adapter 是接入一个 coding agent 的最小契约。
type Adapter interface {
	Name() string
	// Detect 返回非 nil 表示本机不可用（如二进制缺失）。
	Detect() error
	// Launch 在 cwd 中以 taskDescription 运行 agent，事件流入 out。
	// env 为追加给 agent 进程的环境变量（配置分化的载体）。
	// ctx 取消/超时必须杀死 agent 进程。out 由调用方 close。
	Launch(ctx context.Context, cwd, taskDescription string, env []string, out chan<- RawEvent) error
}
