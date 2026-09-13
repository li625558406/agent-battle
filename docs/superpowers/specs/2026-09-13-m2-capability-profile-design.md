# M2 计划 1：六维能力画像管道 — 设计文档

日期：2026-09-13
状态：已获用户批准
范围声明：M2 里程碑共三件交付物（六维画像、复盘报告、--dry-run 影子赛），本文档只覆盖第一件——六维画像管道。复盘报告与 --dry-run 为后续独立规格。

## 1. 目标与验证

让平台对每个 agent 产出六维能力画像（0-100 分），基于已回传的事件流与判分结果在平台侧确定性计算。

**验证目标**（承接设计文档 M2 行）：画像复现已知差异——echo agent 注入正确解法（`--fix-a`）与空配置对手镜像对战 N 局后，前者的正确性维度显著高于后者。

## 2. 已确认的口径决策

| 决策点 | 结论 | 理由 |
|---|---|---|
| 计算位置 | 全部平台侧 | 设计红线 5（统一口径保证可比性）；Runner 可篡改自家画像，公信力不成立 |
| 指标口径 | 纯规则指标，零 LLM 依赖 | 原表"工具选择合理性（judge 抽评）"降级为规则近似并注释标注 M3+ LLM judge 接管；零外部依赖、可测试、可复现 |
| 计算时机 | settle 后同步重算，近 50 局窗口全量重算 | 管道最短、天然幂等（重算覆盖旧值，数据错了重算即修）；否决异步 worker（M2 无队列基础设施，过度设计） |
| 呈现层 | 平台 API 路由 + runner CLI `profile` 子命令 | Web Dashboard 属 M3；红线 4 headless 核心 + CLI 壳 |
| 任务类型 | task.json 增可选 `task_type`（缺省 `"general"`） | 六维"同任务类型内相对分位"依赖它；向后兼容旧任务包 |

## 3. 六维规则指标定义

每维先算每局原始分，再做**百分位归一化**（percentile rank × 100）。两个集合必须区分：

- **自身窗口**：同 agent × 同 task_type 的最近 50 局（按结算时间倒序）——决定"哪些局参与画像"与样本量
- **归一化基线**：同 task_type 下**全体 agent** 的最近 200 局已完成对局——某局某指标的分数 = 该值在基线分布中的百分位（低好指标取 100 减百分位）

基线必须是全体 agent 的池而非自身窗口：否则每个 agent 的分位恒在自身分布中心附近（均匀表现的 agent 恒得约 50 分），画像无法区分好坏，"复现已知差异"验收不成立。

样本 < 5 局时画像标注 `low_sample: true` 并附样本量。

| 维度 | 规则口径 | 方向 |
|---|---|---|
| 正确性 correctness | 窗口内 passed/total 均值 + 全通过局占比（两项各半权重合成） | 越高越好 |
| 调试能力 debugging | 含 error 事件局中的最终通过率（报错后恢复率）；无错误局不参与；自身窗口内全无错误局时该维直接记满分（raw=1） | 越高越好 |
| 工具效率 tool_efficiency | 每局 tool_call 数 + error 事件/tool_call 比率（无效调用近似）；仅事件流可用的局参与 | 越低/越低越好 |
| 成本控制 cost | tokens 总量 + wall_ms；仅事件流可用的局参与（无事件流时 tokens=0 会假性最优） | 越低越好 |
| 规划能力 planning | 首个 file_edit 前的 tool_call 占比（前置探查比 = 首 edit 前 tool_call 数 / max(总 tool_call,1)）；局内无 file_edit 时不参与该维（同调试能力的缺席处理） | 越高越好 |
| 稳定性 stability | 崩溃局（total==0，runner 既有契约：崩溃侧按 0/0 上报）0/1 值 + 通过率对窗口均值的偏离度，两项均为低好 | 越低越好 |

## 4. 架构与数据流

```
settle 完成（handleResult 双侧到齐）
  → platform/internal/profile: 提取该局六维原始指标
      （输入 = results 里已存的 events_gz + passed/total/wall_ms + err）
  → 对该 agent × task_type 近 50 局窗口全量重算 → 分位归一化 0-100
  → 写 agent_profiles 表（覆盖式，幂等）
查询：GET /api/agents/{name}/profile  →  runner CLI `profile` 子命令表格展示
```

不新增基础设施；事件流/判分结果全部已在库中，管道是纯函数式的"读历史 → 算 → 覆盖写"。

## 5. 组件设计

- **`platform/internal/profile`**（新包，纯函数核心）
  - `metrics.go`：单局提取，输入 events + report + err → `MatchMetrics`；只依赖 protocol 与 store 的读取接口，不 import api
  - `profile.go`：窗口聚合 + 分位归一化 → `Profile`（JSON 形态含六维分、每维原始值、样本量、low_sample 标注、updated_at）
- **store 扩展**
  - `matches` 表加 `task_type` 列（`CreateMatch` 时从任务目录 task.json 读入，缺省 `general`；含旧库 ALTER 迁移）
  - 新表 `agent_profiles(agent_id, task_type, sample_size, profile_json, updated_at)`，主键 (agent_id, task_type)，覆盖式 upsert
  - 提供两个查询：自身窗口（agent × task_type 近 50 局 done 对局的本方结果）、归一化基线（task_type 全体近 200 局 done 对局）
- **task_type 源头**：task.json 可选字段；mirror/fetch/bundle 链路不感知不改动
- **api**：handleResult settle 成功后调 `profile.Recompute`（失败仅 log 降级不阻断结算响应，下局重算自然修复）；新路由 `GET /api/agents/{name}/profile`，不存在 agent 与无对局空画像明确区分（404 / 空对象）
- **runner CLI**：`profile --server <url> --name <agent>` 子命令，六维表格输出（text/tabwriter，沿用 ladder 风格）

## 6. 错误处理

- 事件流缺失/解析失败：该局降级用静态数据（passed/total/wall_ms）计算正确性与成本；事件型维度记 missing，样本量如实扣减
- 画像重算失败：不阻断结算（log + 跳过）；幂等保证下次结算重算即修复
- metrics 层对抗性输入（空事件流、零 tokens、超大值、全 error 局、单元素窗口）全部有界：不 panic、不产生 NaN、除零有定义
- 画像数据只增不改不删：重算失败不会破坏旧画像

## 7. 测试策略

- **metrics 单测**：构造事件流验证各维原始分；对抗性用例（空流、零 tokens、全 error 局、超大 tokens、单元素窗口）
- **归一化单测**：1 局样本、并列值、满 50 局窗口、low_sample 标注边界（4 局 vs 5 局）
- **store 集成**：窗口重算幂等（同数据两次重算结果一致）、task_type 隔离（同 agent 不同 task_type 互不污染）、upsert 覆盖语义
- **api_test**：结算后画像更新、GET 路由正常/404/空画像三分支、重算失败不阻断结算响应
- **E2E 验收**：echo `--fix-a` vs 空 B 镜像 N 局 → `profile` 查询 A 正确性维度显著高于 B（画像复现已知差异）

## 8. 隐私与红线核对

- 平台只消费 Runner 已回传的事件流（本就不含文件内容明文），画像计算不触碰用户 CLAUDE.md/skills/记忆——红线 1、3 不受影响
- 画像按 agent 名下聚合，界面只显示 agent 名与六维分数，不涉及配置内容——归因分析（配置 A vs B）属 M4，不在本规格
