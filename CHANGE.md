# CHANGE.md — 项目迭代记录

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
