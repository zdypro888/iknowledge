# iknowledge 开发约定(Codex 入口)

本文件只是入口:全部开发约定、铁律与当前进度以 [CLAUDE.md](CLAUDE.md) 为**唯一正本**,请完整阅读并遵守——不在此处复制内容,防两份文档漂移。

对应 Codex 的两点换算:
- CLAUDE.md 里"真实客户端联测禁用 `claude -p`、用 PTY 交互式"的纪律,对 Codex 同义为:协议级手段(curl/httptest)优先;需要真实 Codex 会话时用 `codex exec --dangerously-bypass-approvals-and-sandbox </dev/null`(不关 stdin 会挂起等输入)。
- 本仓库的知识库接入(kb_* 工具)对 Codex 经 `~/.codex/config.toml` 的 `[mcp_servers.knowledge]` 段,见 `iknowledge setup` 第 ④ 段输出。
