# 进度消息、表情回应、发送文件、接收图片 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 grok-bot 在私聊里显示可更新的进度消息，并让 Grok 能加表情、发文件、看图片。

**Architecture:** grok 的聊天轮改用流式输出，bot 根据工具调用事件编辑一条进度消息。bot 进程内跑一个只监听回环地址的最小 MCP 服务，提供 `react` 和 `send_file` 两个工具，每轮靠随机令牌确定目标聊天。图片由平台层识别、按需下载，通过 `--prompt-json` 交给 grok。

**Tech Stack:** Go 1.26 标准库、现有的 `go-telegram-bot-api/v5`、`discordgo`，grok CLI 1.0.70。

**Spec:** `docs/superpowers/specs/2026-10-27-chat-actions-design.md`

## Global Constraints

- 不新增 Go 依赖。MCP 只用标准库实现。确实需要 SDK 时，先停下来问用户。
- 不改数据库结构。
- 在 `dev-chat-actions` 分支开发，不在 `master` 提交。每次提交前运行 `git diff --cached | grep -nEi "password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|bearer|cookie|credential"`，命中内容只能是变量名或占位符。
- 测试中的 token 一律用 `your_token_here` 这类占位符。日志里不能出现 Telegram 文件下载 URL（URL 里含 bot token）和每轮令牌。
- 离线测试命令：`GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
- 用户可见的文字用中文，沿用现有语气。固定文案：
  - 进度初始文字：`处理中…`
  - 只加表情的标记：`[[NO_REPLY]]`
  - 空回答：`处理失败：没有得到回复。`（现有）
  - 不支持的附件：`目前只处理文字和图片。`
  - 混合附件提示：`其余附件暂不支持。`
  - 无说明文字的图片：`（用户发送了一张图片）` / `（用户发送了 N 张图片）`
- 数值：进度编辑间隔不少于 3 秒；每轮最多发 10 个文件；Telegram 发文件上限 50MB，Discord 25MB；单张收图上限 10MB；每条消息最多 10 张图；相册静默 1.5 秒后合并；进度详情截断到 80 个字符。
- `DATA_DIR/workspace/.grok/config.toml` 首行固定为 `# managed by grok-bot; do not edit`。文件存在但不是 bot 写的就不覆盖。

## Review Focus

1. **用户在处理过程中删了原消息**：进度消息编辑失败时，最终回答仍要以新消息送达。→ Task 2 `TestFinishFallsBackWhenEditFails`
2. **文件在检查之后、发送之前被删或变大**：这个文件补一条失败说明，其余文件照发。→ Task 8 `TestFlushFilesReportsFailureAndContinues`
3. **带变体选择符或肤色的表情**（`❤️`、`👍🏻`）：Telegram 上 `❤️` 去掉 U+FE0F 后按 `❤` 接受；肤色变体被拒，错误里附可用列表。→ Task 6 `TestTelegramReactionNormalizes`
4. **超长或多行的搜索词、URL**：进度只显示一行，截断到 80 个字符。→ Task 3 `TestProgressTextOneLine`
5. **grok 超时被杀**：进度消息改成错误文字，本轮令牌被注销，之后用它调用会被拒。→ Task 3 `TestProgressShowsErrorOnFailure`、Task 5 `TestTokenRejectedAfterEnd`

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/grok/stream.go`（新） | 解析 `streaming-messages-json`，回调工具调用事件 |
| `internal/grok/client.go` | 聊天轮改用流式输出；新增 `OnTool`、`Env`、`Images`、`Actions` 选项；提示词 |
| `internal/platform/platform.go` | 新增 `Progress`、`ProgressHandle`、`Reactor`、`FileSender`、`Image`、`OutFile` |
| `internal/platform/telegram/progress.go`、`react.go`、`files.go`、`images.go`（新） | Telegram 的各平台实现 |
| `internal/platform/discord/progress.go`、`react.go`、`files.go`、`images.go`（新） | Discord 的各平台实现 |
| `internal/actions/server.go`（新） | MCP 服务、令牌登记、工具调用 |
| `internal/actions/config.go`（新） | 写 `workspace/.grok/config.toml` |
| `internal/sandbox/send.go`（新） | `CheckSendable`，与 `render` 共用同一份禁止名单 |
| `internal/core/progress.go`（新） | 进度节流、工具事件转成文字 |
| `internal/core/turn.go`（新） | 组装一轮的 `actions.Turn`、结束后的收尾（删除进度、发文件） |
| `internal/core/service.go`、`tasks.go` | 接入以上部分 |
| `cmd/grok-bot/main.go` | 启动 MCP 服务、写配置 |
| `README.md` | 功能说明、限制、手动验证清单 |

阶段和可单独合并的点：**阶段 1** Task 1–3（进度）→ **阶段 2** Task 4–6（MCP + 表情）→ **阶段 3** Task 7–8（发文件）→ **阶段 4** Task 9–10（图片）→ Task 11（README）。Task 0 的结果决定各阶段能否开工（见设计 4.6）。

---

### Task 0: 实测 grok 的行为并记录结果

**Files:**
- Create: `docs/superpowers/plans/2026-10-27-chat-actions-probe.md`

这一步会消耗少量订阅额度。执行前要向用户说明并取得同意。P1–P4 在本机 Windows 上做（grok 1.0.70 已登录），P5 必须在 Debian 测试实例上做，把命令交给用户执行。临时文件放在 scratchpad。

- [ ] **P1 流式格式**：分别运行下面两条，保存输出。

  ```bash
  grok -p "用一句话说你好" -m grok-4.7 --output-format streaming-messages-json --max-turns 2 --disable-web-search --verbatim
  grok -p "搜索今天的科技新闻，给一条标题" -m grok-4.7 --output-format streaming-messages-json --max-turns 4 --verbatim
  ```

  记录以下内容：
  - `tool_use` 块的 `name` 和 `input` 字段名（`web_search`、`web_fetch`）。
  - `result` 行里 `result`、`session_id`、`subtype`、`is_error` 的实际形状。
  - 用 `-s <uuid>` 新建会话后，`result.session_id` 是否等于该 uuid。
  - 用 `-m bad-model` 制造一次失败，记录错误行的形状。

- [ ] **P2 项目级 MCP 和环境变量展开**：
  1. 新建临时目录 `probe/`，写入 `probe/.grok/config.toml`：url 指向 `http://127.0.0.1:18765/mcp`，headers 带 `Authorization = "Bearer ${GROKBOT_TURN_TOKEN}"`。
  2. 用 `python -c` 起一个打印请求头、对任何 POST 都返回 404 的 HTTP 服务。
  3. 设置 `GROKBOT_TURN_TOKEN=probe-value`，在 `probe/` 下以 `--cwd probe` 运行一次 `grok -p ...`。

  记录以下内容：
  - grok 是否连接了这个服务。
  - 收到的 `Authorization` 是 `Bearer probe-value` 还是原样的 `${...}`。
  - 是否需要 `trusted_folders.toml` 信任该目录。
  - `grok mcp list` 在该目录下的输出。

- [ ] **P3 图片怎么传**：用一张约 1KB 的 PNG，依次尝试下面几种传法，直到有一种能让 grok 正确描述图片：
  - `--prompt-json '[{"type":"text","text":"描述这张图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"<b64>"}}]'`
  - `{"type":"image_url","image_url":{"url":"data:image/png;base64,<b64>"}}`
  - 把 JSON 写进文件后用 `--prompt-file` 传

  再确认三件事：用 `-r` 恢复会话时能否带图；能否在 JSON 里引用图片的本地路径；200KB 以上的图片能否通过 `--prompt-file` 传入。

- [ ] **P4 MCP 工具全名**：推迟到 Task 4 完成后再做，见 Task 6 Step 1。

- [ ] **P5（Debian，交给用户）**：
  1. 以服务用户身份在 `DATA_DIR/workspace` 下重复 P2，加上 `--sandbox grokbot`，确认在沙箱内能连到 `127.0.0.1`。
  2. 用 200KB 的图片重复 P3 中能用的那种传法，确认 Linux 下单个参数的长度限制不会挡住它。
  3. 记录 grok 拉起的 stdio MCP 进程里是否带有 `GROKBOT_TURN_TOKEN`，只看变量是否存在，不记录值。可以用 `tr '\0' '\n' < /proc/<pid>/environ | grep -c GROKBOT_TURN_TOKEN` 查看。

- [ ] **记录与判定**：把实测结果写进 probe 文档，每项注明"成立 / 不成立 + 证据摘要"，不贴令牌或账户信息。按设计 4.6 的依赖表判断：哪一项不成立，就停下对应阶段并告诉用户。

- [ ] **Commit**

  ```bash
  git add docs/superpowers/plans/2026-10-27-chat-actions-probe.md
  git commit -m "docs: record grok probe results for chat actions"
  ```

---

### Task 1: grok 聊天轮改用流式输出

**Files:**
- Create: `internal/grok/stream.go`, `internal/grok/stream_test.go`
- Modify: `internal/grok/client.go`（`ReplyOptions`、`Reply`、`replyArgs`）
- Test: `internal/grok/client_test.go`

**Interfaces:**
- Produces:
  - `type ToolUse struct { Name string; Input json.RawMessage }`
  - `func ParseStream(r io.Reader, onTool func(ToolUse)) (HeadlessResult, error)`
  - `ReplyOptions` 新增字段 `OnTool func(ToolUse)`、`Env []string`（追加到 grok 进程环境）。

- [ ] **Step 1: Write the failing tests**（`stream_test.go`，样例行用 probe 文档 P1 记录的真实形状）
  - `TestParseStreamResult`：init 行 + 一行 assistant（含 text 块和 `server_tool_use{name:"web_search",input:{query}}`）+ 一行 assistant（`tool_use{name:"use_tool",input:{tool_name,tool_input}}`）+ result 行 → `Text` 等于 `result.result` 去掉首尾空白，`SessionID` 等于 `session_id`，`onTool` 依次收到 `web_search`、`use_tool`。`tool_use` 和 `server_tool_use` 都要上报。
  - `TestParseStreamError`：`subtype:"error_during_execution","is_error":true,"errors":["Couldn't set model ..."]` → 返回 `*RunError`，`Message` 取 `errors[0]`；`subtype:"error_max_turns"` → `*RunError`，`Message` 含 `max turns`；`type:"error"` 行 → `*RunError`，`Message` 取 `message`。
  - `TestParseStreamIgnoresUnknown`：中间夹杂未知 `type` 的行和非 JSON 行 → 不报错。
  - `TestParseStreamEmptyResult`：`result.result` 为空 → 返回 `Text==""`，**不报错**。
  - `TestParseStreamNoResult`：没有 result 行 → 返回错误。
  - `client_test.go` 的 `TestReplyArgsStreaming`：`JSONSchema==""` 时参数含 `--output-format streaming-messages-json`；有 `JSONSchema` 时仍是 `json`。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/grok/`
  Expected: FAIL，提示 `ParseStream` 未定义。

- [ ] **Step 3: Implement**
  - `ParseStream` 用 `bufio.Scanner` 逐行读取，缓冲上限调到 8MB。
  - `Reply` 在非 schema 轮次通过 `cmd.StdoutPipe` 边读边解析，stderr 仍收集起来。`ParseStream` 出错时：如果是 `*RunError` 就原样返回；否则交给 `headlessFailure("", stderr, err, runErr)`，保留现有的跳过 `Session … found locally` 的逻辑。
  - 非 schema 轮次去掉 `result.Text == ""` 时报错的逻辑；schema 轮次保持原样。
  - `cmd.Env = append(envWithoutUpdate(), opts.Env...)`。

- [ ] **Step 4: Run tests to verify they pass**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: PASS，原有 `TestHeadlessFailure*`、`TestParseHeadless*` 继续通过。

- [ ] **Step 5: Commit** — `git commit -m "feat: stream grok chat turns and report tool calls"`

---

### Task 2: 平台的可编辑进度消息

**Files:**
- Modify: `internal/platform/platform.go`
- Create: `internal/platform/telegram/progress.go`, `internal/platform/telegram/progress_test.go`, `internal/platform/discord/progress.go`, `internal/platform/discord/progress_test.go`

**Interfaces:**
- Produces（`platform` 包）：
  ```go
  type Progress interface {
      StartProgress(ctx context.Context, in Inbound, text string) (ProgressHandle, error)
  }
  type ProgressHandle interface {
      Update(ctx context.Context, text string) error
      // Finish 把进度消息改成第一段，其余段落新发并引用原消息。编辑失败时自己改为全部新发，只有新发也失败才返回错误。
      Finish(ctx context.Context, text string) error
      Delete(ctx context.Context) error
  }
  ```
  `telegram.messageReply` 和 `discord.messageReply` 实现 `Progress`。`interactionReply` 不实现。

- [ ] **Step 1: Write the failing tests**（沿用 `receipt_test.go` 的假 HTTP 写法）
  - Telegram `TestProgressStartEditsAndFinishes`：
    - 开始时调用 `sendMessage`，带 `reply_parameters` 引用原消息，文字为 `处理中…`。
    - `Update` 调用 `editMessageText`，`message_id` 是进度消息的 ID。
    - 对 4000×2 长度的文本调用 `Finish`：先 `editMessageText`（第一段），再 `sendMessage`（第二段，引用原消息）。
  - Telegram `TestProgressNotModifiedIsSuccess`：`editMessageText` 返回 `Bad Request: message is not modified` 时，`Update` 返回 nil。
  - Telegram 和 Discord 各一个 `TestFinishFallsBackWhenEditFails`：编辑返回 400，`Finish` 改为把所有段落作为新消息发出，并返回 nil。
  - Telegram `TestProgressDelete` 调用 `deleteMessage`；Discord 版本断言 `DELETE /channels/{c}/messages/{id}`。
  - Discord `TestProgressStartEditsAndFinishes`：断言 `ChannelMessageSendComplex` 带 `Reference`，编辑走 `PATCH`，分段上限 2000。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/platform/...`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - Telegram 继续用 `MakeRequest`，与现有 `send` 风格一致。
  - Discord 用 `ChannelMessageSendComplex`、`ChannelMessageEditComplex`、`ChannelMessageDelete`，并保持 `AllowedMentions: nobody()`、`SuppressEmbeds`。

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: editable progress messages on telegram and discord"`

---

### Task 3: core 接入进度消息

**Files:**
- Create: `internal/core/progress.go`, `internal/core/progress_test.go`
- Modify: `internal/core/service.go`（`dispatch`、`chat`、`chatTurn`、`startSession`）

**Interfaces:**
- Consumes: Task 1 的 `grok.ToolUse`、`ReplyOptions.OnTool`；Task 2 的 `platform.Progress`。
- Produces:
  - `func progressText(t grok.ToolUse) string`
  - `type progressView struct{...}`，方法 `Show(ctx, text string)`、`Finish(ctx, text string) error`、`Delete(ctx) error`
  - `Service` 新增 `ProgressInterval time.Duration`，0 表示用默认值 3 秒；测试里设为 1 纳秒。

- [ ] **Step 1: Write the failing tests**
  - `TestProgressTextOneLine`：
    - `web_search`，query 为 `"a\nb"` 加 200 个字 → 结果以 `正在搜索：` 开头，不含换行，长度不超过 `len("正在搜索：")+80` 个字符。
    - `web_fetch` → `正在打开网页：<url>`；`read_file`、`list_dir`、`grep` → `正在读取：<path>`；`write`、`search_replace` → `正在写入：<path>`；`use_tool` → `正在调用工具：<name>`；未知工具 → `正在处理：<tool>`。
    - input 字段名按 P1 的记录；取不到时只显示前半句。
  - `TestProgressThrottle`：间隔为 1 小时时，连续 `Show` 三次只产生一次 `Update`；文字相同也不重复更新。
  - `TestChatUsesProgressMessage`：fake engine 回调一次 `OnTool` 后返回 `pong` → 记录到的顺序是：开始 `处理中…`、Update、Finish(`pong`)；没有另发新消息。
  - `TestCommandHasNoProgress`：`/usage` 不调用 `StartProgress`。
  - `TestProgressShowsErrorOnFailure`：engine 返回 `context.DeadlineExceeded` → Finish 的文字等于 `grok.UserFacing(err)`。
  - `TestProgressStartFailureFallsBack`：`StartProgress` 返回错误 → 回答通过 `out.Send` 发出。
  - 现有的 `TestReceiptThenResult`、`TestQueueReportsAhead` 不改断言，继续通过。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/core/`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - `dispatch` 处理非命令、非任务的条目时，轮到它才调用 `StartProgress`。👀 仍在 `Handle` 入队时加。
  - Discord 的输入状态只持续几秒，开始处理时再调用一次 `MarkReceived`。Telegram 不需要，因为重复设置 👀 没有意义；可以用 `in.Platform` 判断。
  - 把 `OnTool` 通过 `ReplyOptions` 传进 `chatTurn` 和 `startSession`，调用 `view.Show(ctx, progressText(t))`。
  - 结束时，有进度消息就用 `Finish`，没有就用现有的 `s.send`。
  - 定时任务不变。

- [ ] **Step 4: Run tests to verify they pass**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: show grok progress in an editable message"`

**阶段 1 完成点**：可以单独合并。合并前在测试实例上做设计第 6 节手动验证的第 1、2 项。

---

### Task 4: 本地 MCP 服务和配置文件

**Files:**
- Create: `internal/actions/server.go`, `internal/actions/config.go`, `internal/actions/server_test.go`, `internal/actions/config_test.go`

**Interfaces:**
- Produces:
  ```go
  type File struct { Path, Name, Caption string; Size int64 }
  type Turn struct {
      React     func(ctx context.Context, emoji string) error // nil 表示不提供 react
      CheckFile func(path string) (File, error)                // nil 表示不提供 send_file
      MaxFiles  int                                            // 0 表示 10
  }
  func (t *Turn) Reacted() bool
  func (t *Turn) Files() []File
  type Server struct{ ... }
  func Listen(addr string) (*Server, error)        // addr 固定传 "127.0.0.1:0"
  func (s *Server) Port() int
  func (s *Server) Serve(ctx context.Context) error
  func (s *Server) Begin(t *Turn) (env string, end func()) // env 形如 "GROKBOT_TURN_TOKEN=<32 位 hex>"
  var ErrUnmanaged = errors.New("config.toml 不是 bot 生成的")
  func WriteConfig(path string, port int) error
  ```
  `Server` 的方法可并发调用。`end` 可以重复调用。

- [ ] **Step 1: Write the failing tests**（用 `httptest` 和真实的 `Server.Handler`）
  - `TestRejectsMissingAndWrongToken`：没有 `Authorization` 或令牌错误 → 401。
  - `TestTokenRejectedAfterEnd`：`end()` 之后，同一令牌调用 `tools/call` → 401。
  - `TestInitializeAndList`：`initialize` 返回 `capabilities.tools` 和 `serverInfo.name=="grokbot"`；`notifications/initialized` 返回 202 且没有 body；`tools/list` 在 `React` 和 `CheckFile` 都不为 nil 时列出 `react`、`send_file`，`CheckFile` 为 nil 时只列出 `react`。
  - `TestTurnsAreIsolated`：两个 Turn，用 A 的令牌调用 `react` → 只有 A 的 `React` 被调用。
  - `TestSendFileQueuesAndLimits`：`CheckFile` 成功 → 返回非错误结果，`Files()` 多一项；超过 `MaxFiles` → `isError:true`；`CheckFile` 失败 → `isError:true`，文字为错误信息。
  - `TestUnknownToolIsError`、`TestGetIs405`。
  - `config_test.go`：
    - `TestWriteConfigCreatesAndUpdates`：首行是标记，含 `127.0.0.1:<port>/mcp` 和 `${GROKBOT_TURN_TOKEN}`；端口变化时重写。
    - `TestWriteConfigRefusesUnmanaged`：已有文件没有标记 → 返回 `ErrUnmanaged`，文件内容不变。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/actions/`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - JSON-RPC 只支持 `initialize`（回传客户端请求的 `protocolVersion`）、`notifications/*`、`ping`、`tools/list`、`tools/call`。grok 会先发 `server/discover`，其他未知方法也一样，一律回 `-32601 method not found`（加测试 `TestUnknownMethodIsMethodNotFound`）。
  - 令牌比较用 `crypto/subtle`。
  - 配置文件的 headers 写法以 P2 实测为准，采用原子写入（参照 `sandbox.Manager.write`）。
  - 工具描述用英文写给 Grok：
    - `react`：`"React to the user's current message with one emoji."`，参数 `emoji`。
    - `send_file`：`"Send a file from this machine to the user. path must be absolute."`，参数 `path`，可选 `caption`。

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: local MCP server for per-turn chat actions"`

---

### Task 5: core 和 main 接入 MCP 通道

**Files:**
- Create: `internal/core/turn.go`, `internal/core/turn_test.go`
- Modify: `internal/core/service.go`, `internal/grok/client.go`（`ReplyOptions.Actions`、提示词）, `cmd/grok-bot/main.go`

**Interfaces:**
- Consumes: Task 4 的 `actions.Server.Begin`、`actions.Turn`、`actions.WriteConfig`。
- Produces:
  - `Service` 新增 `Actions *actions.Server`，nil 表示没有 MCP 通道。
  - `ReplyOptions` 新增：
    - `Actions bool`：本轮有 MCP 通道。`replyArgs` 据此加 `--trust`，否则 grok 不加载项目级 MCP（probe P2）。
    - `React bool`：提示词提到 react。定时任务轮次为 false。
    - send_file 的提示在 `Actions && FileTools` 时出现，见 Task 8。
  - `func (s *Service) beginTurn(in platform.Inbound, out platform.Replier, fileTools bool) (*actions.Turn, []string, func())`，返回本轮 Turn、要追加的 env 和结束函数。

- [ ] **Step 1: Write the failing tests**
  - `TestChatTurnPassesTokenEnv`：`svc.Actions` 不为 nil 时，engine 收到的 `opts.Env` 有一项以 `GROKBOT_TURN_TOKEN=` 开头，且 `opts.Actions==true`；为 nil 时没有这一项，且 `Actions==false`。
  - `TestTokenEndsAfterTurn`：engine 在回调里记下令牌；轮次结束后用它请求 `Server` → 401。
  - `client_test.go` 的 `TestReplyArgsTrust`：`Actions` 为 true 时参数含 `--trust`，为 false 或有 `JSONSchema` 时不含。
  - `client_test.go` 的 `TestSystemPromptActions`：`React` 为 true 时，提示词含 `grokbot__react` 和 `[[NO_REPLY]]`；为 false 时都不含。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/core/ ./internal/grok/`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - 提示词追加一句：`" You can react to the user's message with one emoji using the MCP tool grokbot__react. If a reaction alone is enough, call it and reply with exactly [[NO_REPLY]]."`
  - main：
    - `actions.Listen("127.0.0.1:0")`，然后 `WriteConfig(filepath.Join(workspace, ".grok", "config.toml"), port)`。
    - 返回 `ErrUnmanaged` 时记一条 `slog.Warn`，并让 `svc.Actions = nil`。
    - 用 `go srv.Serve(ctx)` 启动服务。

- [ ] **Step 4: Run tests to verify they pass**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: give each grok turn a token for chat actions"`

---

### Task 6: 表情回应

**Files:**
- Create: `internal/platform/telegram/react.go`, `internal/platform/telegram/react_test.go`, `internal/platform/discord/react.go`, `internal/platform/discord/react_test.go`
- Modify: `internal/platform/platform.go`, `internal/core/turn.go`, `internal/core/service.go`

**Interfaces:**
- Produces:
  - `type Reactor interface { React(ctx context.Context, in Inbound, emoji string) error }`，由两个平台的 `messageReply` 实现。
  - `const noReply = "[[NO_REPLY]]"`（core）。

- [ ] **Step 1: 用真实服务复测**：probe 已确认全名是 `grokbot__react`。在 Task 4、5 完成后，本机用真实 bot 的 MCP 服务跑一轮（`--trust`，cwd 为临时 workspace），确认两点：`server/discover` 回 `-32601` 后 grok 仍能 `initialize`；Grok 能找到并调用 `grokbot__react`。把结果补进 probe 文档。

- [ ] **Step 2: Write the failing tests**
  - Telegram `TestTelegramReactionNormalizes`：
    - `❤️` 发出时为 `❤`。
    - `👍🏻` 返回错误，错误文字含 `👍`（可用列表）。
    - `👍` 调用 `setMessageReaction`，`message_id` 是原消息。
  - Telegram 可用集合，取自 Bot API `ReactionTypeEmoji`：
    `👍 👎 ❤ 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂ 🤷 🤷‍♀ 😡`
  - Discord `TestDiscordReactAddsUnicode`：断言请求为 `PUT /channels/{c}/messages/{m}/reactions/{emoji}/@me`。
  - core `TestReactOnlyDeletesProgress`：
    - fake engine 在 `Reply` 里通过令牌调用 `react`，然后返回 `[[NO_REPLY]]` → 调用了 `Delete`，没有 Finish，也没有 Send。
    - 新会话的 ID 被 `StartSession` 保存。
  - `TestNoReplyWithoutReactIsEmpty`：只返回标记、没调用 react → Finish 的文字是 `处理失败：没有得到回复。`
  - `TestMarkerInsideTextKept`：返回 `好的 [[NO_REPLY]] 呀` → 原样 Finish。
  - `TestTaskTurnHasNoReact`：定时任务轮次的 `Turn.React == nil`。

- [ ] **Step 3: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: FAIL

- [ ] **Step 4: Implement**
  - `beginTurn` 在 `out` 实现了 `platform.Reactor` 且 `in.MessageID != ""` 时，设置 `Turn.React`。
  - 结束时的判断按设计 4.3 的三条规则。
  - 新会话的 ID 在空回答时也保存：Task 1 之后 `Reply` 不再因空回答报错，确认 `startSession` 走正常的保存路径即可。

- [ ] **Step 5: Run tests to verify they pass** — 同 Step 3，Expected: PASS

- [ ] **Step 6: Commit** — `git commit -m "feat: let grok react to the user's message"`

**阶段 2 完成点**：可以单独合并。合并前在测试实例上做手动验证第 3 项（Discord 也做一遍）。

---

### Task 7: 发送前的文件检查

**Files:**
- Create: `internal/sandbox/send.go`, `internal/sandbox/send_test.go`
- Modify: `internal/sandbox/sandbox.go`（`render` 改为使用 `Policy.Deny()`）

**Interfaces:**
- Produces:
  - `func (p Policy) Deny() []string`，`render` 和 `CheckSendable` 共用。
  - `func (p Policy) CheckSendable(path string) (real string, size int64, err error)`，错误文字用中文，会原样交给 Grok。

- [ ] **Step 1: Write the failing tests**（Windows 上不支持软链接的用例用 `t.Skip`，参照现有 `sandbox_test.go`）
  - `TestCheckSendableEnforcesEveryDenyEntry`：遍历 `p.Deny()` 的每一项，在临时目录里造出对应的文件（目录类条目在其下建一个文件，`**/.env` 建 `x/.env`，`**/*.pem` 建 `x/a.pem`）→ 全部被拒。这条测试保证两边名单一致。
  - `TestCheckSendableSymlinkIntoDenied`：`link → ~/.ssh/id` 被拒；`link.txt → x/.env` 被拒。
  - `TestCheckSendableRejectsDirAndMissing`：传目录、不存在的路径、相对路径都被拒。
  - `TestCheckSendableAccepts`：普通文件返回真实路径和大小。
  - 原有 `TestRender` 不改断言，继续通过。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/sandbox/`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - 原路径（`Clean` 后）和 `EvalSymlinks` 后的真实路径都要检查。
  - 目录类条目先 `resolve` 再用 `within` 判断。
  - `**/.env` 按文件名完全匹配；`**/*.pem` 用 `filepath.Match("*.pem", base)`。
  - 用 `os.Stat` 确认是普通文件（`Mode().IsRegular()`），再 `os.Open` 后立即关闭，确认可读。

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: shared deny list check for files sent to chat"`

---

### Task 8: 发送文件

**Files:**
- Create: `internal/platform/telegram/files.go`, `internal/platform/telegram/files_test.go`, `internal/platform/discord/files.go`, `internal/platform/discord/files_test.go`
- Modify: `internal/platform/platform.go`, `internal/core/turn.go`, `internal/core/service.go`, `internal/core/tasks.go`, `internal/grok/client.go`（提示词）

**Interfaces:**
- Produces:
  ```go
  type OutFile struct { Path, Name, Caption string }
  type FileSender interface {
      SendFile(ctx context.Context, chatID, replyTo string, f OutFile) error
      FileLimit() int64 // Telegram 50<<20，Discord 25<<20
  }
  ```
  由两个平台的 `*Bot` 实现，`messageReply` 通过嵌入获得这两个方法。
  - core：`func (s *Service) flushFiles(ctx context.Context, fs platform.FileSender, chatID, replyTo string, files []actions.File)`
  - `runTask` 的返回值改为 `(string, []actions.File)`。

- [ ] **Step 1: Write the failing tests**
  - Telegram `TestSendFilePhotoVsDocument`：
    - `.PNG` 走 `sendPhoto`，`.pdf` 走 `sendDocument`。
    - 请求是 multipart，带 `caption`，`reply_to_message_id` 是原消息，`allow_sending_without_reply=true`。
  - Discord `TestSendFileAttaches`：multipart 里有文件，`message_reference` 是原消息。
  - core `TestSendFileCheckedAndSentAfterReply`：
    - 本轮 `FileTools=true`。fake engine 通过令牌调用 `send_file`，传一个临时文件，然后返回 `好了`。
    - 记录顺序为：Finish(`好了`) 在先，SendFile 在后。
  - `TestSendFileRefusedWithoutFileTools`：`FileTools=false` 时，`tools/list` 里没有 `send_file`，提示词也不提。
  - `TestSendFileRespectsPlatformLimit`：`FileLimit` 设为 10 字节，传入 11 字节的文件 → 工具返回错误。
  - `TestFlushFilesReportsFailureAndContinues`：两个文件中第一个在发送前被删除 → 补发一条含文件名的失败说明，第二个照常发出。
  - `TestTaskSendsFilesAfterResult`：任务轮次 `send_file` 后，`⏰` 结果消息在前，文件在后，`replyTo==""`。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - Telegram 用 `tgbotapi.NewPhoto` / `NewDocument` 加 `tgbotapi.FilePath`，设置 `ReplyToMessageID`、`AllowSendingWithoutReply`、`Caption`。
  - Discord 用 `MessageSend.Files`，`Reference` 的写法同现有 `send`。
  - `beginTurn` 在 `fileTools` 为真且 `out`（或任务的 `Senders[platform]`）实现了 `FileSender` 时，设置 `Turn.CheckFile`：先调 `CheckSendable`，再比较 `FileLimit()`。
  - 提示词：`Actions && FileTools` 时追加 `" You can send a file to the user with the MCP tool grokbot__send_file; path must be absolute."`

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: let grok send files to the chat"`

**阶段 3 完成点**：可以单独合并。合并前在测试实例上做手动验证第 4、5、7 项。

---

### Task 9: 接收图片（单条消息）

**Files:**
- Create: `internal/platform/telegram/images.go`, `internal/platform/telegram/images_test.go`, `internal/platform/discord/images.go`, `internal/platform/discord/images_test.go`
- Modify: `internal/platform/platform.go`, `internal/platform/telegram/telegram.go`, `internal/platform/discord/discord.go`, `internal/core/service.go`, `internal/grok/client.go`

**Interfaces:**
- Produces:
  ```go
  // platform
  type Image struct {
      MIME, Name string
      Size       int64
      Load       func(ctx context.Context) ([]byte, error) // 只在通过名单检查后才调用
  }
  // Inbound 新增：
  Images  []Image
  Skipped bool // 同一条消息里有被忽略的非图片附件
  // grok
  type Image struct { MIME string; Data []byte }
  // ReplyOptions 新增 Images []Image
  // Client 新增 InboxDir string（main 设为 DATA_DIR/inbox，权限 0700）
  ```
  `TextMessage` 的含义改为"有文字或有图片"。

- [ ] **Step 1: Write the failing tests**
  - Telegram `TestInboundPhotoUsesLargest`：三个尺寸的 photo 加 caption → `Images` 只有 1 张且取最大尺寸，`Text` 等于 caption，`TextMessage==true`。
  - Telegram `TestInboundImageDocument`：`image/png` 的 document 被识别为图片；`application/pdf` 不是图片 → `TextMessage==false`。
  - Telegram `TestLoadUsesGetFile`：`Load` 先 `getFile` 再下载；返回的错误文字里不含 token。
  - Discord `TestInboundImageAttachments`：两张图加一个 zip → `len(Images)==2`，`Skipped==true`；只有一个 zip → `TextMessage==false`。
  - core：
    - `TestImageMessageReachesEngine`：在 `Load` 返回字节，engine 收到的 `opts.Images` 有一张，`Prompt` 等于 caption。没有 caption 时 `Prompt=="（用户发送了一张图片）"`，两张时为 `（用户发送了 2 张图片）`。
    - `TestImageNotLoadedForStranger`：不在名单里的用户 → `Load` 从未被调用。
    - `TestImageTooLarge`：`Size` 为 10MB+1 → 不调用 engine，回复提示超过上限。
    - `TestSkippedAttachmentNote`：`Skipped==true` → 回答前面加 `其余附件暂不支持。`
    - `TestUnsupportedOnly`：只有语音 → 回复 `目前只处理文字和图片。`
    - `TestCaptionGrantApplies`：caption 为 `允许写入 <dir>` → 授权生效。
  - grok `TestPromptBlocks`：文字加两张图 → JSON 数组依次为 `{"type":"text","text":...}`、`{"type":"image","mimeType":"image/png","data":"<base64>"}`、第二个图片块。注意是 ACP 字段名 `mimeType`/`data`，不是 Anthropic 的 `source`。
  - grok `TestReplyArgsImages`：有 `Images` 时参数里有 `--prompt-file <InboxDir 下的文件>`，没有 `-p`；没有图片时仍用 `-p`。
  - grok `TestPromptFileRemoved`：`Reply` 返回后（成功或失败），临时文件都不在了。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: FAIL

- [ ] **Step 3: Implement**
  - 图片走 `--prompt-file`（probe P3）。用 `os.CreateTemp(InboxDir, "prompt-*.json")` 建文件，权限 0600，写入 ACP 内容块数组。grok 进程结束后用 `defer os.Remove` 删除，删除失败只记日志。这是 bot 自己的临时文件，不涉及用户文件。
  - Discord 下载用带 ctx 的 `http.Client`，用 `io.LimitReader` 限制在 10MB+1。

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: accept images in private chats"`

---

### Task 10: Telegram 相册合并

**Files:**
- Create: `internal/platform/telegram/album.go`, `internal/platform/telegram/album_test.go`
- Modify: `internal/platform/telegram/telegram.go`（`Run`）

**Interfaces:**
- Consumes: Task 9 的 `platform.Image`。
- Produces: `type albums struct{...}`，方法为 `add(in platform.Inbound, groupID string, reply platform.Replier)` 和 `flush`。静默时间作为字段，默认 1.5 秒，测试里调小。

- [ ] **Step 1: Write the failing tests**
  - `TestAlbumMergesGroup`：同一 `media_group_id` 的三条消息，第二条带 caption → handler 只被调用一次，`len(Images)==3`，`Text` 是 caption，`MessageID` 是第二条。
  - `TestAlbumWithoutCaptionUsesFirst`：没有 caption 时，`MessageID` 取第一条。
  - `TestNonAlbumPassesThrough`：没有 `media_group_id` 的消息立即交给 handler。
  - `TestAlbumFlushOnStop`：ctx 取消时，缓冲中的组被丢弃，不会 panic。

- [ ] **Step 2: Run tests to verify they fail**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./internal/platform/telegram/`
  Expected: FAIL

- [ ] **Step 3: Implement** — 用 `time.AfterFunc` 加 mutex。同一组每来一条就重置计时器。合并后的组超过 10 张时，只保留前 10 张并设置 `Skipped=true`。

- [ ] **Step 4: Run tests to verify they pass** — 同 Step 2，Expected: PASS

- [ ] **Step 5: Commit** — `git commit -m "feat: merge telegram albums into one turn"`

**阶段 4 完成点**：可以单独合并。合并前在测试实例上做手动验证第 6 项。

---

### Task 11: README 和整体验证

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 更新 README**
  - 把"目前只处理文字"改为"文字和图片"。
  - 新增"进度消息""表情回应""发送文件""接收图片"四节，说明规则、上限、发文件和沙箱读取规则一致、文件功能关闭时不能发文件。
  - 说明 `DATA_DIR/workspace/.grok/config.toml` 由 bot 生成；已有的非 bot 文件不会被覆盖，此时表情和发文件不可用。
  - 说明 bot 调用 grok 时对 `DATA_DIR/workspace` 使用 `--trust`。不在沙箱里运行时，`~/.grok/trusted_folders.toml` 里会多出这个目录的一条信任记录。
  - 把设计第 6 节的 8 项手动验证加进"服务器实测清单"。
  - 回滚说明：不改数据库，恢复旧程序即可。配置文件可保留；如果要移除，由管理员确认后手动处理。

- [ ] **Step 2: 全量测试**

  Run: `GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
  Expected: PASS

- [ ] **Step 3: 敏感信息检查后提交**

  ```bash
  git add README.md
  git diff --cached | grep -nEi "password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|bearer|cookie|credential"
  git commit -m "docs: describe progress, reactions, file sending and images"
  ```

  Expected: grep 命中的只有变量名或占位符。
