# grok-bot

在 Telegram 或 Discord 的私聊里发文字，这台机器上已登录的 Grok 订阅会接着上下文回复。不使用按量 API key。两个平台的上下文彼此分开。

普通消息的接收提示与 `codex-tg-bot` 一致：Telegram 用 👀 表情回应原消息，Discord 显示输入状态；处理完再发结果，不再单独发送固定文字回执。命令直接返回结果，Discord 斜杠命令先延迟确认，再更新原响应。

Telegram 和 Discord 的文字消息及文字命令会引用触发它们的原消息回复；长回复的每个分段也引用同一条原消息。排队提示、错误提示分别引用对应的消息，连续发送时不会引用错上一条或下一条。原消息已删除时允许发送普通回复。Discord 斜杠命令仍更新原来的命令响应。Grok 会话上下文保持不变。

同一个聊天里的消息仍按顺序处理。只有前面还有消息时才提示「已排队，前面还有 N 条。」；队列满时提示稍后重发。表情或输入状态发送失败不会影响最终回复。

验证应答变化：在测试实例的 Telegram 私聊发送文字，检查原消息上的 👀 和引用回复；在 Discord 私聊发送文字，检查输入状态和引用回复；发送 `/help`、`/model`、`/usage`，确认引用对应的命令消息且没有固定文字回执。Discord 斜杠命令应先显示等待状态，完成后更新响应。连续发送两条消息时，第二条仍显示排队提示，结果按顺序返回并分别引用各自的原消息。长回复的各分段应引用同一条原消息；处理期间删除原消息，确认最终结果仍能发送。

已有 Go 依赖缓存时，可禁用联网下载运行回归测试：

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
go test -mod=readonly ./...
```

缺少缓存时，上述检查会失败；恢复已锁定依赖需另行确认。本次只改变接收提示和 Discord 交互回复方式，不修改模型、CLI 参数、配置或数据库结构。Discord 输入状态是一次提示，长任务期间可能消失；排队很久的斜杠命令仍受平台交互有效期限制。

回滚时先备份当前改动，再确认并从修改前的文件备份逐项恢复 `internal/core/service.go`、`internal/core/service_test.go`、`internal/platform/platform.go`、两个平台的 `.go` 实现文件及本说明。新增的两个 `receipt_test.go` 需一并移至备份目录，避免引用已回滚的接口，不需要删除。已部署实例需要另行确认后恢复原二进制并重启；本地源码修改不会自动改变正在运行的服务。

## 命令

- `/model` 查看可用模型和当前模型
- `/model grok-4.6` 切换这个聊天之后使用的模型
- `/usage` 查看订阅本期剩余额度
- `/reset` 清空这个聊天的上下文
- `/dir` 查看开放的目录；`/dir add ro|rw /绝对路径` 开放目录；`/dir del /绝对路径` 移除
- `/task 一句话` 创建定时任务，例如 `/task 每天早上九点总结科技新闻`；`/task list` 查看；`/task del 编号` 删除
- `/help` 查看说明

### 会话恢复时的错误排查

`Session … found locally (originally in …)` 表示 CLI 找到了原会话，不能据此判断上下文失效。bot 优先返回 CLI 的 JSON 错误，并在纯文本错误中跳过这条会话提示；不自动清空或替换现有会话。

如果提示 `unknown model id`，在发生错误的平台发送 `/model` 查看当前模型及 CLI 提供的可用模型，再发送 `/model 可用模型名` 切换。两个平台的模型选择独立保存；切换模型保留上下文。不要先用 `/reset`，它会清空这个聊天的上下文关联。

仍然失败时，在服务器只读检查 `grok --version` 和 `sudo journalctl -u grok-tg-bot -n 60 --no-pager`，分享前隐藏 Token、Session ID 和用户标识。验证时在测试实例恢复已有会话，确认模型错误能正确展示，再切到可用模型连续发送两条消息确认上下文。错误提取回归测试可离线运行 `go test -mod=readonly ./internal/grok`（同时设置 `GOTOOLCHAIN=local`、`GOPROXY=off`）。该修复不改变 CLI 参数、配置、数据库或会话数据；回滚源码时从修改前备份恢复 `internal/grok/client.go` 和本说明，将新增测试文件移到备份目录。服务器回滚仍需先确认，再恢复已备份的程序并重启服务。

目前只处理文字。可以列出并调用已连接的 MCP，可以联网搜索和打开网页，可以在开放的目录里读写文件。不能执行命令。

### 联网搜索

默认开启，由 Grok 自己决定什么时候搜索、打开哪些网页。网页请求从运行 bot 的服务器发出。设置 `WEB_SEARCH=false` 并重启服务即可关闭。

### 目录读写

文件读写只在 Linux 和 macOS 上可用，由 grok 沙箱在内核层面限制（Linux 需要 5.13 以上内核；禁止读取的路径依赖 `bubblewrap`）。

- 用 `/dir add ro /srv/notes` 开放只读目录，`/dir add rw /srv/out` 开放可读写目录。名单所有聊天共用，允许名单里的用户都能修改。
- 不能开放 `/`、`/etc`、`/proc`、`/sys`、`/dev`、`/boot`、`/root`、`~/.grok`、`~/.ssh`、`DATA_DIR`，也不能开放这些位置的上级或下级目录，以及包含 `BOT_ENV_FILE` 的目录。软链接按真实路径检查。能否读写最终还受服务用户自身的 Unix 权限限制。
- bot 会生成服务用户的 `~/.grok/sandbox.toml`（第一行是 `# managed by grok-bot; do not edit`）。如果这个文件已经存在且不是 bot 生成的，bot 不会覆盖它，文件功能保持关闭，`/dir` 会提示管理员手动合并 `grokbot` 配置。
- 升级前已有的会话保留上下文，但不能使用文件功能；在对应聊天发送 `/reset` 开新会话后才能使用。

### 定时任务

`/task` 后面用一句话描述时间和要做的事，由 Grok 解析成"一次 / 每天 / 每周 / 每隔多少分钟"。解析结果会复述给你，错了就用 `/task del 编号` 删掉。

- 每次执行都用独立的新会话，不会混进你平时聊天的上下文；结果以 `⏰ 定时任务 #编号` 开头发回创建它的聊天。
- 一次性任务执行后自动删除。bot 停机期间错过的执行不补跑；错过的一次性任务会提示后删除。
- 时区由 `TIMEZONE` 决定。每个聊天最多 `TASK_LIMIT` 个任务，周期任务间隔不少于 `TASK_MIN_INTERVAL`。

### 服务器实测清单

Windows 不支持 grok 沙箱，以下几项需要在 Debian 测试实例上确认。任何一项不符合预期，先停用文件功能（不添加目录），不要自行放宽限制：

1. 禁止读取 `~/.grok/auth.json` 后，grok 仍能正常登录和回复。
2. 基于 `strict` profile 时，已有 MCP 服务器仍能启动和调用。
3. `/dir add` 后，带 `grokbot` 的已有会话能否直接读到新目录，还是需要 `/reset`。
4. 让 Grok 尝试写 `~/.grok/sandbox.toml`，应当被拒绝。
5. `uname -r` 至少 5.13；`bubblewrap` 是否已安装（是否安装由管理员决定）。

## 配置

在 Debian 上，服务使用已经执行过 `grok login` 的那个系统用户。复制 `deploy/grok-tg-bot.env.example` 为 `/etc/grok-tg-bot.env`：

```
TELEGRAM_BOT_TOKEN=Telegram 机器人 token
ALLOWED_USER_IDS=你的 Telegram 数字用户 ID
DISCORD_BOT_TOKEN=Discord 机器人 token
DISCORD_ALLOWED_USER_IDS=你的 Discord 数字用户 ID
DATA_DIR=/var/lib/grok-tg-bot
DEFAULT_MODEL=grok-4.7
GROK_BIN=grok
WEB_SEARCH=true
BOT_ENV_FILE=/etc/grok-tg-bot.env
TIMEZONE=Asia/Shanghai
TASK_LIMIT=20
TASK_MIN_INTERVAL=60m
```

两个平台可以只填其中一个。填了哪个平台，就要填它的允许名单。名单可以写多个，用逗号分开。聊天记录、当前模型和 Grok 会话 ID 存在 `DATA_DIR/bot.db`。

Discord 只处理私聊。把机器人邀请进你所在的服务器，邀请时勾选 `bot` 和 `applications.commands`，然后才能给它发私聊。斜杠命令是 `/help`、`/model`、`/usage`、`/reset`、`/dir`、`/task`。第一次注册后，命令可能要过一会儿才出现。不需要打开 Message Content Intent。

发布出来的文件叫 `grok-bot`。正在运行的服务仍使用原来的程序路径、单元名、环境文件和数据目录。

安装并启动：

```sh
sudo install -m 0755 grok-bot /usr/local/bin/grok-tg-bot
sudo install -d -o grok -g grok /var/lib/grok-tg-bot
sudo cp deploy/grok-tg-bot.service /etc/systemd/system/grok-tg-bot.service
sudo systemctl daemon-reload
sudo systemctl enable --now grok-tg-bot
```

把 service 文件里的 `User=` 改成实际登录了 grok.com 的用户。

`WEB_SEARCH` 控制联网搜索；`BOT_ENV_FILE` 是环境文件的位置，Grok 不能读取它；`TIMEZONE` 为空时用服务器本地时区；`TASK_LIMIT`、`TASK_MIN_INTERVAL` 限制定时任务数量和最小间隔。

## 发布和升级

本版本会给 `bot.db` 新增 `chats.sandbox` 列和 `dirs`、`tasks` 两张表（只新增，不改已有数据）。升级前先备份：

```sh
sudo systemctl stop grok-tg-bot
sudo cp -p /var/lib/grok-tg-bot/bot.db /var/lib/grok-tg-bot/bot.db.bak-$(date +%Y%m%d)
sudo systemctl start grok-tg-bot
```

升级后可以只读检查：`sqlite3 /var/lib/grok-tg-bot/bot.db "PRAGMA table_info(chats);"` 应包含 `sandbox`。回滚时恢复旧程序并重启即可，旧程序会忽略新增的列和表。

推送 `v*` 标签后，GitHub Actions 编译 Linux amd64 静态二进制，并把它挂到对应的 Release。

在服务器上，已登录 `gh` 时可以执行：

```sh
sudo sh deploy/upgrade.sh
```

升级只替换程序。`bot.db` 里的上下文还在。Grok 命令行本身用 `grok update` 单独更新。
