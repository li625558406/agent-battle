# M2 计划 2：对局复盘报告 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 对任意已结算对局产出确定性复盘报告（双侧时间线 + first_error 关键节点标注 + 六项行为对比），公开 API 路由 + CLI `review` 子命令，零 LLM 依赖。

**Architecture:** 平台侧纯函数包 `platform/internal/review` 组装报告（复用 profile 包的事件解码与指标提取，口径单点）；api 加公开路由 `GET /api/matches/{id}/review`（404/409/200 三分支）；runner 侧 `client.Review` + `review` 子命令纯渲染。复盘是只读投影，不改表结构、无迁移、不预生成。

**Tech Stack:** Go 1.22+（mux 方法+路径参数）、modernc.org/sqlite、text/tabwriter、net/http/httptest。

**规格**：`docs/superpowers/specs/2026-09-13-m2-plan2-review-design.md`

**工程须知（每个任务的实现者都必须知道）：**
- 仓库根 `D:\AI\agent-battle`，Go module 名 `agentbattle`；protocol 包在根目录 `protocol/`（import 路径 `agentbattle/protocol`），不在 platform/internal 下。
- **gopls 滞后误报是本仓库长期现象**：IDE 报 undefined/UnusedImport/幽灵文件告警一律忽略，以真实命令 `go build ./... && go vet ./...` 为准。
- 所有命令在仓库根以 git-bash 运行；提交信息用中文 `feat:`/`test:`/`docs:` 前缀，结尾带 `Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>`。
- 工作分支 `m2/review`（Task 1 创建）；push 由控制器在收官后与用户确认，实现者不 push。
- `protocol.Event` 字段：`Seq int, TS int64, Type, Tool, ArgsHash string, DurationMS int64, Tokens int, Note string, PrevHash string, Hash string`。file_edit 的路径存在 `Note` 字段（红线：Note 只允许存路径等摘要信息）。事件类型常量：`EventToolCall="tool_call"`、`EventFileEdit="file_edit"`、`EventError="error"` 等。
- `profile.ExtractMetrics(events []protocol.Event, passed, total int, wallMS int64) MatchMetrics`：events 为 nil 时 HasEvents=false；passed 钳制到 [0,total]；Tokens 只累加正值；ToolCalls 数 tool_call 事件。
- 测试须含对抗性用例（空/损坏输入、边界值、钳制行为），这是全局 CLAUDE.md 的硬性要求。

---

### Task 1: profile 包导出 DecodeEvents

**Files:**
- Modify: `platform/internal/profile/recompute.go:85-112`（decodeEvents → DecodeEvents 及两处调用点）

review 包需要复用事件解码（gzip + NDJSON + 16MB 上限 + 整流降级）。本任务纯重命名导出，零行为变更。

- [ ] **Step 1: 确认 decodeEvents 的全部引用点**

Run: `grep -rn "decodeEvents" --include="*.go" .`
Expected: 仅 `platform/internal/profile/recompute.go` 内 3 处（定义 1 + 调用 2）。若测试文件也有引用，一并记录。

- [ ] **Step 2: 重命名为导出函数**

修改 `platform/internal/profile/recompute.go`，把：

```go
// decodeEvents 解压 NDJSON 事件流；正常读到 EOF 时返回累积的事件，任何其他
// 失败（空输入、坏 gzip 头、中段截断/坏 CRC/畸形 JSON、解压超 maxEventBytes
// 等）整流丢弃返回 nil（该局降级为无事件流，由 ExtractMetrics/BuildProfile
// 的缺席规则处理，不阻断整场画像；也绝不把残缺数据带进画像）。
func decodeEvents(gz []byte) []protocol.Event {
```

改为（只改函数名与首行注释，逻辑一行不动）：

```go
// DecodeEvents 解压 NDJSON 事件流；正常读到 EOF 时返回累积的事件，任何其他
// 失败（空输入、坏 gzip 头、中段截断/坏 CRC/畸形 JSON、解压超 maxEventBytes
// 等）整流丢弃返回 nil（降级为无事件流，由 ExtractMetrics/BuildProfile/
// review.BuildReport 的缺席规则处理，绝不把残缺数据带进下游）。
// 导出供 review 包复用（复盘与画像共用同一解码与降级口径）。
func DecodeEvents(gz []byte) []protocol.Event {
```

同文件内两处调用 `decodeEvents(r.EventsGZ)` 改为 `DecodeEvents(r.EventsGZ)`（约 45、49 行）。

- [ ] **Step 3: 全量验证**

Run: `go build ./... && go vet ./... && go test -race ./platform/...`
Expected: 全部通过（无编译错误、既有测试全绿——零行为变更的证明）。

- [ ] **Step 4: Commit**

```bash
git add platform/internal/profile/recompute.go
git commit -m "refactor: profile.decodeEvents 导出为 DecodeEvents（review 包复用）

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 2: review 纯函数包 —— BuildReport

**Files:**
- Create: `platform/internal/review/report.go`
- Test: `platform/internal/review/report_test.go`

报告结构、时间线提取、first_error 标注、compare 指标。只依赖 protocol 与 profile，不 import api/store。

- [ ] **Step 1: 写失败测试**

创建 `platform/internal/review/report_test.go`：

```go
// report_test.go —— BuildReport 时间线/标注/对比与降级、对抗性行为。
package review

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"testing"

	"agentbattle/protocol"
)

// gz 构造合法 gzip NDJSON 事件流（BuildReport 内部经 profile.DecodeEvents 解码）。
func gz(t *testing.T, events ...protocol.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var meta = Meta{MatchID: 7, TaskID: "fix-add", TaskType: "general", Status: "done", Winner: "a"}

// TestBuildReportTimeline 正常流：时间线逐事件映射、first_error 标注、
// compare 各项（pass_ratio/tokens 复用 ExtractMetrics，errors/edits 自数）。
func TestBuildReportTimeline(t *testing.T) {
	a := SideInput{
		Agent: "echoA", Passed: 2, Total: 2, WallMS: 100,
		EventsGZ: gz(t,
			protocol.Event{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 5},
			protocol.Event{Seq: 2, Type: protocol.EventError},
			protocol.Event{Seq: 3, Type: protocol.EventFileEdit, Note: "src/a.go"},
		),
	}
	b := SideInput{
		Agent: "echoB", Passed: 1, Total: 2, WallMS: 200,
		EventsGZ: gz(t, protocol.Event{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 3}),
	}
	rep := BuildReport(meta, a, b)

	if rep.MatchID != 7 || rep.TaskID != "fix-add" || rep.TaskType != "general" ||
		rep.Status != "done" || rep.Winner != "a" {
		t.Fatalf("元信息不符: %+v", rep)
	}
	if sa := rep.Sides["a"]; sa.Agent != "echoA" || sa.Passed != 2 || sa.Total != 2 ||
		sa.WallMS != 100 || !sa.HasEvents {
		t.Fatalf("sides.a 不符: %+v", sa)
	}
	ta := rep.Timeline["a"]
	if len(ta) != 3 || ta[0].Seq != 1 || ta[0].Type != "tool_call" || ta[0].Tool != "bash" ||
		ta[0].Tokens != 5 || ta[2].Type != "file_edit" || ta[2].Path != "src/a.go" {
		t.Fatalf("timeline.a 不符: %+v", ta)
	}
	if len(rep.Marks["a"]) != 1 || rep.Marks["a"][0].Kind != "first_error" || rep.Marks["a"][0].Seq != 2 {
		t.Fatalf("marks.a 应为 first_error@2: %+v", rep.Marks["a"])
	}
	if len(rep.Marks["b"]) != 0 {
		t.Fatalf("b 无 error 应无标注: %+v", rep.Marks["b"])
	}
	c := rep.Compare
	if c.PassRatio.A != 1 || c.PassRatio.B != 0.5 ||
		c.ToolCalls.A != 1 || c.ToolCalls.B != 1 ||
		c.Errors.A != 1 || c.Errors.B != 0 ||
		c.Edits.A != 1 || c.Edits.B != 0 ||
		c.Tokens.A != 5 || c.Tokens.B != 3 ||
		c.WallMS.A != 100 || c.WallMS.B != 200 {
		t.Fatalf("compare 不符: %+v", c)
	}
}

// TestBuildReportDegraded 单侧事件流缺失/损坏：整侧降级（timeline 空、
// has_events=false），静态数据照常；空切片序列化为 [] 而非 null。
func TestBuildReportDegraded(t *testing.T) {
	rep := BuildReport(meta,
		SideInput{Agent: "a1", Passed: 2, Total: 2, WallMS: 100},          // 无事件流
		SideInput{Agent: "b1", Passed: 1, Total: 2, WallMS: 200, EventsGZ: []byte("junk")}, // 坏 gzip
	)
	sa := rep.Sides["a"]
	if sa.HasEvents || len(rep.Timeline["a"]) != 0 || len(rep.Marks["a"]) != 0 {
		t.Fatalf("无事件流侧应降级: %+v", sa)
	}
	if sa.PassRatioRep(&rep) != 1 { // 见下：直接查 compare 更直观
		t.Fatalf("占位断言不应到达")
	}
	if rep.Compare.PassRatio.A != 1 || rep.Compare.Tokens.A != 0 || rep.Compare.WallMS.A != 100 {
		t.Fatalf("降级侧静态数据应照常: %+v", rep.Compare)
	}
	if rep.Compare.Tokens.B != 0 {
		t.Fatalf("坏 gzip 侧 tokens 应 0: %+v", rep.Compare)
	}
	// 空切片必须序列化为 [] 而非 null（契约：前端/CLI 不处理 null 分支）
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"timeline":{"a":[],"b":[]}`)) ||
		!bytes.Contains(out, []byte(`"marks":{"a":[],"b":[]}`)) {
		t.Fatalf("空 timeline/marks 应序列化为 []:\n%s", out)
	}
}

// TestBuildReportAdversarial 对抗性输入全部有界：首事件即 error、全 error 流、
// passed 越界钳制、负 tokens 不计、NaN 不产生。
func TestBuildReportAdversarial(t *testing.T) {
	// 首事件即 error：标注 seq=1
	rep := BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 2,
			EventsGZ: gz(t, protocol.Event{Seq: 9, Type: protocol.EventError})},
		SideInput{Agent: "b", Passed: 0, Total: 2})
	if mk := rep.Marks["a"]; len(mk) != 1 || mk[0].Seq != 9 {
		t.Fatalf("首事件即 error 应标注其 seq: %+v", mk)
	}
	// 全 error 流：errors=3、仅首个标注
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 0, Total: 0,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: protocol.EventError},
				protocol.Event{Seq: 2, Type: protocol.EventError},
				protocol.Event{Seq: 3, Type: protocol.EventError})},
		SideInput{Agent: "b", Passed: 0, Total: 0})
	if rep.Compare.Errors.A != 3 {
		t.Fatalf("全 error 流 errors 应 3: %+v", rep.Compare)
	}
	if len(rep.Marks["a"]) != 1 || rep.Marks["a"][0].Seq != 1 {
		t.Fatalf("应仅标注首个 error: %+v", rep.Marks["a"])
	}
	if rep.Compare.PassRatio.A != 0 {
		t.Fatalf("total==0 → pass_ratio 0: %+v", rep.Compare)
	}
	// passed 越界钳制 + 负 tokens 不计（与 ExtractMetrics 口径一致）
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 5, Total: 2, WallMS: 1,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: protocol.EventMessage, Tokens: -7},
				protocol.Event{Seq: 2, Type: protocol.EventMessage, Tokens: 4})},
		SideInput{Agent: "b", Passed: -1, Total: 2, WallMS: 1})
	if rep.Compare.PassRatio.A != 1 || rep.Compare.PassRatio.B != 0 {
		t.Fatalf("passed 越界应钳制到 [0,total]: %+v", rep.Compare)
	}
	if rep.Compare.Tokens.A != 4 {
		t.Fatalf("负 tokens 不应计入: %+v", rep.Compare)
	}
	// 任何 compare 值不得为 NaN
	out, _ := json.Marshal(rep.Compare)
	if bytes.Contains(out, []byte("NaN")) {
		t.Fatalf("compare 不得含 NaN: %s", out)
	}
	if math.IsNaN(rep.Compare.PassRatio.A) || math.IsNaN(rep.Compare.PassRatio.B) {
		t.Fatal("pass_ratio 不得为 NaN")
	}
}
```

注意：`TestBuildReportDegraded` 中 `sa.PassRatioRep(&rep)` 这一行是**占位错误行，实现时直接删除**（真实断言是其后的 compare 三行）。保留它会导致编译失败——这正是"先红"的一步。

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./platform/internal/review/`
Expected: 编译失败（`undefined: BuildReport` 等）。

- [ ] **Step 3: 实现 report.go**

创建 `platform/internal/review/report.go`：

```go
// Package review 是对局复盘报告的纯函数核心：输入单场对局的元信息与双侧
// 事件流/判分结果，组装结构化复盘（时间线 + 关键节点标注 + 行为对比）。
// 只依赖 protocol 与 profile（复用事件解码与指标提取，口径与画像单点一致），
// 不 import api/store。设计规格：
// docs/superpowers/specs/2026-09-13-m2-plan2-review-design.md §3。
package review

import (
	"agentbattle/platform/internal/profile"
	"agentbattle/protocol"
)

// Meta 对局元信息（api 层经 store.MatchByID/TaskTypeOf 读出后传入）。
type Meta struct {
	MatchID  int64
	TaskID   string
	TaskType string
	Status   string
	Winner   string
}

// SideInput 单侧输入（api 层经 store.ReviewData 读出后传入）。
type SideInput struct {
	Agent    string
	Passed   int
	Total    int
	WallMS   int64
	EventsGZ []byte
}

// SideMeta 单侧概要。
type SideMeta struct {
	Agent     string `json:"agent"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	WallMS    int64  `json:"wall_ms"`
	HasEvents bool   `json:"has_events"`
}

// TlEvent 时间线事件。红线 3：只含路径与操作类型（Path 来源事件 Note），
// 无文件内容明文。不含 PrevHash/Hash——哈希链属 §7.2 审计功能，M3+ 再做。
type TlEvent struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"`
	Type       string `json:"type"`
	Tool       string `json:"tool,omitempty"`
	Path       string `json:"path,omitempty"` // 仅 file_edit 有值
	DurationMS int64  `json:"duration_ms,omitempty"`
	Tokens     int    `json:"tokens,omitempty"`
}

// Mark 关键节点标注。M2 标注集仅 first_error：首个 error 事件的 Seq。
// 设计文档 §6.4 的"大回滚"无事件语义（事件流无回滚/删除）、"转折点"无
// 规则口径，均砍掉；M3+ LLM judge 可接管。
type Mark struct {
	Kind string `json:"kind"`
	Seq  int    `json:"seq"`
}

// Pair 一项对比指标的 A/B 值。
type Pair struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

// Compare 双侧行为对比。pass_ratio/tool_calls/tokens/wall_ms 复用
// profile.ExtractMetrics（与画像同一份提取代码，口径不可能漂移）；
// errors/edits 为事件个数（时间线遍历时自数——MatchMetrics 只有布尔）。
type Compare struct {
	PassRatio Pair `json:"pass_ratio"`
	ToolCalls Pair `json:"tool_calls"`
	Errors    Pair `json:"errors"`
	Edits     Pair `json:"edits"`
	Tokens    Pair `json:"tokens"`
	WallMS    Pair `json:"wall_ms"`
}

// Report 复盘报告（GET /api/matches/{id}/review 返回体）。
type Report struct {
	MatchID  int64               `json:"match_id"`
	TaskID   string              `json:"task_id"`
	TaskType string              `json:"task_type"`
	Status   string              `json:"status"`
	Winner   string              `json:"winner"`
	Sides    map[string]SideMeta `json:"sides"`
	// Timeline/Marks 值恒为非 nil 切片（空侧序列化为 [] 非 null，消费端
	// 无需处理 null 分支）。
	Timeline map[string][]TlEvent `json:"timeline"`
	Marks    map[string][]Mark    `json:"marks"`
	Compare  Compare              `json:"compare"`
}

// sideStats 单侧内部统计（不导出：只服务于 Compare 组装）。
type sideStats struct {
	passRatio float64
	toolCalls int
	errors    int
	edits     int
	tokens    int
	wallMS    int64
}

// buildSide 单侧处理：解码事件（缺失/损坏整流降级）→ 一次遍历同时产出
// 时间线、first_error 标注与计数。
func buildSide(in SideInput) (SideMeta, []TlEvent, []Mark, sideStats) {
	events := profile.DecodeEvents(in.EventsGZ)
	m := profile.ExtractMetrics(events, in.Passed, in.Total, in.WallMS)
	meta := SideMeta{
		Agent: in.Agent, Passed: in.Passed, Total: in.Total,
		WallMS: in.WallMS, HasEvents: len(events) > 0,
	}
	tl := make([]TlEvent, 0, len(events))
	marks := []Mark{}
	st := sideStats{
		passRatio: m.PassRatio, toolCalls: m.ToolCalls,
		tokens: m.Tokens, wallMS: in.WallMS,
	}
	firstErr := true
	for _, e := range events {
		te := TlEvent{Seq: e.Seq, TS: e.TS, Type: e.Type, Tool: e.Tool}
		if e.Type == protocol.EventFileEdit {
			te.Path = e.Note
			st.edits++
		}
		if e.Tokens > 0 {
			te.Tokens = e.Tokens
		}
		te.DurationMS = e.DurationMS
		tl = append(tl, te)
		switch e.Type {
		case protocol.EventError:
			st.errors++
			if firstErr {
				marks = append(marks, Mark{Kind: "first_error", Seq: e.Seq})
				firstErr = false
			}
		}
	}
	return meta, tl, marks, st
}

// BuildReport 组装复盘报告。单侧事件流缺失/解码失败时该侧降级：timeline
// 空、has_events=false，静态数据（pass_ratio/wall_ms）照常——与画像管道
// 降级口径一致。
func BuildReport(meta Meta, a, b SideInput) Report {
	sa, ta, ma, sta := buildSide(a)
	sb, tb, mb, stb := buildSide(b)
	return Report{
		MatchID: meta.MatchID, TaskID: meta.TaskID, TaskType: meta.TaskType,
		Status: meta.Status, Winner: meta.Winner,
		Sides:    map[string]SideMeta{"a": sa, "b": sb},
		Timeline: map[string][]TlEvent{"a": ta, "b": tb},
		Marks:    map[string][]Mark{"a": ma, "b": mb},
		Compare: Compare{
			PassRatio: Pair{A: sta.passRatio, B: stb.passRatio},
			ToolCalls: Pair{A: float64(sta.toolCalls), B: float64(stb.toolCalls)},
			Errors:    Pair{A: float64(sta.errors), B: float64(stb.errors)},
			Edits:     Pair{A: float64(sta.edits), B: float64(stb.edits)},
			Tokens:    Pair{A: float64(sta.tokens), B: float64(stb.tokens)},
			WallMS:    Pair{A: float64(sta.wallMS), B: float64(stb.wallMS)},
		},
	}
}
```

- [ ] **Step 4: 运行确认通过（先删掉测试中的占位行）**

删除 `TestBuildReportDegraded` 中的 `sa.PassRatioRep(&rep)` 判断块（Step 1 已注明），然后：

Run: `go build ./... && go vet ./... && go test -race ./platform/internal/review/`
Expected: 全部 PASS。

- [ ] **Step 5: 全量回归**

Run: `go test -race ./...`
Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
git add platform/internal/review/
git commit -m "feat: review 纯函数包——复盘报告组装（时间线/first_error/对比）

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 3: store 扩展 —— ReviewData 查询

**Files:**
- Modify: `platform/internal/store/store.go`（文件末尾追加）
- Test: `platform/internal/store/store_test.go`（文件末尾追加）

- [ ] **Step 1: 写失败测试**

在 `platform/internal/store/store_test.go` 末尾追加：

```go
// TestReviewData 双侧 JOIN 取数、缺行零值兜底、不存在对局报错。
func TestReviewData(t *testing.T) {
	s := openTest(t)
	a1, _ := s.CreateAgent("r1")
	a2, _ := s.CreateAgent("r2")

	// 已结算对局：双侧齐全
	m1, err := s.CreateMatch("tk", "general", a1.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m1, "a", Result{Passed: 2, Total: 2, WallMS: 100, EventsGZ: []byte("gza")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m1, "b", Result{Passed: 1, Total: 2, WallMS: 200, EventsGZ: []byte("gzb")}); err != nil {
		t.Fatal(err)
	}
	sa, sb, err := s.ReviewData(m1)
	if err != nil {
		t.Fatal(err)
	}
	if sa.AgentID != a1.ID || sa.Agent != "r1" || sa.Passed != 2 || sa.Total != 2 ||
		sa.WallMS != 100 || string(sa.EventsGZ) != "gza" {
		t.Fatalf("sideA 不符: %+v", sa)
	}
	if sb.AgentID != a2.ID || sb.Agent != "r2" || sb.Passed != 1 || sb.Total != 2 ||
		sb.WallMS != 200 || string(sb.EventsGZ) != "gzb" {
		t.Fatalf("sideB 不符: %+v", sb)
	}

	// pending 对局只有 a 侧：b 侧 agent 名照常、结果零值（LEFT JOIN 兜底）
	m2, err := s.CreateMatch("tk", "general", a1.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m2, "a", Result{Passed: 1, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if _, sb, err = s.ReviewData(m2); err != nil {
		t.Fatal(err)
	}
	if sb.Agent != "r2" || sb.Passed != 0 || sb.Total != 0 || sb.WallMS != 0 || sb.EventsGZ != nil {
		t.Fatalf("缺行侧应零值兜底: %+v", sb)
	}

	// 不存在的对局 → 错误
	if _, _, err := s.ReviewData(99999); err == nil {
		t.Fatal("不存在的对局应报错")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./platform/internal/store/ -run TestReviewData`
Expected: 编译失败（`s.ReviewData undefined`）。

- [ ] **Step 3: 实现 ReviewData**

在 `platform/internal/store/store.go` 末尾追加：

```go
// ReviewSide 单侧复盘数据（AgentID/Agent 来自 agents；结果字段来自 results，
// 缺行时零值——done 对局双侧必齐，防御性兜底）。
type ReviewSide struct {
	AgentID  int64
	Agent    string
	Passed   int
	Total    int
	WallMS   int64
	EventsGZ []byte
}

// ReviewData 返回对局双侧复盘数据。一条 JOIN（matches × agents × results，
// LEFT JOIN 保证缺行侧也返回 agent 名），行序固定 A 前 B 后。
// 自我对局（agent_a == agent_b）被 api 层禁止，此处仅 2 行契约；不足 2 行
// 即对局异常，报错而非静默。
func (s *Store) ReviewData(matchID int64) (ReviewSide, ReviewSide, error) {
	rows, err := s.db.Query(`
		SELECT CASE WHEN a.id = m.agent_a THEN 'a' ELSE 'b' END,
		       a.id, a.name,
		       COALESCE(r.passed, 0), COALESCE(r.total, 0), COALESCE(r.wall_ms, 0),
		       r.events_gz
		FROM matches m
		JOIN agents a ON a.id = m.agent_a OR a.id = m.agent_b
		LEFT JOIN results r ON r.match_id = m.id
		  AND r.side = CASE WHEN a.id = m.agent_a THEN 'a' ELSE 'b' END
		WHERE m.id = ?
		ORDER BY CASE WHEN a.id = m.agent_a THEN 0 ELSE 1 END`, matchID)
	if err != nil {
		return ReviewSide{}, ReviewSide{}, err
	}
	defer rows.Close()
	var out [2]ReviewSide
	i := 0
	for rows.Next() && i < 2 {
		if err := rows.Scan(&out[i].AgentID, &out[i].Agent,
			&out[i].Passed, &out[i].Total, &out[i].WallMS, &out[i].EventsGZ); err != nil {
			return ReviewSide{}, ReviewSide{}, err
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return ReviewSide{}, ReviewSide{}, err
	}
	if i != 2 {
		return ReviewSide{}, ReviewSide{}, fmt.Errorf("对局 %d 双侧数据不全（%d 行）", matchID, i)
	}
	return out[0], out[1], nil
}
```

注意：`rows.Scan` 里第一个 CASE 列声明了但未使用——Scan 需要占位接收。把第一列 Scan 进一个局部变量：在循环体内

```go
	var side string
	if err := rows.Scan(&side, &out[i].AgentID, &out[i].Agent,
		&out[i].Passed, &out[i].Total, &out[i].WallMS, &out[i].EventsGZ); err != nil {
```

（`side` 值与 `out[i]` 的 A/B 序由 ORDER BY 保证一致，不必再校验；如实现者想加固，可校验 `side` 与序号匹配并在不匹配时报错。）

- [ ] **Step 4: 运行确认通过**

Run: `go build ./... && go vet ./... && go test -race ./platform/internal/store/`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add platform/internal/store/store.go platform/internal/store/store_test.go
git commit -m "feat: store.ReviewData 复盘双侧 JOIN 查询

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 4: api 公开路由 GET /api/matches/{id}/review

**Files:**
- Modify: `platform/internal/api/api.go:39`（路由注册）、文件末尾（handleReview）
- Test: `platform/internal/api/api_review_test.go`（新建）

- [ ] **Step 1: 写失败测试**

创建 `platform/internal/api/api_review_test.go`：

```go
// api_review_test.go —— GET /api/matches/{id}/review 三分支与内容断言。
package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"agentbattle/protocol"
)

// gzEventsB64 构造 gzip NDJSON 事件流并 base64（上报 body 用）。
func gzEventsB64(t *testing.T, events ...protocol.Event) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// playMatchWithEvents 与 playMatch 同流程，但双侧带事件流。
func playMatchWithEvents(t *testing.T, srv *http.Server, base string) int64 {
	t.Helper()
	tokA := register(t, testingHTTPT(srv), "ra")
	tokB := register(t, testingHTTPT(srv), "rb")
	return 0 // 占位，见下方真实实现
}
```

上面两个 helper 是**占位，不要保留**——测试直接内联流程更清晰。删除上面整个 `playMatchWithEvents`，真实测试如下（追加到同文件）：

```go
type reviewResp struct {
	MatchID  int64  `json:"match_id"`
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
	Status   string `json:"status"`
	Winner   string `json:"winner"`
	Sides    map[string]struct {
		Agent     string `json:"agent"`
		Passed    int    `json:"passed"`
		Total     int    `json:"total"`
		WallMS    int64  `json:"wall_ms"`
		HasEvents bool   `json:"has_events"`
	} `json:"sides"`
	Timeline map[string][]struct {
		Seq  int    `json:"seq"`
		Type string `json:"type"`
		Path string `json:"path"`
	} `json:"timeline"`
	Marks map[string][]struct {
		Kind string `json:"kind"`
		Seq  int    `json:"seq"`
	} `json:"marks"`
	Compare map[string]struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	} `json:"compare"`
}

// TestReviewFlow 已结算对局复盘内容断言 + 400/404/409（pending、aborted）分支。
func TestReviewFlow(t *testing.T) {
	srv, st := newServerWithStore(t)

	// 一场带事件流的对局：A 有 tool_call+error+file_edit，B 仅 tool_call
	tokA := register(t, srv, "ra")
	tokB := register(t, srv, "rb")
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "ra", "agent_b": "rb"})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局: %d", resp.StatusCode)
	}
	mid := int64(m["match_id"].(float64))
	up := func(tok, side string, passed int, events []protocol.Event) {
		t.Helper()
		body := map[string]any{"side": side, "passed": passed, "total": 2, "wall_ms": 100}
		if events != nil {
			body["events_gz_base64"] = gzEventsB64(t, events...)
		}
		resp, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid), tok, body)
		if resp.StatusCode != 200 {
			t.Fatalf("上报 %s: %d", side, resp.StatusCode)
		}
	}
	up(tokA, "a", 2, []protocol.Event{
		{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 5},
		{Seq: 2, Type: protocol.EventError},
		{Seq: 3, Type: protocol.EventFileEdit, Note: "src/a.go"},
	})
	up(tokB, "b", 1, []protocol.Event{
		{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 3},
	})

	// 200：完整内容断言
	resp, m = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid), "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("复盘查询: %d", resp.StatusCode)
	}
	b, _ := json.Marshal(m)
	var rr reviewResp
	if err := json.Unmarshal(b, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.MatchID != mid || rr.TaskID != "demo" || rr.TaskType != "general" ||
		rr.Status != "done" || rr.Winner != "a" {
		t.Fatalf("元信息不符: %+v", rr)
	}
	if sa := rr.Sides["a"]; sa.Agent != "ra" || sa.Passed != 2 || !sa.HasEvents {
		t.Fatalf("sides.a 不符: %+v", sa)
	}
	if ta := rr.Timeline["a"]; len(ta) != 3 || ta[2].Path != "src/a.go" {
		t.Fatalf("timeline.a 不符: %+v", ta)
	}
	if tb := rr.Timeline["b"]; len(tb) != 1 {
		t.Fatalf("timeline.b 不符: %+v", tb)
	}
	if mk := rr.Marks["a"]; len(mk) != 1 || mk[0].Kind != "first_error" || mk[0].Seq != 2 {
		t.Fatalf("marks.a 不符: %+v", mk)
	}
	if len(rr.Marks["b"]) != 0 {
		t.Fatalf("marks.b 应空: %+v", rr.Marks["b"])
	}
	if rr.Compare["errors"].A != 1 || rr.Compare["edits"].A != 1 ||
		rr.Compare["tool_calls"].B != 1 || rr.Compare["tokens"].A != 5 ||
		rr.Compare["pass_ratio"].A != 1 || rr.Compare["pass_ratio"].B != 0.5 {
		t.Fatalf("compare 不符: %+v", rr.Compare)
	}

	// 400：matchID 非数字
	if resp, _ = do(t, "GET", srv.URL+"/api/matches/abc/review", "", nil); resp.StatusCode != 400 {
		t.Fatalf("非数字 id 应 400, got %d", resp.StatusCode)
	}
	// 404：对局不存在
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/99999/review", srv.URL), "", nil); resp.StatusCode != 404 {
		t.Fatalf("不存在对局应 404, got %d", resp.StatusCode)
	}

	// 409 × 2：pending 与 aborted
	_, m2 := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "ra", "agent_b": "rb"})
	mid2 := int64(m2["match_id"].(float64))
	resp2, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid2), tokA,
		map[string]any{"side": "a", "passed": 1, "total": 2, "wall_ms": 1})
	if resp2.StatusCode != 200 {
		t.Fatalf("pending 对局单侧上报: %d", resp2.StatusCode)
	}
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid2), "", nil); resp.StatusCode != 409 {
		t.Fatalf("pending 对局应 409, got %d", resp.StatusCode)
	}
	if _, err := st.SweepStaleMatches(-time.Minute); err != nil { // 负阈值清扫一切 pending → aborted
		t.Fatal(err)
	}
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid2), "", nil); resp.StatusCode != 409 {
		t.Fatalf("aborted 对局应 409, got %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./platform/internal/api/ -run TestReviewFlow`
Expected: 404（路由未注册时 Go mux 对 GET /api/matches/{id}/review 无匹配 → 404/405，断言 200 失败即红）。

- [ ] **Step 3: 注册路由并实现 handleReview**

`platform/internal/api/api.go` 第 39 行 `handleProfile` 路由后加一行：

```go
	mux.HandleFunc("GET /api/matches/{id}/review", s.handleReview)
```

文件末尾追加：

```go
// handleReview GET /api/matches/{id}/review：对局复盘报告（公开路由，同天梯/
// 画像——事件流只含路径与操作类型，无内容明文，无泄露风险）。
// 404 对局不存在 / 409 非 done（pending 未结算、aborted 孤儿）→ 复盘只对
// 已结算对局有意义；matchID 非数字 → 400。
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	matchID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "match id 非法")
		return
	}
	taskID, _, _, status, winner, err := s.St.MatchByID(matchID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "对局不存在")
		return
	}
	if status != "done" {
		writeErr(w, http.StatusConflict, "对局未结算，暂无复盘")
		return
	}
	tt, err := s.St.TaskTypeOf(matchID)
	if err != nil {
		log.Printf("对局 %d 读 task_type 失败: %v", matchID, err)
		writeErr(w, http.StatusInternalServerError, "读对局失败")
		return
	}
	sa, sb, err := s.St.ReviewData(matchID)
	if err != nil {
		log.Printf("对局 %d 读复盘数据失败: %v", matchID, err)
		writeErr(w, http.StatusInternalServerError, "读复盘数据失败")
		return
	}
	rep := review.BuildReport(
		review.Meta{MatchID: matchID, TaskID: taskID, TaskType: tt, Status: status, Winner: winner},
		review.SideInput{Agent: sa.Agent, Passed: sa.Passed, Total: sa.Total, WallMS: sa.WallMS, EventsGZ: sa.EventsGZ},
		review.SideInput{Agent: sb.Agent, Passed: sb.Passed, Total: sb.Total, WallMS: sb.WallMS, EventsGZ: sb.EventsGZ},
	)
	writeJSON(w, http.StatusOK, rep)
}
```

并在 api.go import 块加入 `"agentbattle/platform/internal/review"`（按字母序插在 profile 与 store 之间）。

- [ ] **Step 4: 运行确认通过**

Run: `go build ./... && go vet ./... && go test -race ./platform/internal/api/`
Expected: 全部 PASS（含既有测试）。

- [ ] **Step 5: Commit**

```bash
git add platform/internal/api/api.go platform/internal/api/api_review_test.go
git commit -m "feat: 公开路由 GET /api/matches/{id}/review（404/409/200 三分支）

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 5: runner 侧 —— client.Review + review 子命令

**Files:**
- Modify: `runner/internal/client/client.go`（Profile 方法后追加类型与方法）
- Modify: `runner/cmd/agentbattle/main.go:4-6,26,48-49`（doc/usage/switch 三处注册）
- Create: `runner/cmd/agentbattle/review.go`
- Test: `runner/internal/client/client_test.go`（末尾追加）、`runner/cmd/agentbattle/review_test.go`（新建）

- [ ] **Step 1: 写 client 失败测试**

在 `runner/internal/client/client_test.go` 末尾追加（沿用该文件既有的 httptest 风格）：

```go
// TestReview 复盘拉取：正常解码与 404 错误透传。
func TestReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/matches/7/review" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"match_id":7,"task_id":"fix-add","task_type":"general",
			"status":"done","winner":"a",
			"sides":{"a":{"agent":"A","passed":2,"total":2,"wall_ms":100,"has_events":true},
			         "b":{"agent":"B","passed":1,"total":2,"wall_ms":200,"has_events":false}},
			"timeline":{"a":[{"seq":1,"ts":0,"type":"tool_call","tool":"bash","tokens":5}],"b":[]},
			"marks":{"a":[{"kind":"first_error","seq":2}],"b":[]},
			"compare":{"pass_ratio":{"a":1,"b":0.5},"tool_calls":{"a":1,"b":0},
			           "errors":{"a":1,"b":0},"edits":{"a":0,"b":0},
			           "tokens":{"a":5,"b":0},"wall_ms":{"a":100,"b":200}}}`)
	}))
	defer srv.Close()
	rep, err := New(srv.URL).Review(7)
	if err != nil {
		t.Fatal(err)
	}
	if rep.MatchID != 7 || rep.Winner != "a" || rep.Sides["a"].Agent != "A" ||
		!rep.Sides["a"].HasEvents || rep.Timeline["a"][0].Tool != "bash" ||
		rep.Marks["a"][0].Kind != "first_error" || rep.Compare.PassRatio.B != 0.5 {
		t.Fatalf("复盘解码不符: %+v", rep)
	}
	// 404 透传为错误
	if _, err := New(srv.URL).Review(99); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 应透传为错误: %v", err)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./runner/internal/client/ -run TestReview`
Expected: 编译失败（`rep.Review undefined`）。

- [ ] **Step 3: 实现 client.Review**

在 `runner/internal/client/client.go` 的 `Profile` 方法之后（`GzipEvents` 之前）追加：

```go
// ReviewSideOut 复盘单侧概要。
type ReviewSideOut struct {
	Agent     string `json:"agent"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	WallMS    int64  `json:"wall_ms"`
	HasEvents bool   `json:"has_events"`
}

// ReviewTlEvent 复盘时间线事件（runner 不能导入 platform/internal/review
// ——internal 边界，本地定义最小解析形态，与平台侧 JSON 契约对齐）。
type ReviewTlEvent struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"`
	Type       string `json:"type"`
	Tool       string `json:"tool"`
	Path       string `json:"path"`
	DurationMS int64  `json:"duration_ms"`
	Tokens     int    `json:"tokens"`
}

// ReviewMark 关键节点标注（M2 仅 first_error）。
type ReviewMark struct {
	Kind string `json:"kind"`
	Seq  int    `json:"seq"`
}

// ReviewPair 一项对比指标的 A/B 值。
type ReviewPair struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

// ReviewCompare 双侧行为对比。
type ReviewCompare struct {
	PassRatio ReviewPair `json:"pass_ratio"`
	ToolCalls ReviewPair `json:"tool_calls"`
	Errors    ReviewPair `json:"errors"`
	Edits     ReviewPair `json:"edits"`
	Tokens    ReviewPair `json:"tokens"`
	WallMS    ReviewPair `json:"wall_ms"`
}

// ReviewReport 对局复盘报告。
type ReviewReport struct {
	MatchID  int64                      `json:"match_id"`
	TaskID   string                     `json:"task_id"`
	TaskType string                     `json:"task_type"`
	Status   string                     `json:"status"`
	Winner   string                     `json:"winner"`
	Sides    map[string]ReviewSideOut   `json:"sides"`
	Timeline map[string][]ReviewTlEvent `json:"timeline"`
	Marks    map[string][]ReviewMark    `json:"marks"`
	Compare  ReviewCompare              `json:"compare"`
}

// Review 拉取指定对局的复盘报告（公开路由，无需 token）。
func (c *Client) Review(matchID int64) (ReviewReport, error) {
	var out ReviewReport
	path := fmt.Sprintf("/api/matches/%d/review", matchID)
	if err := c.do(http.MethodGet, path, "", nil, &out); err != nil {
		return ReviewReport{}, err
	}
	return out, nil
}
```

- [ ] **Step 4: client 测试转绿**

Run: `go test -race ./runner/internal/client/`
Expected: 全部 PASS。

- [ ] **Step 5: 写 CLI 失败测试**

创建 `runner/cmd/agentbattle/review_test.go`：

```go
// review_test.go —— CLI review 子命令的渲染与负路径测试。
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbattle/runner/internal/client"
)

// sampleReview 构造一份最小复盘报告。
func sampleReview() client.ReviewReport {
	return client.ReviewReport{
		MatchID: 7, TaskID: "fix-add", TaskType: "general", Status: "done", Winner: "a",
		Sides: map[string]client.ReviewSideOut{
			"a": {Agent: "echoA", Passed: 2, Total: 2, WallMS: 100, HasEvents: true},
			"b": {Agent: "echoB", Passed: 1, Total: 2, WallMS: 200, HasEvents: false},
		},
		Timeline: map[string][]client.ReviewTlEvent{
			"a": {
				{Seq: 1, Type: "tool_call", Tool: "bash", Tokens: 5},
				{Seq: 2, Type: "error"},
				{Seq: 3, Type: "file_edit", Path: "src/a.go", DurationMS: 12},
			},
			"b": {},
		},
		Marks: map[string][]client.ReviewMark{
			"a": {{Kind: "first_error", Seq: 2}},
			"b": {},
		},
		Compare: client.ReviewCompare{
			PassRatio: client.ReviewPair{A: 1, B: 0.5},
			ToolCalls: client.ReviewPair{A: 1, B: 0},
			Errors:    client.ReviewPair{A: 1, B: 0},
			Edits:     client.ReviewPair{A: 1, B: 0},
			Tokens:    client.ReviewPair{A: 5, B: 0},
			WallMS:    client.ReviewPair{A: 100, B: 200},
		},
	}
}

// TestPrintReview 渲染：胜者名、结论行、对比表、★标注、无事件侧 "—"
// 与"（无事件流）"提示、平局文案。
func TestPrintReview(t *testing.T) {
	var buf bytes.Buffer
	printReview(&buf, sampleReview())
	out := buf.String()
	for _, want := range []string{
		"对局 7", "fix-add", "general", "胜者 echoA",
		"通过率", "tool_call", "tokens", "wall_ms",
		"echoA", "echoB", "时间线", "#2 error", "★", "src/a.go",
		"（无事件流）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺 %q:\n%s", want, out)
		}
	}
	// 无事件侧的 event 类指标渲染 "—"（tokens 行 B 列）
	if !strings.Contains(out, "—") {
		t.Fatalf("无事件侧 event 类指标应渲染 —:\n%s", out)
	}

	// 平局：winner="tie" → 胜者显示 平局
	rep := sampleReview()
	rep.Winner = "tie"
	buf.Reset()
	printReview(&buf, rep)
	if !strings.Contains(buf.String(), "胜者 平局") {
		t.Fatalf("平局应显示 平局:\n%s", buf.String())
	}
}

// TestCmdReviewNegative 必填校验与 404/409 错误透传。
func TestCmdReviewNegative(t *testing.T) {
	if err := cmdReview([]string{}); err == nil {
		t.Fatal("--server 缺失应报错")
	}
	if err := cmdReview([]string{"--server", "http://x"}); err == nil {
		t.Fatal("--match 缺失应报错")
	}
	if err := cmdReview([]string{"--server", "http://x", "--match", "0"}); err == nil {
		t.Fatal("--match 0 应报错")
	}
	if err := cmdReview([]string{"--server", "http://x", "--match", "-3"}); err == nil {
		t.Fatal("--match 负数应报错")
	}

	// 404 透传
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	if err := runReview(&buf, srv.URL, 99); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 应透传: %v", err)
	}

	// 正常路径：200 返回最小合法报告，渲染不报错
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"match_id":1,"task_id":"t","task_type":"general","status":"done",
			"winner":"a",
			"sides":{"a":{"agent":"A","passed":1,"total":1,"wall_ms":1,"has_events":false},
			         "b":{"agent":"B","passed":1,"total":1,"wall_ms":1,"has_events":false}},
			"timeline":{"a":[],"b":[]},"marks":{"a":[],"b":[]},
			"compare":{"pass_ratio":{"a":1,"b":1},"tool_calls":{"a":0,"b":0},
			           "errors":{"a":0,"b":0},"edits":{"a":0,"b":0},
			           "tokens":{"a":0,"b":0},"wall_ms":{"a":1,"b":1}}}`)
	}))
	defer ok.Close()
	buf.Reset()
	if err := runReview(&buf, ok.URL, 1); err != nil {
		t.Fatalf("正常复盘不应报错: %v", err)
	}
	if !strings.Contains(buf.String(), "对局 1") {
		t.Fatalf("应渲染结论行:\n%s", buf.String())
	}
}
```

- [ ] **Step 6: 运行确认失败**

Run: `go test -race ./runner/cmd/agentbattle/ -run 'TestPrintReview|TestCmdReviewNegative'`
Expected: 编译失败（`printReview`/`cmdReview`/`runReview` undefined）。

- [ ] **Step 7: 实现 review.go**

创建 `runner/cmd/agentbattle/review.go`：

```go
// review.go 实现 agentbattle review 子命令：按 matchID 查询并打印对局复盘。
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"agentbattle/runner/internal/client"
)

// cmdReview 解析参数并调 runReview 打印复盘。
func cmdReview(args []string) error {
	fs := newFlagSet("review")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	match := fs.Int64("match", 0, "对局 ID（必填，正整数）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" {
		return fmt.Errorf("--server 必填")
	}
	if *match <= 0 {
		return fmt.Errorf("--match 必填（正整数）")
	}
	return runReview(os.Stdout, *server, *match)
}

// runReview 拉取复盘并打印。
func runReview(w io.Writer, server string, matchID int64) error {
	rep, err := client.New(server).Review(matchID)
	if err != nil {
		return err
	}
	printReview(w, rep)
	return nil
}

// printReview 渲染复盘：结论行 + 双侧概要 + 对比表 + 双侧时间线。
// 无事件侧的 event 类对比指标渲染 "—"（与 profile 的缺维占位一致）。
func printReview(w io.Writer, rep client.ReviewReport) {
	winner := "平局"
	if rep.Winner == "a" || rep.Winner == "b" {
		if s, ok := rep.Sides[rep.Winner]; ok {
			winner = s.Agent
		}
	}
	fmt.Fprintf(w, "对局 %d · %s（%s）· 胜者 %s\n",
		rep.MatchID, rep.TaskID, rep.TaskType, winner)
	for _, side := range []string{"a", "b"} {
		s, ok := rep.Sides[side]
		if !ok {
			continue
		}
		ev := "无"
		if s.HasEvents {
			ev = "有"
		}
		fmt.Fprintf(w, "  %s %s: %d/%d wall %dms 事件流 %s\n",
			strings.ToUpper(side), s.Agent, s.Passed, s.Total, s.WallMS, ev)
	}

	hasEvents := func(side string) bool {
		s, ok := rep.Sides[side]
		return ok && s.HasEvents
	}
	cell := func(v float64, has bool) string {
		if !has {
			return "—"
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	evA, evB := hasEvents("a"), hasEvents("b")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "指标\tA\tB")
	fmt.Fprintf(tw, "通过率\t%.2f\t%.2f\n", rep.Compare.PassRatio.A, rep.Compare.PassRatio.B)
	fmt.Fprintf(tw, "tool_call\t%s\t%s\n", cell(rep.Compare.ToolCalls.A, evA), cell(rep.Compare.ToolCalls.B, evB))
	fmt.Fprintf(tw, "error\t%s\t%s\n", cell(rep.Compare.Errors.A, evA), cell(rep.Compare.Errors.B, evB))
	fmt.Fprintf(tw, "file_edit\t%s\t%s\n", cell(rep.Compare.Edits.A, evA), cell(rep.Compare.Edits.B, evB))
	fmt.Fprintf(tw, "tokens\t%s\t%s\n", cell(rep.Compare.Tokens.A, evA), cell(rep.Compare.Tokens.B, evB))
	fmt.Fprintf(tw, "wall_ms\t%.0f\t%.0f\n", rep.Compare.WallMS.A, rep.Compare.WallMS.B)
	tw.Flush()

	for _, side := range []string{"a", "b"} {
		evs := rep.Timeline[side]
		star := map[int]bool{}
		for _, mk := range rep.Marks[side] {
			if mk.Kind == "first_error" {
				star[mk.Seq] = true
			}
		}
		agent := side
		if s, ok := rep.Sides[side]; ok {
			agent = s.Agent
		}
		fmt.Fprintf(w, "时间线 %s（★=首次报错）:\n", agent)
		if len(evs) == 0 {
			fmt.Fprintln(w, "  （无事件流）")
			continue
		}
		for _, e := range evs {
			line := fmt.Sprintf("  #%d %s", e.Seq, e.Type)
			if e.Tool != "" {
				line += " " + e.Tool
			}
			if e.Path != "" {
				line += " " + e.Path
			}
			if e.DurationMS > 0 {
				line += fmt.Sprintf(" %dms", e.DurationMS)
			}
			if e.Tokens > 0 {
				line += fmt.Sprintf(" %dtok", e.Tokens)
			}
			if star[e.Seq] {
				line += " ★"
			}
			fmt.Fprintln(w, line)
		}
	}
}
```

注意 `strings.ToUpper` 需要 `"strings"` import——把它加进 review.go 的 import 块。

- [ ] **Step 8: main.go 三处注册**

`runner/cmd/agentbattle/main.go`：

1. 顶部 doc 注释第 5 行 `// profile（查看能力画像）。` 改为：

```go
// profile（查看能力画像）、review（查看对局复盘）。
```

2. usage 字符串中 `  agentbattle profile  --server URL --name X` 后加一行：

```
  agentbattle review   --server URL --match N
```

3. switch 中 `case "profile":` 分支后加：

```go
	case "review":
		err = cmdReview(os.Args[2:])
```

- [ ] **Step 9: 运行确认通过**

Run: `go build ./... && go vet ./... && go test -race ./runner/...`
Expected: 全部 PASS。

- [ ] **Step 10: Commit**

```bash
git add runner/internal/client/client.go runner/internal/client/client_test.go \
        runner/cmd/agentbattle/review.go runner/cmd/agentbattle/review_test.go \
        runner/cmd/agentbattle/main.go
git commit -m "feat: runner CLI review 子命令与 client.Review

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 6: E2E 验收 + 收官

**Files:**
- Modify: `runner/e2e/platform_e2e_test.go`（末尾追加 TestReviewReport）
- Modify: `CHANGE.md`、`CLAUDE.md`（收官记录）

- [ ] **Step 1: 写 E2E 测试**

在 `runner/e2e/platform_e2e_test.go` 末尾追加（复用同文件既有 helper：findRepoRoot/buildBin/copyDir/freePort/waitHTTP/extractToken）：

```go
// TestReviewReport M2 计划 2 验收：镜像 1 局后 review 子命令按 matchID 查得
// 结构完整的复盘——双方 agent 名、结论行胜者、对比表、双侧时间线非空。
// 标注（first_error）依赖事件流含 error，真实对局不确定，由 review 包单测
// 确定性覆盖；此处只断结构。
func TestReviewReport(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过黑盒 E2E")
	}
	root := findRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "agentbattle.exe")
	buildBin(t, root, bin, "./runner/cmd/agentbattle")
	serverBin := filepath.Join(t.TempDir(), "agentbattle-server.exe")
	buildBin(t, root, serverBin, "./platform/cmd/agentbattle-server")

	tasksDir := t.TempDir()
	if err := copyDir(filepath.Join(root, "examples", "fix-add"), filepath.Join(tasksDir, "fix-add")); err != nil {
		t.Fatalf("拷贝示例任务失败: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "review-e2e.db")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	srvCmd := exec.Command(serverBin, "--addr", addr, "--tasks", tasksDir, "--store", dbPath)
	srvCmd.Dir = root
	var srvOut bytes.Buffer
	srvCmd.Stdout = &srvOut
	srvCmd.Stderr = &srvOut
	if err := srvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srvCmd.Process.Kill()
		srvCmd.Wait()
		if t.Failed() {
			t.Logf("server 输出:\n%s", srvOut.String())
		}
	})
	waitHTTP(t, "http://"+addr+"/api/ladder")

	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	tokA := extractToken(t, run("register", "--server", "http://"+addr, "--name", "revA"))
	tokB := extractToken(t, run("register", "--server", "http://"+addr, "--name", "revB"))

	taskOut := filepath.Join(t.TempDir(), "task")
	run("fetch", "--server", "http://"+addr, "--task", "fix-add", "--out", taskOut)
	run("mirror",
		"--server", "http://"+addr,
		"--task", taskOut,
		"--task-id", "fix-add",
		"--agent", "echo",
		"--fix-a", "add() { echo $(( $1 + $2 )); }",
		"--rounds", "1",
		"--name-a", "revA", "--token-a", tokA,
		"--name-b", "revB", "--token-b", tokB,
		"--out", filepath.Join(t.TempDir(), "reports"))

	// 独立起服首个对局 id 恒为 1；--fix-a 判 a 胜（与 TestPlatformLoopEcho
	// 同一确定性来源：A 2/2 > B 1/2）
	rep := run("review", "--server", "http://"+addr, "--match", "1")
	for _, want := range []string{
		"对局 1", "revA", "revB", "胜者 revA", "通过率", "时间线",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("复盘输出缺 %q:\n%s", want, rep)
		}
	}

	// 负路径：不存在的对局 → 非零退出且错误含 404
	c := exec.Command(bin, "review", "--server", "http://"+addr, "--match", "999")
	if b, err := c.CombinedOutput(); err == nil {
		t.Fatalf("不存在对局应报错:\n%s", b)
	} else if !strings.Contains(string(b), "404") {
		t.Fatalf("错误应含 404: %v\n%s", err, b)
	}
}
```

- [ ] **Step 2: 运行 E2E**

Run: `go test -race ./runner/e2e/ -run 'TestReviewReport|TestPlatformLoopEcho' -v`
Expected: 两个测试 PASS（TestPlatformLoopEcho 回归确认无破坏）。

- [ ] **Step 3: 全量验证**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: 全部 PASS（short 模式外全量，含其余 E2E）。

- [ ] **Step 4: 更新 CHANGE.md 与 CLAUDE.md**

`CHANGE.md` 末尾追加迭代条目（日期 2026-09-13、主题"M2 计划 2：对局复盘报告"、核心变更点：review 纯函数包 / GET /api/matches/{id}/review 公开路由 / runner review 子命令 / profile.DecodeEvents 导出复用 / E2E TestReviewReport；遗留事项：LLM 自然语言总结留 M3+ Web Dashboard、哈希链审计 M3+）。

`CLAUDE.md` 的 CHANGE.md 简介行更新为：M1 与 M2 计划 1（六维画像）、M2 计划 2（对局复盘报告：`review` 子命令与 `GET /api/matches/{id}/review` 公开路由）均已完成；--dry-run 属 M2 后续。

- [ ] **Step 5: Commit**

```bash
git add runner/e2e/platform_e2e_test.go CHANGE.md CLAUDE.md
git commit -m "test: E2E 复盘验收 TestReviewReport；收官记录更新

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

- [ ] **Step 6: 收官报告**

向控制器汇报：全部任务完成状态、每个任务的偏离（如有）、`go build ./... && go vet ./... && go test -race ./...` 全绿证据。push 与合并 main 由控制器与用户确认后执行。

---

## Self-Review 记录

1. **规格覆盖**：§2 五项口径决策（纯规则→T2 无 LLM；平台侧→T2/T4；公开路由按 matchID→T4/T5；标注集仅 first_error→T2；单测断标注+E2E 断结构→T2/T6）全覆盖；§3 结构与四条口径→T2；§4 数据流→T3/T4/T5；§5 组件清单五项→T1(profile 导出)/T2(review 包)/T3(store)/T4(api)/T5(runner)；§6 错误处理（降级→T2 测试、超限→DecodeEvents 既有、兜底→T3 缺行零值、400→T4）；§7 测试四层→T2/T4/T5/T6；§8 红线核对无代码影响。
2. **占位符扫描**：T2 Step 1 与 T4 Step 1 各有一处**故意标注的占位代码块**（`PassRatioRep` 行 / `playMatchWithEvents`），均明确指示实现时删除，属于"先红"教学设计而非计划缺陷；其余无 TBD/TODO。
3. **类型一致性**：`profile.DecodeEvents`（T1 定义 → T2 调用）；`review.Meta/SideInput/Report`（T2 定义 → T4 调用）；`store.ReviewSide`（T3 定义 → T4 消费）；`client.ReviewReport` 及子类型（T5 定义 → T5 CLI 测试消费）；`cmdReview/runReview/printReview`（T5 测试 → T5 实现 → main.go 注册）签名一致。
