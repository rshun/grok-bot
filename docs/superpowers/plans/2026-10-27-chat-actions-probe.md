# grok 实测结果（Task 0）

日期：2026-10-27
环境：本机 Windows 11，grok 1.0.70，模型 `grok-4.7`，不带 `--sandbox`。Debian 部分（P5）待用户执行，结果补在文末。

## 结论

| 项 | 结果 | 影响的阶段 |
|---|---|---|
| P1 流式输出 | 成立 | 阶段 1 可以开工 |
| P2 项目级 MCP + 令牌展开 | 成立，但必须加 `--trust` | 阶段 2、3 可以开工，计划已调整 |
| P3 图片 | 成立：ACP 图片块，用 `--prompt-file` 传 | 阶段 4 可以开工，计划已调整 |
| P4 工具全名 | `grokbot__react`，通过 `use_tool` 调用 | 提示词按此写 |
| P5 Debian | 待测 | 阶段 2 合并前、阶段 4 合并前必须补上 |

## P1 `streaming-messages-json`

- 第一行是 `{"type":"system","subtype":"init","session_id":...,"mcp_servers":[{"name","status"}],"tools":[...]}`。
- 模型的工具调用出现在 `assistant` 行的 `message.content` 里，分两种块：
  - 本地工具和 MCP：`{"type":"tool_use","name":...,"input":{...}}`。实测见过的 input：
    - `search_tool {"query","limit"}`
    - `use_tool {"tool_name":"grokbot__react","tool_input":{...}}`
    - `list_dir {"target_directory"}`
    - `grep {"pattern","path"}`
  - **联网搜索是 `{"type":"server_tool_use","name":"web_search","input":{"query":...}}`**，不是 `tool_use`。结果块是 `web_search_tool_result`。
- 工具结果在 `user` 行里，块类型是 `tool_result`。
- 最后一行：
  - 成功：`{"type":"result","subtype":"success","is_error":false,"result":"<最终文字>","session_id":...}`
  - 失败：`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["<错误文字>"],"session_id":...}`，进程退出码为 1，stderr 是 `Error: <同样的文字>`。
  - 达到上限：`subtype:"error_max_turns"`，`result` 为 null，stderr 是 `Error: max turns reached`。
- `result.result` 和现在 `--output-format json` 的 `text` 含义相同：都会把工具调用前的过渡语（"我去查…"）拼进最终文字，现有行为不变。
- `-s <uuid>` 新建会话时，`result.session_id` 等于该 uuid。

## P2 项目级 MCP

- `--cwd` 目录下的 `.grok/config.toml` 能被 `grok mcp list` 识别，显示为 `(project)`。
- **不加 `--trust` 时**：grok 根本不连接这个服务，Grok 在工具搜索里也找不到它。原因是项目级 MCP 受"信任目录"控制。
- **加 `--trust` 后**：
  - grok 连接服务，`Authorization: Bearer ${GROKBOT_TURN_TOKEN}` 被正确展开成环境变量的值。
  - 请求顺序：`server/discover`（新协议探测，带 `_meta`）→ `initialize`（`protocolVersion:"2025-11-25"`）→ `notifications/initialized` → `tools/list` → `tools/call`。
  - 探测服务对 `server/discover` 回了一个空的 `result`，grok 照常回退到 `initialize`。实现时改回 JSON-RPC `-32601 method not found`，Task 6 用真实服务再测一次，确认也能回退。
- `--trust` 的副作用：不在沙箱里运行时，会往 `~/.grok/trusted_folders.toml` 写一条该目录的信任记录（本次实测给 scratchpad 下的 `probe/p2` 写了一条）。在 `--sandbox grokbot` 下这个文件只读，信任只对本次会话有效、不落盘（据 grok 文档 18-sandbox）。bot 只对 `DATA_DIR/workspace` 使用 `--trust`，该目录内容由 bot 控制。
- 由于 grok 拉起子进程时会传递环境变量，Theseus-mcp（HTTP）不受影响；stdio MCP 是否继承令牌在 P5 里看。

## P3 图片

- `--prompt-json` 要的是 **ACP 内容块**。Anthropic 格式（`source.base64`）会报 `missing field data`。
- 能用的图片块：`{"type":"image","mimeType":"image/png","data":"<base64>"}`，和 `{"type":"text","text":...}` 放在同一个数组里。
- **`--prompt-file <path>` 接受同样的 JSON 内容块数组**，可以绕开命令行长度限制（Windows 整行 32K，Linux 单个参数约 128KB）。
- 用 `-r` 恢复会话并带图（`--prompt-file` 和 `--prompt-json` 都试过）：能用，模型同时记得前一轮的图片。
- `{"type":"resource_link","uri":"file:///...png"}` 也能用（grok 自己读文件，不需要 `read_file` 工具），但同样要落盘，不采用。
- 结论：bot 把内容块写到 `DATA_DIR/inbox/` 下的临时 JSON（0600），用 `--prompt-file` 传入，grok 结束后删除。

## P4 工具名

- MCP 工具全名是 `<服务名>__<工具名>`，即 `grokbot__react`、`grokbot__send_file`。
- Grok 先用 `search_tool` 搜索，再用 `use_tool {"tool_name":"grokbot__react","tool_input":{"emoji":"👍"}}` 调用。
- 调用后最终回答正好是 `[[NO_REPLY]]`，符合预期。

## P5 Debian（待用户执行）

1. 在 `--sandbox grokbot` 下，`--trust` 是否生效？能否连到 `127.0.0.1`？令牌是否展开？
2. 约 300KB 的图片通过 `--prompt-file` 能否正常识别？
3. （可选）有 stdio MCP 时，子进程环境里是否带有 `GROKBOT_TURN_TOKEN`。

结果：_待补_

## P4 复测：真实 MCP 服务（2026-10-28，本机 Windows）

用 `internal/actions` 的真实服务、bot 写出的 `workspace/.grok/config.toml` 和 `--trust` 跑本机 grok 1.0.70：

- 对 `server/discover` 回 `-32601` 后，grok 照常走 `initialize`，后续 `tools/list`、`tools/call` 都正常。
- `Authorization: Bearer ${GROKBOT_TURN_TOKEN}` 展开正确，令牌校验通过。
- 用户明确要求时，Grok 先用 `search_tool` 找到工具，再用 `use_tool {"tool_name":"grokbot__react","tool_input":{"emoji":"👍"}}` 调用，最终回答正好是 `[[NO_REPLY]]`。
- 只说"谢谢，好的"、不明确要求时，Grok 选择回一句文字，没有加表情。这属于提示词层面的行为，不影响功能是否可用。
