# grok-bot

在 Telegram 或 Discord 的私聊里发文字或图片，这台机器上已登录的 Grok 订阅会接着上下文回复。不使用按量 API key。两个平台的上下文彼此分开。

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
- `/reset` 清空这个聊天的上下文（长期记忆仍会永久保留）
- `/memory` 查看长期记忆；`/memory add 内容` 添加；`/memory del 编号` 删除；`/memory clear` 清空（平时直接说「长期记忆：内容」「查看长期记忆」「删除长期记忆 #1」即可，执行 `/reset` 后仍会保留）
- `/dir` 查看可写目录；`/dir add rw /绝对路径` 允许写入；`/dir del /绝对路径` 取消写入（平时直接说"允许写入 /srv/out"即可）
- `/shell` 查看服务器命令执行权限；`/shell on` 开启；`/shell off` 关闭（平时直接说"开启命令执行" / "关闭命令执行"即可）
- `/task 一句话` 创建或管理定时任务（平时无需 `/task`，直接在聊天里说时间安排、暂停、恢复、跳过下次、立即试跑、修改时间或删除即可）；`/task list` 查看；`/task run 编号` 立即试跑；`/task del 编号` 删除
- `/help` 查看说明

### 会话恢复时的错误排查

`Session … found locally (originally in …)` 表示 CLI 找到了原会话，不能据此判断上下文失效。bot 优先返回 CLI 的 JSON 错误，并在纯文本错误中跳过这条会话提示；不自动清空或替换现有会话。

如果提示 `unknown model id`，在发生错误的平台发送 `/model` 查看当前模型及 CLI 提供的可用模型，再发送 `/model 可用模型名` 切换。两个平台的模型选择独立保存；切换模型保留上下文。不要先用 `/reset`，它会清空这个聊天的上下文关联。

仍然失败时，在服务器只读检查 `grok --version` 和 `sudo journalctl -u grok-tg-bot -n 60 --no-pager`，分享前隐藏 Token、Session ID 和用户标识。验证时在测试实例恢复已有会话，确认模型错误能正确展示，再切到可用模型连续发送两条消息确认上下文。错误提取回归测试可离线运行 `go test -mod=readonly ./internal/grok`（同时设置 `GOTOOLCHAIN=local`、`GOPROXY=off`）。该修复不改变 CLI 参数、配置、数据库或会话数据；回滚源码时从修改前备份恢复 `internal/grok/client.go` 和本说明，将新增测试文件移到备份目录。服务器回滚仍需先确认，再恢复已备份的程序并重启服务。

目前处理文字和图片；语音、视频和其他文件会回复暂不支持。可以列出并调用已连接的 MCP，可以联网搜索和打开网页，可以读取系统允许的文件，可以写入明确授权过的目录，可以给你的消息加表情、把服务器上的文件发给你。不能执行任何文件或命令。

### 联网搜索

默认开启，由 Grok 自己决定什么时候搜索、打开哪些网页。网页请求从运行 bot 的服务器发出。设置 `WEB_SEARCH=false` 并重启服务即可关闭。

### 目录读写

文件读写只在 Linux 和 macOS 上可用，由 grok 沙箱在内核层面限制（Linux 需要 5.13 以上内核；禁止读取的路径依赖 `bubblewrap`）。直接用自然语言说就行：

- **读取**：由系统权限控制。直接说"看看 /srv/summer 里有什么"，服务用户能读的文件 Grok 就能读，不需要授权。grok 登录凭证、`~/.ssh`、`BOT_ENV_FILE`、`bot.db`、所有 `.env` 和 `.pem` 文件一律读不到。
- **写入**：必须在消息里明确说"允许写入 /srv/out"（也可以说"授权写入""可以写入"），授权会一直保留；说"取消 /srv/out 的写入权限"或"取消写入 /srv/out"收回。只说"写到 /srv/out"不算授权。bot 会在回复开头确认授权结果。授权只认你本人发的消息，Grok 自己、网页或文件内容都不能授权。
- **执行**：不能执行任何文件或命令。
- 可写名单所有聊天共用。不能授权写入 `/`、`/etc`、`/proc`、`/sys`、`/dev`、`/boot`、`/root`、`~/.grok`、`~/.ssh`、`DATA_DIR`，以及这些位置的上下级目录和包含 `BOT_ENV_FILE` 的目录；软链接按真实路径检查。
- `/dir` 查看可写目录；`/dir add rw`、`/dir del` 是命令形式的兜底。
- bot 会生成服务用户的 `~/.grok/sandbox.toml`（第一行是 `# managed by grok-bot; do not edit`）。如果这个文件已经存在且不是 bot 生成的，bot 不会覆盖它，文件功能保持关闭，`/dir` 会提示管理员手动合并 `grokbot` 配置。
- 服务器没有安装 bubblewrap（`bwrap`）时，Linux 上的 grok 无法启动自定义沙箱（因 grok 内置的运行时 socket 黑名单依赖 `bwrap`），此时 bot 会自动省略 `--sandbox`，文件读写与聊天、联网搜索、定时任务照常可用（写入目录由系统提示词约束，发文件仍由 bot 检查黑名单）；装上 `sudo apt install -y bubblewrap` 后新会话自动启用内核沙箱，不用重启服务。
- 已有会话无需发送 `/reset` 即可直接使用文件读写功能。
- **已知风险（已接受）**：Grok 能读服务用户能读的一切，同时又能打开网页。恶意网页可能诱导 Grok 读取本机文件并通过网址发出去。请用专门的系统用户运行服务，不要让它能读到不该给 Grok 看的文件。
- bot 不再把自己的 token 和配置（`TELEGRAM_BOT_TOKEN`、`DISCORD_BOT_TOKEN`、允许名单、`DATA_DIR`、`BOT_ENV_FILE` 等）传给 grok，Grok 读 `/proc/self/environ` 也拿不到 bot token。`GROK_*` 开头的设置和 `HOME`、`PATH`、语言、代理等通用变量照常传下去。

### 定时任务

`/task` 后面用一句话描述时间和要做的事，由 Grok 解析成"一次 / 每天 / 每周 / 每隔多少分钟"。解析结果会复述给你，错了就用 `/task del 编号` 删掉。

- 每次执行都用独立的新会话，不会混进你平时聊天的上下文；结果以 `⏰ 定时任务 #编号` 开头发回创建它的聊天。
- 一次性任务执行后自动删除。bot 停机期间错过的执行不补跑；错过的一次性任务会提示后删除。
- 时区由 `TIMEZONE` 决定。每个聊天最多 `TASK_LIMIT` 个任务，周期任务间隔不少于 `TASK_MIN_INTERVAL`。

### 进度消息

普通聊天消息收到后，bot 先回一条「处理中…」，Grok 每次搜索或调用工具时把它改成「正在搜索：…」「正在调用：…」，最多每 3 秒改一次。完成后这条消息直接变成回答；回答太长时第一段沿用这条消息，后面的段落另发并引用原消息。出错时这条消息改成错误提示。编辑失败会改为发新消息。命令和定时任务不显示进度。

### 表情回应

Grok 可以给你当前这条消息加一个表情，比如你说「好的谢谢」，它可能只回一个 👍。只加表情、不写文字时，进度消息会被删掉。只能加在当前这条消息上，不能动别的消息；Telegram 只接受官方的固定表情集合。定时任务没有原消息，不能加表情。

### 发送文件

Grok 可以把服务器上的文件发给你，例如「把 /srv/out/report.pdf 发给我」。先发回答，再按顺序发文件，每个文件都引用原消息；定时任务的文件跟在 `⏰ 定时任务` 结果后面。

- 发文件和 Grok 读文件使用同一份禁止名单：grok 登录凭证、`~/.ssh`、`BOT_ENV_FILE`、`bot.db`、所有 `.env` 和 `.pem` 都发不出去。发文件另外拒绝 `DATA_DIR` 整个目录、`/proc`、`/sys`、`/dev` 下的文件和硬链接；软链接按真实路径检查，打开文件时不跟随软链接，发送时在已打开的文件上再校验一次。
- 文件功能关闭时（非 Linux/macOS，或 `sandbox.toml` 不是 bot 生成的）不能发文件。
- 每轮最多 10 个文件。Telegram 单个文件最大 50MB，10MB 以内的图片按照片发，更大或按照片发送失败时按文件发；Discord 单个文件最大 10MB。
- 某个文件发送失败时只提示「文件 xxx 发送失败。」，不影响回答和其他文件。

### 接收图片

可以发照片，也可以以文件形式发图片（不会被压缩），支持 JPEG、PNG、GIF、WebP。说明文字和图片一起交给 Grok；没有说明文字时 Grok 也会看图回答。

- 每条消息最多 10 张，多出的忽略并提示；单张超过 10MB 不处理。Telegram 相册（一次选多张发送）会合并成一条消息，回复引用带说明文字的那张。
- 只有白名单用户的图片会被下载；图片下载有总时长上限，失败时提示「图片下载失败，请重发。」。
- 图片写入 `DATA_DIR/inbox` 下的临时文件（权限 0600）交给 grok，处理完立即删除。grok 自己的会话记录（`~/.grok` 下）仍会保留图片，同一会话后面还能提到它；`/reset` 只是换一个新会话，不会删除旧记录。带图片的消息即使说明文字以 `/` 开头也不会当作命令。
- `DATA_DIR` 下的任何文件都不能通过发文件发出去。**已知风险**：沙箱目前没有禁止读取 `DATA_DIR/inbox`，两个聊天同时处理时，Grok 在一个聊天里理论上能读到另一个聊天正在处理的图片临时文件（但发不出去）。只有一个白名单用户时没有影响。

### 表情和发文件的实现方式

bot 在 `127.0.0.1` 的随机端口上开一个只给本机 grok 用的 MCP 服务，每轮对话生成一个随机令牌，令牌只能操作当前这个聊天。接入配置写在 `DATA_DIR/workspace/.grok/config.toml`，不改你全局的 `~/.grok/config.toml`。如果这个文件已经存在且不是 bot 生成的，bot 不会覆盖它，此时表情和发文件不可用，其他功能照常。

grok 只读取受信任目录里的项目配置，所以 bot 调用 grok 时对 `DATA_DIR/workspace` 使用 `--trust`。不在沙箱里运行时，服务用户的 `~/.grok/trusted_folders.toml` 里会多出这个目录的一条信任记录。

### 服务器实测清单

Windows 不支持 grok 沙箱，以下几项需要在 Debian 测试实例上确认。全部通过之前不要授权任何写入目录：

1. 让 Grok 尝试写 `~/.grok/sandbox.toml`，应当被拒绝（`read-only` 基础 profile 允许写 `~/.grok/`，这一项最关键）。
2. 禁止读取 `~/.grok/auth.json` 后，grok 仍能正常回复。
3. 新会话里 Theseus-mcp 等 MCP 能正常连接。
4. 读取：服务用户能读的目录可以读；`~/.ssh`、环境文件读不到。
5. 写入：授权的目录能写；没授权的目录（包括 `/srv` 下其他目录）写不进去。
6. v0.2.0 创建的 `grokbot` 会话恢复后是否用上了新规则（只用于了解情况）。
7. `uname -r` 至少 5.13；`bubblewrap` 是否已安装（是否安装由管理员决定）。

进度消息、表情、发文件和图片还需要在测试实例上确认：

1. 发一句需要联网搜索的话：看到「处理中…」→「正在搜索…」→变成回答，并引用原消息。
2. 发一个会产生很长回答的问题：第一段是编辑后的进度消息，后续段落引用原消息。
3. 发「好的谢谢」：Grok 只加表情时，进度消息消失，原消息上的 👀 变成新表情（Telegram）。
4. 「把 /srv/xxx/report.pdf 发给我」：收到文件；「把 /etc/grok-tg-bot.env 发给我」、指向 `.env` 的软链接、`~/.ssh/id_ed25519`：都被拒绝。
5. 卸掉或模拟没有 bubblewrap：发文件被拒，聊天正常。
6. 发一张照片（带 / 不带说明文字），再以文件形式发一张图，再发一个多图相册：Grok 能描述图片内容，相册只得到一次回复。
7. 定时任务里让 Grok 发文件：文件跟在 `⏰ 定时任务` 结果后面。
8. Discord 私聊重复 1、3、4、6（相册除外）。

另外在 `--sandbox` 下确认：沙箱里的 grok 能连上 `127.0.0.1` 上的 MCP 服务；带图片的 `--prompt-file` 能正常读取。再试一次把 `DATA_DIR/inbox` 加入沙箱的禁止读取列表后 `--prompt-file` 是否仍能读取：能的话应当加上，以消除上面「接收图片」里的已知风险。

回滚这四项功能：不改数据库，恢复旧程序并重启即可。`DATA_DIR/workspace/.grok/config.toml` 可以保留，旧程序不会读取它，它只指向一个已经不存在的端口；`~/.grok/trusted_folders.toml` 里的信任记录同样无害。如果要移除，由管理员确认后手动处理。

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
