# CLAUDE.md — AgentBattle 项目

## 项目简介

AgentBattle：本地优先的 AI agent 养成对战平台。用户的 coding agent（Claude Code / Codex 等）在各自电脑本地运行（带私有记忆/skills），通过平台匹配进行双人对抗（同题竞速/攻防/盲评），产出 ELO 天梯、多维能力画像、配置归因分析。

## 关键文档

- [CHANGE.md](./CHANGE.md) — 迭代记录。当前进度：**M1（计划 1/2/3）、M2 计划 1（六维画像）与 M2 计划 2（对局复盘报告：`review` 子命令与 `GET /api/matches/{id}/review` 公开路由）均已完成**——平台注册/任务下发/结果上报/Elo 结算/天梯全链路联通，并发与孤儿对局已加固；结算后自动重建六维能力画像（全体池百分位归一化），`profile` 子命令与 API 可查；任一已结算对局可按 matchID 查得结构完整复盘。--dry-run 影子赛属 M2 后续。平台/Runner 代码分别在 `platform/`、`runner/` 子树。
- [设计文档](./docs/superpowers/specs/2026-09-13-agent-battle-platform-design.md) — 完整产品/技术设计：架构、Runner 设计、赛制、评分体系、防作弊、测试策略、里程碑。

## 核心设计红线（实现时不可违背）

1. Runner 永不读取/上传用户的 CLAUDE.md、skills、记忆内容
2. 对局环境 = 平台下发的全新临时仓库，永不触碰用户真实项目
3. 事件流不含文件内容明文（只有路径与操作类型）
4. Runner 核心 headless 化，所有 UI（CLI/Web Dashboard/桌面壳）都是壳
