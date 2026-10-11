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
- `/help` 查看说明

目前只处理文字。可以列出并调用已连接的 MCP。不能改这台机器上的文件，也不能执行命令。

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
```

两个平台可以只填其中一个。填了哪个平台，就要填它的允许名单。名单可以写多个，用逗号分开。聊天记录、当前模型和 Grok 会话 ID 存在 `DATA_DIR/bot.db`。

Discord 只处理私聊。把机器人邀请进你所在的服务器，邀请时勾选 `bot` 和 `applications.commands`，然后才能给它发私聊。斜杠命令是 `/help`、`/model`、`/usage`、`/reset`。第一次注册后，命令可能要过一会儿才出现。不需要打开 Message Content Intent。

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

## 发布和升级

推送 `v*` 标签后，GitHub Actions 编译 Linux amd64 静态二进制，并把它挂到对应的 Release。

在服务器上，已登录 `gh` 时可以执行：

```sh
sudo sh deploy/upgrade.sh
```

升级只替换程序。`bot.db` 里的上下文还在。Grok 命令行本身用 `grok update` 单独更新。
