# grok-bot

在 Telegram 或 Discord 的私聊里发文字，这台机器上已登录的 Grok 订阅会接着上下文回复。不使用按量 API key。两个平台的上下文彼此分开。

收到消息后先回复「收到，正在处理。」，处理完再发结果。同一个聊天里的消息按顺序处理。

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
