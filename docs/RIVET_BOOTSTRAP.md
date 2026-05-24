# Rivet / Neo WebSocket Bootstrap 抓包说明与 self-serve 合成草案

> 状态：本文件是 Thread C 的协议研究文档。当前仓库环境没有真实 Amp 登录态和 actors.ampcode.com 会话可用，因此 `testdata/bootstrap/smart-mode.jsonl` 先保留 `PENDING:` 标记；真实数据应通过 `scripts/capture-bootstrap.sh` 在用户 Mac 上用透传模式采集。本文只基于现有 amp-proxy 观测点、Rivet WS 帧 dump、以及客户端/代理代码中已经暴露的帧类型整理，不逆向 amp 客户端二进制。

## 1. 抓包路径

amp-proxy 已有内置 dump 机制：设置 `AMP_PROXY_DUMP_DIR=/some/dir` 后，`traffic_dump.go` 会把 HTTP 请求/响应和 Rivet WebSocket 帧写入：

```text
$AMP_PROXY_DUMP_DIR/<startTs>/timeline.jsonl
```

其中 WebSocket 文本帧格式为：

```json
{"kind":"ws_frame","direction":"<","frame":{"type":"..."}}
```

方向约定：`>` 表示 amp client → actors upstream，`<` 表示 actors upstream → amp client。抓 bootstrap 时必须同时设置 `AMP_PROXY_RIVET_PASSTHROUGH=1` 并重启 amp-proxy，因为 `NewRivetForwarder` 只在启动时读取该变量；否则默认 local injection 会在 `client_append_user_msg` 处接管，抓不到真实服务端后续推理流。

推荐流程：

```bash
scripts/capture-bootstrap.sh
# 另一个终端执行脚本提示的命令：
amp -x "say hi" --mode smart
```

脚本会从最新 `timeline.jsonl` 中抽取「WS upgrade 完成后、首个 `client_append_user_msg` 之前」所有 server→client 帧，写入 `testdata/bootstrap/smart-mode.jsonl`。如果没有捕获到 timeline，则保留 `PENDING:` 行。备用手动方案：直接打开最新 `~/.amp-proxy/bootstrap-captures/<ts>/timeline.jsonl`，筛选 `kind == "ws_frame" && direction == "<"`，在第一个 `direction == ">" && frame.type == "client_append_user_msg"` 前停止；若旧版本没有 dump，可临时让 `logFrame` 输出 JSONL，但这属于调试手段，不应提交业务改动。

## 2. WS 握手与 subprotocol 语义

Rivet 把路由和认证信息放在 `Sec-WebSocket-Protocol` 多个条目里，amp-proxy 目前原样转发：

- `rivet`：协议标记。`RivetForwarder.CanHandle` 只要看到它或 `rivet_` 前缀，就把该 WS 视为 Neo/Rivet 连接。
- `rivet_target.<actorTarget>`：目标 actor 或路由目标，self-serve 可记录但不必转发给本地合成器。
- `rivet_actor.<actorId>`：远端 actor id。self-serve 没有真实 actor 时可生成本地 session id，但应保持日志可追踪。
- `rivet_encoding.<bare|json>`：帧编码。当前 dump 看到的是文本 JSON；self-serve 首版只应支持 JSON 文本帧，未知编码直接拒绝或回退 upstream。
- `rivet_conn_params.<urlencoded-json>`：连接参数，里面包含 `wsToken` JWT。`extractRivetSessionInfo` 会解析它拿 `tid` 和 `am`。
- `rivet_token.<jwt>`：Rivet ACL token 类凭证；透传给 actors 时需要，self-serve 模式不应验证或存储真实 token。

URL query 中还可能有 `rvt-token`，当前 forwarder 会用真实 upstream password 重写。self-serve 不连接 upstream 时不需要该值，但可以保留用于兼容日志。

## 3. `rivet_conn_params.wsToken` JWT payload 字段

目前明确从 payload 读取的字段如下：

- `tid`：Amp thread id，例如 `T-...`。amp-proxy 用它绑定 `RivetSession` 和 thread store；self-serve 必须保留。
- `oid`：owner/user id，用于归属或审计；本地合成通常只镜像，不应作为安全边界。
- `am`：agent mode，例如 `smart`、`large`、`rush`。决定本地模型映射和是否 local route。
- `sub`：JWT subject，可能是用户或连接主体；目前没有业务依赖。
- `iss`：issuer，已观察/注释为 `amp-workers`。
- `aud`：audience，已观察/注释为 `amp-dtw`。
- `exp` / `iat`：过期和签发时间。actors 是否校验签名是开放问题；self-serve 只需要解析 payload，不应把签名有效性当作本地权限判断。

fixture 规则：真实 `wsToken` 或任何 JWT 必须替换成 `[REDACTED].[REDACTED].[REDACTED]`；`threadId/tid` 可保留，因为单独不具备认证能力。

## 4. 服务端启动帧序列：候选与合成策略

真实顺序以 `testdata/bootstrap/smart-mode.jsonl` 为准；未抓到前，下表是从 forwarder 注释、Rivet session 代码和已知 UI 需求推断出的 server→client bootstrap 候选。critical 表示客户端可能阻塞或后续本地推理依赖；nice-to-have 表示缺失会降级但一般不阻塞；cosmetic 表示主要影响 UI 呈现。

| frame type | criticality | 关键字段 / 时机 | self-serve 合成策略 |
|---|---|---|---|
| `agent_state` | critical | 连接建立后告知 agent 当前 `idle`/`streaming`/错误状态；本地注入推理前至少需要一个稳定 idle 起点。 | 静态生成 `{type:"agent_state", state:"idle"}`，带本地 seq。推理开始/结束再发 streaming/idle。 |
| `executor_connected` | critical | 服务端确认 executor 已连接或注册；comments 指出本地仍依赖 server 的 `executor_connected`。 | 在收到首个 `executor_connect` 后 echo/ack，包含 thread/session id（若真实 fixture 有字段则照结构补齐）。 |
| `thread_loaded` / `thread_hydrated` | nice-to-have | 可能在 thread 历史和元数据准备好后发送。 | 从本地 thread store 合成空历史或已持久化历史；未知字段按 fixture 最小化。 |
| `client_thread_settings` / `thread_settings` | nice-to-have | 可能同步 reasoning effort、model/mode 等设置。 | 从 `agentMode`、`client_update_thread_settings` 和默认配置生成；没有则省略。 |
| `inference_tools` | nice-to-have | 通知 UI/agent 当前可用 inference tool 集合或工具状态。 | 等 `executor_tools_register` 后由注册内容转换，或首版省略并只依赖 executor tool path。 |
| `plugin_message` | critical for plugin hooks | 本地推理中若发起插件请求，客户端通过 plugin hook 回复。bootstrap 期可能无。 | 不在最小 bootstrap 中主动发；需要插件时按现有 `tryDeliverPluginReply` 约定生成请求。 |
| `message_added` | critical after user msg | 服务端创建 assistant/user message 记录，UI 依赖它渲染消息。bootstrap 之前通常无新 assistant。 | 不在连接 bootstrap 必发；推理开始时合成 assistant message id。 |
| `delta` | critical after inference | 流式 token / thinking / tool_use 增量。bootstrap 前通常无。 | 不在最小 bootstrap 必发；由本地 LLM streaming 转换。 |
| `thread_title` | cosmetic | 首轮后标题生成。 | 本地推理完成后异步生成；失败可省略。 |
| `error_set` | nice-to-have | executor 或服务端错误展示。 | 只在本地初始化失败、工具权限拒绝等情况下合成。 |
| `heartbeat` / WS ping | nice-to-have | 尚未确认是否存在应用层 heartbeat；gorilla 也可处理 WS control ping/pong。 | 首版依赖 WebSocket ping/pong；若 fixture 出现应用层 heartbeat，则按固定周期 echo。 |
| `session_bound` / `actor_ready` | nice-to-have | 可能表示 actor/session 绑定完成。 | 若真实 fixture 证明客户端等待它，则用 thread id 和 generated actor id 合成；否则省略。 |

这里列出了超过 8 个不同 frame type；真实 fixture 到位后，应把表中「候选」替换为「观察到的顺序 + 字段」。

## 5. 客户端→服务端首批帧顺序

`RivetSession.observeClient` 已枚举客户端会发的帧。启动早期常见顺序预计是：

1. `executor_connect`：executor 宣告连接，可能携带环境、版本或能力；self-serve 应记录并回 `executor_connected`。
2. `executor_tools_register`：注册工具列表，字段大致为 `tools[].name/description/inputSchema/source/meta`；本地 Anthropic 工具转换由 `translateToolsToAnthropic` 完成。
3. `executor_skill_snapshot`：已安装 skills 摘要；用于 system prompt。
4. `executor_guidance_snapshot`：AGENTS.md / 开发约束文本；用于 system prompt。
5. `executor_environment_snapshot`：工作目录、OS、repo 等环境摘要；用于 system prompt。
6. `client_update_thread_settings`：reasoning effort、模型/模式相关设置。
7. `executor_tools_bootstrap_complete`：工具 bootstrap 完成；现有代码有 `waitForToolsBootstrap`，说明本地推理最好等它或超时。
8. `client_append_user_msg`：用户消息。当前 local injection 就在此处 suppress upstream 并启动本地 inference。

开放问题：客户端是否严格等待 `executor_connected` 才继续发送工具注册，还是先发一批再等待 ack；是否存在应用层 heartbeat；actors 是否验证 `wsToken` 签名，或只把 payload 作为路由上下文。这些问题应由真实 `timeline.jsonl` 的方向和时间戳确认。

## 6. 结论：self-serve 最小合成帧集

预期最小集合少于 5 个。首版建议：

1. `agent_state`：连接接受后立即发 `idle`，让 UI/CLI 有初始状态。
2. `executor_connected`：收到 `executor_connect` 后发 ack，解除 executor ready 依赖。
3. `thread_loaded` 或真实 fixture 中等价的 hydrated 帧：只在客户端确实等待 thread hydration 时发送；字段保持最小。
4. 推理开始后的 `message_added`：不是纯 bootstrap，但 self-serve 要显示 assistant 回复必须合成。
5. 推理流中的 `delta` + 结束 `agent_state idle`：属于 inference 阶段，不计入连接 bootstrap，但必须由本地 LLM path 继续提供。

如果真实抓包证明 `thread_loaded`/settings 类帧不是阻塞项，那么「连接 bootstrap」最小只需要 `agent_state` 和 `executor_connected` 两帧；随后等待客户端 `executor_tools_bootstrap_complete` 与 `client_append_user_msg` 即可进入现有 local inference。
