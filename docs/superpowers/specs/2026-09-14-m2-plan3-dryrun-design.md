# M2 计划 3：--dry-run 影子赛 — 设计文档

日期：2026-09-14
状态：已获用户批准
范围声明：M2 里程碑三件交付物的最后一件。前两件（六维画像、复盘报告）已合并 main。

## 1. 目标与验证

设计文档 §7.3 的隐私承诺落地："**先看平台会收到什么数据，再决定联网。透明度即获客话术。**"用户在零真实出网的前提下完整跑一局镜像对战，逐字节核查平台将要收到的每一种数据（注册、建赛、上报、事件流），作为"永不上传 CLAUDE.md/skills/记忆"承诺的可验证证明。

**验证目标**：E2E 无 `--server` 离线跑 `mirror --dry-run` 1 局——`dry_run/` 下 5 个导出文件齐全、register 含 agent 名、upload 含 passed/total 与解码后事件数组、stdout 含隐私声明行；dump 保真性（body 与请求字节一致）由单测确定性覆盖。

## 2. 已确认的口径决策

| 决策点 | 结论 | 理由 |
|---|---|---|
| 入口形态 | `mirror --dry-run` 旗标（非独立子命令） | 与真实对战同一条代码路径，"所见即所传"字面成立；两套流程必然漂移 |
| 拦截机制 | 本地回环假服务端（127.0.0.1 随机端口） | 请求走真实 HTTP 序列化路径（header/URL/压缩全真实），公信力最强；零侵入 client/session 代码 |
| 交付形态 | 导出 payload 文件 + stdout 摘要 | 人能读、机器可逐字节核查；仅 stdout 摘要不可核查，公信力弱 |
| 离线口径 | 完全离线：`--server` 给出即报错；任务用本地目录；`--task-id` 仍必填 | "再决定联网"意味着此刻不该有任何真实连接；bundle 由假服务端从本地任务目录打 zip 供给 |
| 敏感占位 | 假服务端响应的 token/judge_key 用 `dry-run-placeholder` 字面值 | 导出文件的 X-Token header 处正好展示真实对局时凭证所在位置，增强透明度 |

## 3. 假服务端响应编排与导出

mirror 流程（原封不动）发出的请求 → `127.0.0.1:<随机端口>` 假服务端，逐请求原样落盘 + 应答预编排：

| 请求 | 导出文件（`--out/dry_run/` 下，序号递增） | 预编排响应 |
|---|---|---|
| POST /api/agents | `001_register_a.json`、`002_register_b.json` | `{"id":N,"name":"<原名>","token":"dry-run-placeholder"}` |
| POST /api/matches | `003_create_match.json` | `{"match_id":N,"judge_key":"dry-run-placeholder"}` |
| POST /api/matches/{id}/results | `004_upload_a.json`、`005_upload_b.json` | 首侧 `{"status":"waiting"}`；双侧到齐 `{"status":"done","winner":"<通过率高者胜，同则 tie>","rating_a":1200,"rating_b":1200}` |

两个实现口径（计划期核实的 mirror 流程事实）：

1. **register 由 dry-run 接线代发**：mirror 本身不注册（`--server` 模式用现成 token），dry-run 模式下由 CLI 接线代码向假服务端依次注册 `--name-a`/`--name-b`，取回占位 token 供 CreateMatch/UploadResult 使用——payload 覆盖注册环节，用户能看到全部四类上传。
2. **无 bundle 路由**：mirror 流程不含 fetch 步骤（任务来自本地目录），假服务端无需应答 bundle 下载；白名单外路由一律 404 fail-fast。

导出文件结构（JSON）：`{"method","path","headers"（含 X-Token 占位凭证）,"body"（原样请求体）}`。upload 文件附加 `events` 数组——body 中 `events_gz_base64` 解压解码后的可读事件明细（路径+操作类型，无内容明文），原 base64 字段保留不动。

多轮（`--rounds N`）：每轮请求独立编号落盘，match_id 递增，流程无感。

stdout 摘要：每请求一段（编号、method、path、落盘文件、关键字段如 name/task_id/passed/事件条数），结尾一行隐私对照声明（对照 §7.3：本次全程未出网；事件流仅含路径与操作类型，不含文件内容与用户配置）。

## 4. 架构与数据流

```
mirror --dry-run --task <本地任务目录> --task-id ID ...
  → dryrun.NewServer(bundleDir) → 绑 127.0.0.1:0
  → client.New("http://127.0.0.1:<port>")（mirror 其余流程零改动）
      │ register / createMatch / uploadResult（真实序列化）
      ▼
  假服务端：dumpRecorder 原样落盘 → 预编排响应
      │  upload 额外解析 events_gz_base64 → 附加 events 数组
      ▼
  --out/dry_run/*.json + stdout 摘要
```

复盘是只读投影，影子赛同理是"录制投影"：不改平台、不改协议、不动 mirror 主流程。

## 5. 组件设计

- **`runner/internal/dryrun`**（新包，隔离全部影子赛逻辑）
  - `Server`：`net/http` server 绑 `127.0.0.1:0`；四类路由预编排响应（§3 表）；`Base()` 返回 `http://127.0.0.1:<port>` 供 client 接线；`Close()` 释放
  - `dumpRecorder`：逐请求落盘——method/path/headers/body 原样记录；文件名白名单序号命名（无用户输入拼路径）；单文件 10MB 帽（防超大 body 刷盘）
  - upload 请求：解析 body → `events_gz_base64` 解压解码为 `events` 数组附加进导出文件（解压失败保留原字段 + stdout 标注，不中断）
  - bundle 路由：本地任务目录递归打 zip 应答
- **`runner/cmd/agentbattle/mirror.go`**（小改）：加 `--dry-run` 布尔旗标；置位时校验互斥（`--server` 给出即报错）并构造 dryrun.Server 接线 client；usage/doc 同步
- **红线核对**：dump 全程本地 `--out`，零出网；CLI 是壳（红线 4）；事件流无内容明文（红线 3），导出文件反而让用户亲眼验证这一点

## 6. 错误处理

- 本地任务目录不存在/缺 task.json：mirror 既有预检报错，dry-run 提前失败，不产出半套 dump
- events_gz 解压失败：单请求降级（保留原 base64 字段 + stdout 标注），不中断流程——与复盘管道降级口径一致
- dump 落盘 IO 失败：立即中止 dry-run（用户要的就是文件，写不了没意义）
- 非白名单路由：404（mirror 流程不该发出别的请求，发出即说明流程漂移，fail fast）
- `--dry-run` 与 `--server` 互斥：报错退出，避免用户误以为连了真服

## 7. 测试策略

- **dryrun 单测**（对抗性优先）：四类路由响应编排正确性；dump 保真（method/path/headers/body 与请求字节一致）；对抗——非白名单路由 404、超大 body 触发 10MB 帽、畸形 events_gz 降级标注、A/B 两侧并发上报文件序号不串
- **CLI 单测**：`--dry-run` 与 `--server` 互斥报错；`--task-id` 缺失报错
- **E2E `TestDryRunMirror`**：无 `--server` 离线跑 1 局——5 个导出文件齐全、register 含 agent 名、upload 含 passed/total 与非空 events 数组、stdout 含隐私声明行；无需起真实平台（本就不给 server 地址，不起 serverBin）
