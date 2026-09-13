# M2 计划 2：对局复盘报告（纯规则） — 设计文档

日期：2026-09-13
状态：已获用户批准
范围声明：M2 里程碑共三件交付物（六维画像、复盘报告、--dry-run 影子赛），本文档只覆盖第二件——复盘报告。--dry-run 为后续独立规格。

## 1. 目标与验证

基于已入库的事件流与判分结果，对单场对局产出确定性复盘报告：双方时间线 + 关键节点标注 + 六项行为对比。零 LLM 依赖（设计文档 §6.4 的"LLM 自然语言总结"留给 M3+ 与 Web Dashboard 一同做）。

**验证目标**：E2E 镜像 1 局后，`review` 子命令按 matchID 查得结构完整的复盘（双方 agent 名、结论行 winner、对比表、双侧时间线非空）；关键节点标注逻辑由单测确定性覆盖。

## 2. 已确认的口径决策

| 决策点 | 结论 | 理由 |
|---|---|---|
| 复盘方式 | 纯规则，零 LLM 依赖 | LLM 总结不可复现、有外部依赖；留 M3+ Web Dashboard 一同做 |
| 计算位置 | 全部平台侧 | 与画像管道同构（设计红线 5 统一口径）；Runner 自算复盘可被篡改，公信力不成立 |
| 入口与公开性 | 公开路由 `GET /api/matches/{id}/review`，无需 token，CLI `review --server --match <id>` | 设计文档 §7.2"复盘报告公开"；事件流本就不含文件内容明文，公开无泄露风险 |
| 关键节点标注集 | 仅 `first_error`（首个 error 事件的 seq） | "大回滚"无事件语义（事件流只有 file_edit，无回滚/删除）；"转折点"无规则口径可判；均砍掉，M3+ LLM judge 可接管 |
| 验收标准 | 单测断标注 + E2E 断结构 | 真实 echo agent 对局是否产生 error 不确定，E2E 无法确定性断言标注出现 |

## 3. 复盘报告结构与口径

`ReviewReport` JSON 形态（`GET /api/matches/{id}/review` 返回体）：

```json
{
  "match_id": 1,
  "task_id": "fix-add",
  "task_type": "general",
  "status": "done",
  "winner": "a",
  "sides": {
    "a": {"agent": "echoA", "passed": 2, "total": 2, "wall_ms": 123, "has_events": true},
    "b": {"agent": "echoB", "passed": 1, "total": 2, "wall_ms": 210, "has_events": true}
  },
  "timeline": {
    "a": [{"seq":1,"ts":0,"type":"tool_call","tool":"bash","path":"","duration_ms":10,"tokens":5}],
    "b": [ ... ]
  },
  "marks": {
    "a": [{"kind":"first_error","seq":3}],
    "b": []
  },
  "compare": {
    "pass_ratio": {"a":1.0,"b":0.5},
    "tool_calls": {"a":3,"b":5},
    "errors":     {"a":0,"b":1},
    "edits":      {"a":2,"b":1},
    "tokens":     {"a":45,"b":30},
    "wall_ms":    {"a":123,"b":210}
  }
}
```

口径定义：

1. **compare 指标来源**：pass_ratio/tool_calls/tokens/wall_ms 四项复用 `profile.ExtractMetrics`（PassRatio/ToolCalls/Tokens/WallMS）；errors/edits 记**事件个数**（非有无布尔），由 review 包提取时间线遍历时自数——MatchMetrics 只有 HasErrors/HasEdit 布尔，个数对复盘更有信息量。复盘与画像共用同一份提取代码，口径不可能漂移。
2. **时间线字段**：Seq/TS/Type/Tool/Path/DurationMS/Tokens。Path 仅 file_edit 事件有值（事件流本就只有路径与操作类型，无内容明文——红线 3 不受影响，公开无泄露）。**不带 PrevHash/Hash**：哈希链属 §7.2 审计功能，M3+ 再做；M2 时间线保持观众可读。
3. **first_error 标注**：该侧时间线中首个 `type=error` 事件的 Seq；无 error 事件则 marks 为空数组。
4. **事件流缺失的一侧**：timeline 为空数组、has_events=false；compare 中事件类指标（tool_calls/errors/edits/tokens）记 0，CLI 渲染为 "—"；静态数据（pass_ratio/wall_ms）照常展示。与画像管道"无事件流不做事件类计算"的降级口径一致。

## 4. 架构与数据流

```
GET /api/matches/{id}/review（handleReview）
  → store.MatchByID（404 不存在 / 409 非 done）
  → store.ReviewData(matchID)：双侧 agent 名 + passed/total/wall_ms + events_gz
      （一条 JOIN 查询；事件解码复用 profile 包导出的 DecodeEvents）
  → review.BuildReport(...)：纯函数组装 ReviewReport
      timeline/marks 逐侧提取；compare 调 profile.ExtractMetrics
  → JSON 返回
runner CLI：review --server <url> --match <id>
  → client.Review(matchID) → tabwriter 渲染（结论行 + 对比表 + 双侧时间线，first_error 标 ★）
```

复盘是只读投影：事件与判分结果全部已在库中，按需计算，不预生成、不改表结构、无迁移。

## 5. 组件设计

- **`platform/internal/review`**（新包，纯函数核心）
  - `report.go`：`ReviewReport` 及子结构 + `BuildReport(meta, sideA, sideB)`；只依赖 protocol 与 profile 包，不 import api/store。BuildReport 内逐侧遍历事件一次：提取时间线、数 errors/edits、找 first_error
- **profile 包微调**：`decodeEvents` 导出为 `DecodeEvents`（limitErrReader 16MB 上限逻辑原样），recompute 调用点同步改名——零行为变更
- **store 扩展**：新增 `ReviewData(matchID)` 一条 JOIN 查询（matches × results × agents），不改表
- **api**：`GET /api/matches/{id}/review` 公开路由，三分支：404（对局不存在）/ 409（pending/aborted 未结算）/ 200；matchID 非数字 → 400
- **runner**：`client.Review(matchID)`（GET 复用 client 基础设施）+ `cmd/agentbattle/review.go`（`runReview(w io.Writer, server string, matchID int64)`，对齐 runProfile 模式，main.go 三处注册）

## 6. 错误处理

- 事件流缺失/解码失败：整侧降级（§3 口径 4），不带残缺数据进报告
- 解压超限：DecodeEvents 整流返回 nil，同上降级
- 单侧结果缺失（done 对局双侧必齐，防御性兜底）：该侧按零值渲染，不 panic
- matchID 非数字：400

## 7. 测试策略

- **review 单测**：构造事件流断 first_error 标注位置（无错误 → 空 marks）；对抗性用例（空流、全 error 流、首事件即 error、超大 tokens、单事件流）
- **api_test**：三分支（404/409/200）+ 200 体内容断言（compare 值、timeline 长度、marks）
- **runner CLI 单测**：必填校验、404/409 报错文案、正常渲染含 ★ 标注与 "—" 占位
- **E2E `TestReviewReport`**：镜像 1 局后 `review --match 1` 断结构——双方 agent 名、结论行 winner、对比表非空、双侧时间线非空（不依赖不确定的 error 事件）

## 8. 隐私与红线核对

- 复盘只消费平台已入库的事件流（只有路径与操作类型，无内容明文）——红线 1、3 不受影响
- 报告公开路由不暴露 token、用户配置、文件内容；按 agent 名展示——与天梯/画像同等公开粒度
- Runner 核心 headless 化：CLI review 是薄渲染壳——红线 4 不受影响
