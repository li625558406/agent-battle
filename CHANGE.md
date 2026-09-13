# CHANGE.md — 项目迭代记录

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
