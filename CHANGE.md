# CHANGE.md — 项目迭代记录

## 2026-09-14 · M2 计划 3 完成：--dry-run 影子赛（M2 收官）

**主题**：`mirror --dry-run` 零出网影子赛——完整走一遍注册/建局/双侧上报流程，全部请求发往本地回环假服务端并逐字节导出，用户开赛前可亲眼核查"平台将收到的全部数据"；M2 三件交付物（六维画像、复盘报告、影子赛）全部完成

**核心变更**：
- 新包 runner/internal/dryrun：本地回环假服务端（127.0.0.1 随机端口），三路由编排——POST /api/agents（自增 id + 占位 token dry-run-placeholder）、POST /api/matches（自增 match_id）、POST /api/matches/{id}/results（首侧 waiting、双侧到齐按通过率判 winner，非 a/b 杂侧只落盘不参与结算，不用零值幻影侧）
- dump 分段确定性拼接落盘：method/path/headers 前导段 MarshalIndent 便于人读，body 以请求原始字节嵌入逐字节保真（不经 encoding/json 压缩/重缩进），events 语义等价重缩进；解码成功附加事件数组、失败记 events_decode_error 降级不中断；全程无字符串搜索/替换（不出现占位符碰撞类）
- 公开面防御：10MB 单请求 body 帽（超帽 413 不落盘）、64MB 事件流解压总量帽（防 zip 炸弹，超帽报错不静默截断）、路由白名单外 404、导出目录非空拒绝重跑（防静默覆盖审计产物）、seq 序号与 sides 状态同持互斥锁（并发安全，导出文件序与编排状态一致）
- mirror --dry-run 接线：与 --server 互斥（影子赛零出网承诺，给出 --server 会让用户误以为数据发去了平台）；--name-a/--name-b 必填且不得相同（同名注册无法分侧归因）；任务预检前移到建服之前（预检失败不产出半套 dump）；代发两次注册取占位 token 后 mirror 主流程零改动；收尾逐条打印拦截摘要 + payload 导出路径 + 隐私声明（"以上即平台将收到的全部数据。全程未出网；事件流仅含路径与操作类型，不含文件内容与用户配置"）；不拉天梯（平台→runner 方向不属于"平台会收到的数据"）
- E2E TestDryRunMirror（runner/e2e/platform_e2e_test.go）：黑盒 exec 真实二进制，零出网跑通 1 局（--fix-a 判 A 2/2、B 1/2，与既有 E2E 同一确定性来源）→ 断言 stdout 摘要与隐私声明、5 个 payload 文件齐全且关键字段正确（含 upload 文件 X-Token 头的占位凭证）、互斥负路径非零退出
- 实测校准断言形态：dump 前导段为缩进 JSON，body 为请求原始 compact 字节（`"name":"dryA"` 无空格）；dry-run-placeholder 只出现在 upload 文件的 X-Token 请求头（register 是被拦截的请求，token 在响应侧不入 dump）

**遗留事项**：
- M2 三件交付物全部完成；M3 公测项（匹配系统、盲评、Web Dashboard）待启动
- M3 Web Dashboard 可复用 dump 文件做"上传预览"页

## 2026-09-13 · M2 计划 2 完成：对局复盘报告

**主题**：任一已结算对局可按 matchID 查得结构完整的复盘报告——双方对比（通过率/耗时/崩溃/分数）+ 双侧事件时间线，CLI `review` 子命令与平台公开路由双端可查，E2E TestReviewReport 验收通过

**核心变更**：
- 新包 platform/internal/review：纯函数 BuildReport（profile.DecodeEvents 导出复用 + store.ReviewData → 复盘结构），无 IO 依赖、单测确定性覆盖（first_error 标注等真实对局不确定的分支在此层覆盖）
- store.ReviewData：matches/results JOIN 一次取齐对局元信息 + 双侧判分结果 + 双侧事件流（压缩），自我对局显式报错
- api：公开路由 GET /api/matches/{id}/review 四分支——400（非法 id）/404（对局不存在）/409（未结算/pending）/200（报告 JSON）
- 公开面加固：时间线每侧最多展示 2000 条（超出静默截断，对比指标与首次报错标注仍按全量事件计算）、Tool/Type 钳制白名单（事件流字段不可信）、wall_ms 负值守卫、渲染层控制字符净化（防终端注入）
- runner 侧：client.Review + CLI `review --server URL --match N` 子命令（结论行 + 对比表含"通过率"行 + 双侧"时间线"），404 等错误非零退出并透传状态码
- E2E TestReviewReport（`runner/e2e/platform_e2e_test.go`）：独立起服镜像 1 局（--fix-a 判 a 胜，与 TestPlatformLoopEcho 同一确定性来源）→ review 按 matchID 1 查得结论行/胜者/对比表/时间线；负路径断言不存在对局报 404

**遗留事项**：
- LLM 自然语言总结留 M3+ Web Dashboard
- 哈希链审计（复盘报告内嵌事件链校验）M3+
- --dry-run 影子赛为 M2 计划 3

## 2026-09-13 · M2 计划 1 完成：六维能力画像管道

**主题**：平台在每场对局结算后自动重建 agent 六维能力画像（0-100），API 与 CLI 可查，E2E 验收"画像复现已知差异"通过

**核心变更**：
- store：matches 加 task_type 列（旧库 ALTER 迁移，缺省 general）+ agent_profiles 覆盖式 upsert 表 + 自身窗口（agent × task_type 近 50 局）与归一化基线（task_type 全体近 200 局）两个查询
- 新包 platform/internal/profile：单局指标提取（事件流 + 判分结果 → 六维原始指标，空流/除零/NaN 有界）→ 全体池百分位归一化（低好指标取反、并列取中档、空基线取 50）→ 覆盖落库；缺席规则有定义（无错误局调试维满分、无 edit 局规划维零样本、无事件流局不参与工具/成本维）
- 六维口径（纯规则，零 LLM 依赖）：正确性（通过率+全过率）、调试（报错恢复率）、工具效率（调用量+无效调用率）、成本（tokens+耗时）、规划（前置探查比）、稳定性（崩溃率+通过率偏离度）；崩溃判定 = total==0（runner 0/0 上报既有契约）
- api：task_type 从任务包 task.json 读入（缺字段归一 general）；settle 后同步重算双方画像（失败 log 降级不阻断结算，下局自愈）；GET /api/agents/{name}/profile（404 与空画像区分）；事件流解压 16MB 上限防 gzip 炸弹
- runner CLI：profile 子命令（tabwriter 六维表，低样本标注）；规格修订——归一化基线必须是全体池而非自身窗口（否则均匀表现恒约 50 分，无法区分好坏）
- E2E 验收 TestProfileKnownDifference：--fix-a 全过 vs 空配置半过镜像 2 局 → A correctness 75 > B 25

**遗留事项**：
- 复盘报告、--dry-run 影子赛（M2 其余两件）后续规格
- 画像归一化基线在小样本任务类型下波动大（E2E 用 4 局基线）；任务类型池扩大后自然收敛
- 六维中"工具选择合理性"现为规则近似，M3+ LLM judge 接管

## 2026-09-13 · M1 计划 3 完成：平台加固收尾 + E2E 确定性升级

**主题**：并发写锁、结算竞态、孤儿对局三类平台风险清除；CLI 分侧配置使 E2E 从三分支断言收敛为确定性断言

**核心变更**：
- store DSN 追加 `_pragma=busy_timeout(5000)`：并发写锁竞争退避等待而非立即 SQLITE_BUSY（database/sql 连接池天然多连接，上报/结算并发路径可竞争）
- settle 原子幂等：事务内以条件更新抢占结算权（`UPDATE matches SET status='done' WHERE id=? AND status='pending'`，RowsAffected==0 即 no-op），全部读取移入同一事务——并发上报双侧时恰好结算一次，Elo/统计不双计（并发 AddResult 测试 10 场 × 2 goroutine 验证）
- 孤儿对局清理：`SweepStaleMatches(olderThan)` 把超时 pending 置为 aborted（不参与 Elo）；AddResult 与 API 层双重守卫拒绝对非 pending 对局上报（409 固定文案）；server `--match-timeout`（默认 30m）周期清扫，mirror 中止遗留的永远 waiting 半场对局得以收敛
- CLI mirror 新增 `--fix-a/--fix-b`：分侧 echo 解法注入（仅 --agent echo），与空配置侧形成确定性判分差
- E2E 升级：`--fix-a` 注入后 winner=a 恒定，胜方无关三分支断言（1220/1180/双 1200）收敛为精确断言；本地汇总行胜负平断言补齐（runner 侧 session.winner 与平台侧 winnerOf 漂移防线闭环）
- 收官顺路修正：TOCTOU 窗口（pending 检查与 AddResult 守卫之间被并发结算/清扫抢先）命中时 API 返回 409 而非误映射 500
- judge_key 按局随机明确移出 backlog（与任务包预签名机制冲突，VerifyDir 要求与 manifest 签名相同的 key；属 M2 防作弊改造范畴，见计划文档 Out of scope）

**遗留事项**：
- sentinel 类型化判别（扩展第三方 adapter 前置）、bundle 路由鉴权、judge GIT_INDEX_FILE 过滤维持 backlog
- judge `TestRunCommandTimeout` 全量并发下偶发计时抖动（存量，独立复跑稳定）

## 2026-09-13 · M1 计划 2 完成：平台最小功能 + Runner 联网对接，真实 claude 联网对战验证通过

**主题**：平台侧（注册/任务下发/结果上报/Elo 结算/基础天梯）+ Runner 联网对接全链路联通，M1 计划 2（9 任务）完成

**核心变更**：
- 前置修复（收官审查遗留 3 项）：① judge 包 git 调用统一走 `gitEnv()`（过滤宿主 `GIT_DIR`/`GIT_WORK_TREE`，hashGitDiff 与判分命令两条路径均覆盖），导出 `judge.VerifyDir`（复用 verifyManifest），mirror 预检升级为验签（坏判分包不再污染 N 局统计）；② mirror ctx 取消/超时局中不计 agent 崩溃、两处取消路径均落盘部分汇总（abort helper，`errors.Is(Canceled/DeadlineExceeded)` 可识别）
- 新增 `platform/` 子树：`elo`（纯函数评级：1200 起、前 10 局 K=40、之后 K=20）→ `store`（SQLite/modernc.org/sqlite 纯 Go，唯一第三方依赖；agents/matches/results 三表、DSN 级 `PRAGMA foreign_keys=ON`、双侧到齐事务结算、winnerOf 与 runner 侧注释互指）→ `api`（REST 5 路由 + X-Token auth：注册/对局创建/结果上报/Elo 结算回传/天梯/任务包 zip 分发；taskID 防目录穿越、body 1MB 上限、错误不泄漏内部信息、禁止自我对局）→ `cmd/agentbattle-server`（--addr/--tasks/--store/--judge-key）
- Runner 侧：`runner/internal/client`（Register/CreateMatch/UploadResult/Ladder/FetchBundle 256MB 上限/ExtractBundle 三重 zip slip 防护+单文件 64MB 上限/GzipEvents 事件流上报）+ CLI register/fetch/ladder 子命令 + mirror `--server` 上报模式（`MirrorConfig.Report` 每轮回调，每轮 = 平台一场 match，崩溃侧按 0/0 上报，结束后展示天梯前 5；天梯拉取失败降级警告不改退出码）
- 黑盒 E2E（`runner/e2e/platform_e2e_test.go`，约 4s）：build CLI+server 二进制 → 随机端口起服 → 注册→fetch→mirror --server→结算→天梯 全链路；echo 双侧无差异化、胜负由 WallMS 决定，断言采用胜方无关三分支（1220/1180/双 1200）+ 天梯统计交叉校验（防 runner/platform 两套 winner 规则漂移）
- 审查驱动修复：id/match_id 按契约输出 JSON 数字（原字符串会导致联调 unmarshal 必挂）、UNIQUE 冲突与写库故障分流 409/500、bundle 错误路径 `http.ErrAbortHandler` 断连、agent_a==agent_b 400、FetchBundle 256MB 显式报错、client 协议断言/事件流 roundtrip/大条目拒绝等 6 个测试补强

**真实验证（本机 claude-code 联网镜像 2 局，CLAUDE_CODE_GIT_BASH_PATH 经 --env-a/b 注入）**：
- 2 局全部成功、零崩溃：第 1 局 winner=a（1200→1220/1180），第 2 局 winner=a（→1237.7/1162.3，定级赛 K=40 数学正确）
- 天梯 API/CLI 双端可见：claude-real-A 1238 分 2 胜、claude-real-B 1162 分 2 负
- DB 核验：results=4（2 局×双侧）、events_gz 全非空、matches done=2、winner 记录正确；本地 4 份 events.ndjson 事件链完整

**遗留事项**：
- 平台 judge_key 为固定开发密钥（dev-secret），按局随机下发划入后续里程碑
- mirror 中止语义在平台侧遗留永远 waiting 的半场 match（孤儿对局），需平台超时判负/清理机制兜底
- CLI echo 无 A/B 差异化（mirror --agent echo 时两侧同配置），单局结局由 WallMS 抖动决定——E2E 已按三分支防御；确定性 A 胜链路验证需 CLI 支持分侧配置（如 --fix-a）
- winner 规则 runner/store 双份实现的漂移风险由注释互指 + E2E 天梯交叉校验兜住；settle 幂等检查非原子（M1 单进程串行可接受）
- sentinel 匹配（errors.Is Canceled/DeadlineExceeded）理论上可被开放 adapter 接口的自含 ctx 错误误触发，当前内置 adapter 不可达，扩展 adapter 前应改类型化判别
- judge `TestRunCommandTimeout` 全量并发下偶发计时抖动（存量，独立复跑稳定）

## 2026-09-13 · mirror ctx 取消路径修复：不计 agent 崩溃 + 落盘部分汇总（M1 计划 2 · Task 2）

**主题**：修复 `Mirror` 在 ctx 取消/超时打断局中时的两处统计与数据丢失缺陷

**核心变更**：
- 缺陷 1：ctx 取消发生在局中时，双侧 `session.Run` 返回的 ctx 包装错误被错误归类 switch 计为 agent 崩溃（AErrors/BErrors++），污染天梯数据
- 缺陷 2：取消后循环顶提前 return 跳过 persistSummary，已完成局的数据全部丢失
- 修复：双 Run 完成后、错误归类 switch 之前新增中止检查（`ctx.Err() != nil` 或 errA/errB `errors.Is` Canceled/DeadlineExceeded），命中则不计崩溃、落盘部分汇总后原样返回 ctx 错误；循环顶的取消 return 同样改为先落盘；新增 `abort` helper（落盘失败用 `errors.Join` 与中止原因一并上报，不掩盖）
- 精确性依据：`session.Run` 仅在父 ctx 已取消时返回包装 `context.Canceled/DeadlineExceeded` 的错误，单局自身超时返回的 "agent 执行超时(%s)" 不包装 sentinel——`errors.Is` 判定不会误吞真实超时崩溃
- 测试（TDD 先红后绿）：`TestMirrorCancelMidRound`（slowCancelAdapter 局中取消：断言返回 Canceled、AErrors/BErrors=0、mirror-*.json 已落盘）、`TestMirrorDeadlineMidRound`（父 ctx 超时路径）；既有测试含 `TestSessionRunTimeout` 全量回归通过（`-race`）

**遗留事项**：无

## 2026-09-13 · M1 计划 1 完成：真实 Claude Code 冒烟 + 20 局镜像对战验证（M1 runner-core）

**主题**：Runner 核心闭环真实环境验证通过，M1 计划 1（runner-core）完成

**核心变更**：
- 本会话交付物全景：protocol（事件 NDJSON + HMAC 哈希链）、sandbox（临时 git 仓库沙箱）、adapter（echo / claude-code stream-json 接入）、collector（事件链追加写）、judge（判分包验签 + 超时 + DiffHash）、session（单局 + Mirror 镜像对战）、CLI（run/mirror/sign）、E2E 回归测试；历经基线 SHA 防 amend 架空、fail-fast 预检、judge 审查修复等多轮加固（见下方各条目）
- 真实 claude-code 单局冒烟通过：`run --task ./examples/fix-add --agent claude-code --yolo` 判分 **2/2**，耗时 50.8s；事件流 7 条（tool_call×5 Read/Glob/Edit/Bash + file_edit + result），哈希链完整
- 20 局镜像对战（同配置 A/B，仅 BATTLE_LABEL 差异，基线对照组）：**A 胜 11 | B 胜 9 | 平 0 | A崩 0 | B崩 0**，总耗时约 25 分钟；全部 40 局判分均 2/2 通过，胜负全部由 WallMS 耗时决胜（两侧通过数恒相同，符合"同配置应接近均分"的对照预期：11/9 偏差在二项分布噪声范围内）
- 事件流全量抽验：40 个 events.ndjson 逐行 JSON 解析 0 坏行，每局均含 tool_call/file_edit/result 且行数 6-10

**遗留事项**：
- claude CLI 在 Windows 上依赖 `CLAUDE_CODE_GIT_BASH_PATH`（须为原生反斜杠路径，如 `F:\Git\bin\bash.exe`，正斜杠会被拒绝），本机未设该变量，需经 `--env`/`--env-a`/`--env-b` 显式注入——后续 Runner 分发文档须收录此项
- 本任务难度下双方恒满分，耗时决胜使"平局率"观测失效；后续需引入难度更高/可部分得分的任务才能真正检验平局分支
- 本机 claude CLI 单局耗时在 22-75s 间波动（同任务），耗时决胜在该噪声量级下区分度有限

## 2026-09-13 · 基线 SHA 注入防 amend 架空（M1 runner-core）

**主题**：复审发现 `examples/fix-add` file-only-change 可被 `git commit --amend` 架空——agent 改 calc.sh 后 amend 根 commit，工作树干净 + `rev-list --count HEAD` 仍为 1，双测试全过且 DiffHash 与诚实零改动不可区分

**核心变更**：
- 修复原理：agent 无法伪造的基线只有执行环境自己记得的值——session 在 agent 启动**前**经 `sandbox.HeadRev` 记录 HEAD SHA，判分时校验 HEAD 未变
- sandbox 新增导出函数 `HeadRev(repoPath)`（git rev-parse HEAD，复用包内 60s 超时与 GIT_DIR/GIT_WORK_TREE 过滤，env 过滤抽取为共享 `gitEnv()` 助手）；Create 签名不变；测试覆盖 40 位 SHA 与非 git 目录报错
- judge `Run` 签名追加 `baselineSHA string`（`Run(taskDir, sandbox, baselineSHA, key)`），非空时以 `AGENTBATTLE_BASELINE_SHA` 注入每条判分命令环境（env 由 judge 进程构造，agent 进程已退出无法影响）；空串不注入（向后兼容）；新增 TestRunInjectsBaselineSHA / TestRunDetectsAmendedHistory / TestRunEmptyBaselineNoInjection，既有测试全量传入真实基线
- session.Run 在 sandbox.Create 之后、Launch 之前调 `sandbox.HeadRev` 记录基线（失败即终止本局——无可信基线则判分不可信），传给 judge.Run
- `examples/fix-add` manifest file-only-change 命令追加 `&& [ "$(git rev-parse HEAD)" = "$AGENTBATTLE_BASELINE_SHA" ]`（bash -c 单引号内部），已重签名
- 对抗验证 6 场景实测：诚实零改动 PASS、只改 calc.sh PASS、**改 calc.sh + amend FAIL（漏洞关闭）**、改 calc.sh + 普通 commit FAIL、夹带 untracked FAIL、删除 .git FAIL

**遗留事项**：
- 已知残余攻击面：agent 删整个 .git → rev-parse 失败 → 判负（可接受）；agent 无法改判分进程的 env（judge 在 agent 退出后自行注入）；`git reset --hard <伪造>` 无法绕过（HEAD SHA 由沙箱真实对象库决定，回退到基线 SHA 本身不产生 diff 也拿不到分）
- 老判分包（manifest 未引用 AGENTBATTLE_BASELINE_SHA）不受影响：env 注入无害，行为与之前一致

## 2026-09-13 · 示例任务 + CLI + 镜像对战（M1 runner-core, Task 10-12）

**主题**：`examples/fix-add` 示例任务、`agentbattle` CLI（run/mirror/sign）、session 镜像对战

**核心变更**：
- Task 10：新增 `examples/fix-add`（task.json + 带 bug 的 seed/calc.sh + tests 判分包 + HMAC sig）。判分两用例：add-basic（bash 脚本验 add 2/3、10/-4）与 file-only-change（git diff 只许动 calc.sh）；seed 刻意用位置参数 `$1 $2`（未定义变量在 bash 算术中恒 0，`$a $b` 写法会使判分失真）
- Task 11：新增 `runner/cmd/agentbattle`：run（单局，--agent echo|claude-code、--env K=V 可多次带校验）、sign（judge.SignDir）、统一 usage/输出目录（mustOutDir → TempDir/agentbattle-reports）
- Task 12：`session.Mirror` A/B 镜像对战 N 局：Summary/RoundDetail（全 snake_case json tag）+ persistSummary 落盘 `mirror-<taskid>-<ts>.json`；胜负规则：双方 0 通过直接平局 → 通过比例高者胜 → 同分比 WallMS 短者胜 → 再平局；单侧崩溃判给对方并计错误数
- CLI mirror 子命令：--env-a/--env-b 配置分化、--rounds 默认 20、pickAdapter 预检

**遗留事项**：
- winner 规则与原计划有一处偏差：双方均 0 通过时直接判平（原计划字面规则"同分比耗时"会使规格自带的 TestMirrorTieOnBothFail 永远无法确定性地得到 Ties==2）；有产出的对局仍按"通过数→耗时→平局"决胜
- CLI mirror 对 echo 两侧注入相同配置（无 FixContent），同分时结果由 wall 毫秒噪声决定，属预期行为（真实 agent 场景由 env 配置分化产生差异）

## 2026-09-13 · judge 包审查问题修复（M1 runner-core）

**主题**：`runner/internal/judge` 代码审查 Important-1/2 与 Minor 修复

**核心变更**：
- Important-1：`hashGitDiff` 不再把 `git diff HEAD` 的 ExitError（退出码 128：非 git 仓库/无基线 commit）静默吞成 `sha256("")`——任何 ExitError 都返回 error（附 stdout 摘要），杜绝判分凭证与"无改动"合法 hash 不可区分的污染
- 测试沙箱改为真实 git 仓库（git init + add -A + 基线 commit，过滤 GIT_DIR/GIT_WORK_TREE），新增 `TestRunFailsWhenSandboxNotGitRepo`；`TestRunVerifiesAndScores` 断言 DiffHash 必须反映 work.txt 真实改动（≠ 空串 hash）
- Important-2：判分命令改 `exec.CommandContext` + 包级 `testTimeout`（2 分钟），超时 → 该条 Passed=false、ExitCode=-1，不外抛（测试失败 ≠ 判分失败）；`cmd.WaitDelay=2s` 防止孤儿子进程持有 stdout 管道导致 kill 后 Wait 永久阻塞；新增 `TestRunCommandTimeout`（slow.sh sleep 5 + 注入 100ms 超时）
- M3：.gitignore 写入与幂等检查统一为 `.judge/`（带斜杠），补幂等测试
- M4：SignDir/verifyManifest 注释补充"未知字段不在签名覆盖范围内"
- M5：copyTree 注释声明覆盖语义（同名覆盖、旧文件残留、统一 0o755、符号链接解引用）

**遗留事项**：
- 无

## 2026-09-13 · 产品设计定稿（V0）

**主题**：AgentBattle 平台完整设计文档产出

**核心变更**：
- 完成赛道调研（GitHub 开源项目 + 前沿产品，三份调研报告）：确认「本地执行 + 联网多人对抗 + ELO 天梯 + 能力画像」交叉点无竞品
- 确定产品定位：本地优先的 agent 养成对战平台（开源 Runner + 官方托管平台）
- 确认产品需求：公开竞技平台 / 本地执行 / 三赛制（同题竞速、攻防、盲评）/ 四项能力分析 / 轻量审计
- 产出设计文档：`docs/superpowers/specs/2026-09-13-agent-battle-platform-design.md`（架构、Runner 设计、赛制、评分体系、防作弊、测试策略、技术栈、M1-M4 里程碑）

**遗留事项**：
- 设计已批准，待进入实现计划（writing-plans）阶段
- 技术栈（Go + Next.js + PG/Redis）为建议值，实现计划阶段最终确认

## 2026-09-13 · mirror fail-fast + CLI 细节修复 + fix-add 判分加固（M1 runner-core）

**主题**：质量审查 Important-1/2 与 Minor（M3/M5/M7/M9）修复

**核心变更**：
- Important-1：`session.Mirror` 开跑前任务级预检 fail-fast——`loadTask`（复用 session.go 同包函数）失败或 `tests/manifest.json` 缺失直接返回 error，不再把任务配置错误按"该侧 agent 崩溃"统计出垃圾汇总且 exit 0（`mirror --task ./nope` 现为 exit 1 + 明确报错）
- Important-2：`examples/fix-add` file-only-change 判分命令重写为 `git status --porcelain` 方案——旧 `git diff HEAD` 看不见 untracked 文件（审查建议命令实测被"夹带 untracked 文件"击穿），且 judge 会在沙箱写入 untracked `.gitignore` 需排除。新语义：排除 .gitignore 后脏条目 ≤1 且（为 0 或恰为 calc.sh），且 `git rev-list --count HEAD` 恒为 1（agent commit 架空 diff 时计数 >1 → 挂）。9 情形矩阵实测全符合预期，已重签名
- M3：CLI 新增 `parseFlags` 助手（env.go），`flag.ErrHelp` 视为帮助请求：打印 usage、立即返回、exit 0 且无误导错误行（run/mirror/sign 三个子命令接入）
- M5：mirror.go 错误路径注释如实化（pre-sandbox 失败时 Dir 为空串）
- M9：sign 成功信息改输出 stdout（与 run/mirror 一致），移除多余 os 导入
- M7：envFlag 重复 K 覆盖旧值（EqualFold 匹配、保最后出现），与 Go 子进程环境去重语义一致

**遗留事项**：
- TestRunCommandTimeout（judge 包）在全量并发跑时偶发超时抖动（100ms 注入超时时序敏感），独立/复跑稳定通过，与本次改动无关，后置观察
- file-only-change 仅守"改动范围 = calc.sh"，内容正确性由 add-basic 把守；只删 calc.sh 会过 file-only-change（语义如此，范围合法），由 add-basic 兜底判负
