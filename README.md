# grok-bot

基于本机已登录 `grok` CLI 订阅的 **Telegram & Discord 双平台私人 AI 助手**（零按量 API Key 计费）。  
A self-hosted **Telegram & Discord private AI assistant** powered by your local logged-in `grok` CLI subscription — **no pay-per-token API keys required**.

**[中文文档](#中文文档) | [English Documentation](#english-documentation)**

---

## 中文文档

- [功能亮点](#功能亮点)
- [快速部署](#快速部署)
- [环境变量说明](#环境变量说明)
- [聊天命令一览](#聊天命令一览)
- [核心功能详解](#核心功能详解)
  - [消息交互体验与跨平台会话接力](#1-消息交互体验与跨平台会话接力)
  - [标准发音、AI 生图与收发文件](#2-标准发音ai-生图与收发文件)
  - [外链安全预抓取与联网搜索](#3-外链安全预抓取与联网搜索)
  - [系统原生定时任务与本机脚本任务](#4-系统原生定时任务与本机脚本任务)
  - [本机命令行主动推送 (`grok-bot send`)](#5-本机命令行主动推送-grok-bot-send)
  - [安全沙箱、目录读写与执行权限](#6-安全沙箱目录读写与执行权限)
- [升级、测试与故障排查](#升级测试与故障排查)

---

### 功能亮点

- **零 API 账单**：直接复用服务器本机 `grok login` 订阅态，无需配置付费 LLM、语音或图床 API Key。
- **双平台独立对话 + 一键跨端接力**：普通对话上下文按平台各自独立（在 Telegram 提问就在 Telegram 回复，在 Discord 提问就在 Discord 回复），同时支持通过 `/sessions` 或内联按钮**在 TG 与 Discord 之间一键接力同一段上下文**。
- **双端自动共享记忆与任务**：长期记忆（`/memory`）与定时任务（`/task`）在 Telegram 与 Discord 之间实时同步共享；定时任务触发时自动双端同时推送。
- **多模态开箱即用**：支持多图识别（含 Telegram 相册合并）、Grok 4.7 原生 AI 生图/改图、联网安全找图、单词/多语种标准 `.mp3` 发音条、外链防 SSRF 正文预抓取（GitHub / X / 网页）、服务器文件安全下发与表情回应。
- **系统级定时调度 + 零额度脚本任务**：自动对接 Linux `crontab`、macOS `launchd`、Windows 任务计划程序；支持将固定业务脚本（`.py` / `.sh` / 可执行程序）绑定为 `/task script`，到点直接运行脚本，不调 Grok、不耗额度，失败自动双端告警。
- **本机主动推送 CLI**：提供 `grok-bot send` 子命令，服务器上的监控、量化或业务脚本可直接复用机器人配置向私聊推送文字与报表文件。
- **纵深安全隔离**：白名单数字 ID 鉴权、子进程敏感环境变量剥离、`~/.grok/sandbox.toml` + `bubblewrap` 内核级禁读黑名单、每轮对话一次性随机端口本地 MCP 令牌。

---

### 快速部署

#### 1. 前置条件

1. 准备一台运行环境（推荐 Debian / Ubuntu Linux，同样支持 macOS 与 Windows），创建一个专用低权限系统用户（例如 `grok`）。
2. 切换到该用户，安装官方 `grok` CLI 并完成登录认证：
   ```sh
   su - grok
   grok login
   ```
3. 创建 Telegram Bot（通过 `@BotFather` 获取 Token）和/或 Discord Bot（在 Discord Developer Portal 创建应用并获取 Bot Token）。
   - **Discord 私聊配置提示**：Discord 仅处理私聊消息，**无需开启** `Message Content Intent`。将机器人邀请进你所在的任意服务器（勾选 `bot` 与 `applications.commands` 权限范围）后即可向其发起私聊。首次启动注册斜杠命令后可能需要数分钟生效。

#### 2. 获取二进制文件

从 [GitHub Releases](https://github.com/rshun/grok-bot/releases) 下载对应平台预编译静态二进制（附 `SHA256SUMS` 校验文件）：

| 平台 / 架构 | 发布文件名 |
| :--- | :--- |
| Linux x86_64 | `grok-bot-linux-amd64`（同 `grok-bot`） |
| Linux ARM64 | `grok-bot-linux-arm64` |
| macOS Apple Silicon | `grok-bot-darwin-arm64` |
| macOS Intel | `grok-bot-darwin-amd64` |
| Windows x86_64 | `grok-bot-windows-amd64.exe` |

也可以从源码编译：

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o grok-bot ./cmd/grok-bot
```

#### 3. 配置文件与 `systemd` 启动（Linux 示例）

复制仓库内的 `deploy/grok-tg-bot.env.example` 到 `/etc/grok-tg-bot.env`（注意设置权限仅管理员或服务用户可读，切勿提交真实密钥）：

```dotenv
# Telegram 与 Discord 可任选其一，或同时配置双平台
TELEGRAM_BOT_TOKEN=your_telegram_bot_token_here
ALLOWED_USER_IDS=123456789

DISCORD_BOT_TOKEN=your_discord_bot_token_here
DISCORD_ALLOWED_USER_IDS=987654321012345678

DATA_DIR=/var/lib/grok-tg-bot
BOT_ENV_FILE=/etc/grok-tg-bot.env
DEFAULT_MODEL=grok-4.7
GROK_BIN=grok

WEB_SEARCH=true
LINK_PREFETCH=true
ALLOW_SHELL=false
SHOW_PROGRESS=false

TIMEZONE=Asia/Shanghai
TASK_LIMIT=20
TASK_MIN_INTERVAL=60m
SCRIPT_DIRS=/home/grok/bin
SCRIPT_TIMEOUT=30m
```

安装二进制与 `systemd` 单元并启动服务（请确认 `deploy/grok-tg-bot.service` 中的 `User=` 与已执行 `grok login` 的系统用户一致）：

```sh
sudo install -m 0755 grok-bot /usr/local/bin/grok-tg-bot
sudo install -d -o grok -g grok /var/lib/grok-tg-bot
sudo cp deploy/grok-tg-bot.service /etc/systemd/system/grok-tg-bot.service
sudo systemctl daemon-reload
sudo systemctl enable --now grok-tg-bot
```

---

### 环境变量说明

| 变量名 | 必填 | 默认值 | 说明 |
| :--- | :---: | :--- | :--- |
| `TELEGRAM_BOT_TOKEN` | 二选一 | - | Telegram 机器人 Token（与 `DISCORD_BOT_TOKEN` 至少填一个） |
| `ALLOWED_USER_IDS` | 配 TG 必填 | - | 允许访问的 Telegram 数字用户 ID，多个用英文逗号分隔 |
| `DISCORD_BOT_TOKEN` | 二选一 | - | Discord 机器人 Token |
| `DISCORD_ALLOWED_USER_IDS` | 配 DC 必填 | - | 允许访问的 Discord 数字用户 ID，多个用英文逗号分隔 |
| `DATA_DIR` | 否 | `data` | SQLite 数据库（`bot.db`）、工作区与任务日志存储目录 |
| `BOT_ENV_FILE` | 否 | `/etc/grok-tg-bot.env` | 环境配置文件路径（自动加入沙箱与发文件黑名单） |
| `DEFAULT_MODEL` | 否 | `grok-4.7` | 新会话默认使用的 Grok 模型名称 |
| `GROK_BIN` | 否 | `grok` | `grok` CLI 可执行文件路径 |
| `GROK_TIMEOUT` | 否 | `10m` | 单轮 Grok CLI 调用最长超时时间 |
| `GROK_MAX_TURNS` | 否 | `50` | 单轮对话允许的最大工具调用步数（设为 `0` 表示不限制） |
| `QUEUE_LIMIT` | 否 | `8` | 同一聊天允许排队的待处理消息上限 |
| `WEB_SEARCH` | 否 | `true` | 是否允许 Grok 自主联网搜索与打开网页 |
| `LINK_PREFETCH` | 否 | `true` | 是否自动安全预抓取消息中的网页/GitHub/X 链接正文 |
| `ALLOW_SHELL` | 否 | `false` | 启动时初始的服务器命令执行开关（运行中可通过 `/shell` 切换） |
| `SHOW_PROGRESS` | 否 | `false` | 是否发送并动态编辑「处理中… / 正在搜索…」进度消息 |
| `TIMEZONE` | 否 | 服务器本地时区 | 定时任务解析与调度的时区（如 `Asia/Shanghai`） |
| `TASK_LIMIT` | 否 | `20` | 允许保存的定时任务最大数量 |
| `TASK_MIN_INTERVAL` | 否 | `60m` | 周期性定时任务的最小触发间隔 |
| `SCRIPT_DIRS` | 否 | `~/bin` | 允许绑定为脚本任务的目录白名单（绝对路径，多个用逗号分隔） |
| `SCRIPT_TIMEOUT` | 否 | `30m` | 单次脚本任务执行的最长超时时间 |

---

### 聊天命令一览

所有主要命令均提供 **Telegram & Discord 交互式内联按钮**，大多数管理操作也可直接使用自然语言触发：

| 命令 | 用法与说明 |
| :--- | :--- |
| `/status` | 查看运行状态、版本、调度器状态、权限开关、数据统计与 MCP 概览（附快捷操作面板按钮） |
| `/stop` | 立即中断当前正在运行的对话或长任务（同 `/cancel`，或运行中直接发「停止」「停一下」） |
| `/new [备注]` | 归档当前会话并开启新话题（长期记忆继续保留，附一键切回按钮） |
| `/sessions` | 查看跨平台保存的历史会话；`/sessions <编号>` 一键切换/跨端接力；`/sessions del <编号>` 删除归档 |
| `/model [模型名]` | 查看当前模型与 CLI 可用模型列表（附点选切换按钮），切换模型保留当前上下文 |
| `/usage` | 查询当前订阅周期剩余额度 |
| `/memory` | 查看跨平台共享长期记忆；支持 `add <内容>`、`del <编号>`、`clear`，或直接说「长期记忆：…」 |
| `/task` | 管理定时任务与脚本任务（支持一句话自然语言、`cron`、`script`、`bind`、`unbind`、`list`、`run`、`pause`、`resume`、`skip`、`del`） |
| `/mcp` | 管理 MCP 服务：`/mcp` 列表、`doctor [名称]` 连通诊断、`add <名称> <URL 或 -- 命令...>`、`enable\|disable <名称>`、`del <名称>` |
| `/dir` | 查看可写目录白名单；`/dir add rw /绝对路径` 授权写入；`/dir del /绝对路径` 收回权限（或直接说「允许写入 /srv/out」） |
| `/shell` | 查看或切换命令执行权限（`/shell on` / `/shell off`，或直接说「开启命令执行」「关闭命令执行」） |
| `/reset` | 清空当前聊天的上下文关联并换新会话（长期记忆永久保留） |
| `/help` | 查看命令帮助说明 |

---

### 核心功能详解

#### 1. 消息交互体验与跨平台会话接力

- **轻量接收反馈与精准引用**：收到普通消息后，Telegram 自动在原消息上添加 👀 表情回应，Discord 显示正在输入状态，不刷固定文字回执。回复（包括超长消息自动切分的每一段、排队提示及错误提示）均精准引用触发它的原消息；若原消息已被删除则自动降级为普通回复。
- **串行消息队列**：同一会话内的消息严格按序处理。仅当前面仍有未完成消息时提示「已排队，前面还有 N 条。」；超出 `QUEUE_LIMIT` 时提示稍后重发。
- **可选动态进度（`SHOW_PROGRESS=true`）**：开启后先发送一条「处理中…」，在 Grok 搜索或调用工具时动态编辑为「正在搜索：…」「正在调用：…」（节流间隔 3 秒），完成后直接将该消息替换为最终回答。
- **跨平台会话接力（`/sessions`）**：历史会话带有 `[来自 TG]` / `[来自 Discord]` 来源标记。你在电脑端 Discord 聊到一半的话题，出门后可在手机 Telegram 发送 `/sessions` 点击对应按钮无缝接力同一段上下文。

#### 2. 标准发音、AI 生图与收发文件

- **标准语音发音（`grokbot__pronounce`）**：询问单词、短语或句子发音（如「entrepreneur 怎么读？」「对比 schedule 的英美发音」），机器人会给出国际音标（IPA）与发音要点，并直接下发可内联点击播放的 `.mp3` 语音条。英文单词优先拉取词典级真人播音录音（区分 `en-US` / `en-GB`），长句及多语种（中、英、日、韩、法、德、西等）自动切换标准播音级 TTS。
- **AI 生图、改图与联网找图（`image_gen` / `image_edit` / `grokbot__send_web_image`）**：原生接入 Grok 4.7 多模态生图与改图，默认优先采用手机友好的竖屏比例（`2:3` 海报插画或 `9:16` 壁纸，可按要求改为横屏或方形）。联网找图通过服务端防 SSRF 客户端下载并校验真实图片魔数（JPG/PNG/WebP/GIF）与清晰度尺寸，拒绝裂图与追踪像素。内置请求前与流式参数双重内容安全拦截，并预留 `media.ImageAuditor` 视觉终审扩展接口。
- **接收图片与附件**：支持发送压缩照片或原图文件（JPEG/PNG/GIF/WebP，每条消息最多 10 张、单张上限 10MB；Telegram 多图相册自动合并为单次提问），临时文件以 `0600` 权限写入 `DATA_DIR/inbox` 并在当轮结束后立即清理。
- **下发本机文件（`grokbot__send_file`）与表情回应（`grokbot__react`）**：可直接说「把 `/srv/out/report.pdf` 发给我」（每轮最多 10 个文件，Telegram 单文件上限 50MB，Discord 上限 10MB），或由 Grok 根据语境在你的消息上添加表情回应（如 👍）。

#### 3. 外链安全预抓取与联网搜索

- **外链自动预抓取（`LINK_PREFETCH=true`）**：消息中包含 `http://` 或 `https://` 链接时（单条消息最多提取前 3 个），机器人在调用 Grok 前会自动通过内置防 SSRF 客户端（拦截回环地址、RFC1918 局域网、CGNAT、Link-Local 等内网网段）抓取上下文：
  - **GitHub**：自动解析 PR、Issue、Commit、Blob 源码链接并提取标题、正文、变更统计与代码内容。
  - **X / Twitter**：自动识别 `x.com/.../status/...` 推文链接，提取作者、正文、长文标题及引用推文。
  - **普通网页 / JSON / 文本**：自动提取网页标题与去噪后的正文摘要。
- **自主联网搜索（`WEB_SEARCH=true`）**：默认开启，由 Grok 按需决定搜索关键词与访问页面。

#### 4. 系统原生定时任务与本机脚本任务

##### Grok 智能定时任务
直接发送 `/task 每天早上 8:30 汇总今日科技要闻`（或在普通对话中自然语言描述），Grok 会自动解析为一次性、每天、每周、每月几号、月末最后一天、固定分钟间隔或标准 5 段 Cron 表达式。

- **操作系统原生调度器驱动**（无需依赖进程内内存轮询）：任务变更时自动同步到当前系统调度服务，到点由系统直接调起 `grok-bot run-task <编号> --data-dir <目录> --env-file <配置>`：
  - **Linux / Unix**：托管写入当前用户的 `crontab`（`# BEGIN GROK-BOT TASKS` ... `# END GROK-BOT TASKS`）。
  - **macOS**：写入 `~/Library/LaunchAgents/com.grokbot.task.<编号>.plist` 并通过 `launchctl` 加载。
  - **Windows**：注册至系统任务计划程序（`\GrokBot\Task_<编号>`）。
- 每次触发均使用独立会话，不污染日常聊天上下文，结果以 `⏰ 定时任务 #编号` 双端推送到 Telegram 与 Discord。

##### 本机脚本任务（`/task script` / `/task bind`）
对于逻辑固定或耗时的自动化任务（如定时下单、数据采集、日报生成），可直接绑定本机脚本执行，**不调用 Grok、不占会话、零额度消耗**：

```text
# 新建脚本任务：5 段 Cron + 脚本绝对路径（不得含空格）+ 可选任务说明
/task script 59 7 * * 1-5 /home/grok/bin/auto_breakfast.py 工作日早餐自动下单

# 将已有的 #3 定时任务切换为运行本机脚本（执行时间保持不变）
/task bind 3 /home/grok/bin/auto_breakfast.py

# 解绑脚本，恢复为普通 Grok 提示词任务
/task unbind 3
```

- **执行规则**：`.py` 使用 `python3`（Windows 优先 `python`），`.sh` 使用 `/bin/sh`，其余文件需具备可执行权限；工作目录为脚本所在目录，超时上限为 `SCRIPT_TIMEOUT`（默认 `30m`）。
- **目录白名单与防篡改**：仅允许绑定 `SCRIPT_DIRS`（默认 `~/bin`）目录下的脚本；校验软链接真实路径，拒绝全局可写（world-writable）文件；仅认用户本人显式发送的 `/task script` 或 `/task bind` 命令，Grok 自然语言解析无权绑定或替换脚本。
- **静默成功与失败告警**：退出码为 `0` 时机器人不额外发消息（由脚本按需调用 `"$GROK_BOT_BIN" send` 主动推送）；退出码非 `0` 或超时时，机器人自动截取末尾输出双端推送告警。完整日志保存在 `<DATA_DIR>/task-logs/task-<编号>.log`（满 2MB 自动轮转）。
- **无密钥注入环境**：脚本运行环境自动剥离 Bot Token 与用户白名单，并注入 `GROK_BOT_BIN`、`BOT_ENV_FILE`、`DATA_DIR`、`GROK_BOT_TASK_ID`。

#### 5. 本机命令行主动推送 (`grok-bot send`)

服务器上的外部脚本、定时任务或监控程序无需另写机器人客户端，直接调用 `grok-bot send` 即可推送消息与附件（自动探测程序目录或 `/etc/grok-tg-bot.env` 配置，并复用发文件安全黑名单校验）：

```sh
# 1. 默认同时推送到所有已配置平台（Telegram + Discord）的最近活跃私聊
grok-bot send "盘前检查完成：今日无异常"

# 2. 仅推送到指定平台（-p tg 或 -p dc）
grok-bot send -p tg "仅发送到 Telegram"
grok-bot send -p dc "仅发送到 Discord"

# 3. 推送文字并附带本机报表文件（支持多次指定 -f / --file）
grok-bot send "今日成交报表" -f /srv/reports/fills.csv

# 4. 通过标准输入管道推送命令输出
df -h | grok-bot send
```

#### 6. 安全沙箱、目录读写与执行权限

- **环境变量隔离**：`grok-bot` 启动 `grok` 子进程前会剥离 `TELEGRAM_BOT_TOKEN`、`DISCORD_BOT_TOKEN`、允许名单、`BOT_ENV_FILE` 等敏感变量，即使子进程读取 `/proc/self/environ` 也无法获取机器人凭证。
- **目录读写控制（Linux / macOS）**：
  - **读取**：受操作系统用户权限与沙箱黑名单双重控制。`~/.grok/auth.json`、`~/.ssh`、`BOT_ENV_FILE`、`bot.db`、所有 `*.env` 与 `*.pem` 私钥文件一律禁止读取与外发。
  - **写入**：默认只读。必须由你本人在聊天中发送 `/dir add rw /srv/out` 或明确说「允许写入 `/srv/out`」才会持久化授权（严禁授权 `/`、`/etc`、`/proc`、`/sys`、`/root`、`~/.grok`、`~/.ssh`、`DATA_DIR` 等敏感路径）。
  - **沙箱配置托管**：自动维护 `~/.grok/sandbox.toml`（首行标记 `# managed by grok-bot; do not edit`，若已存在手动配置则绝不覆盖）。若 Linux 未安装 `bubblewrap`（`bwrap`），机器人会自动省略 `--sandbox` 降级运行并依靠系统用户权限、提示词约束及发文件黑名单兜底；安装 `bubblewrap` 后新会话自动启用内核级沙箱（要求 Linux 内核 >= 5.13）。
- **命令执行开关（`/shell`）**：默认关闭（`ALLOW_SHELL=false`），需要时可通过 `/shell on` 临时开启，用完建议 `/shell off` 关闭。
- **本地一次性 MCP 通道**：表情回应、发文件、标准发音与安全找图通过监听在 `127.0.0.1` 随机端口的内置 MCP 服务实现（配置写入独立的 `DATA_DIR/workspace/.grok/config.toml`，不改动全局 `~/.grok/config.toml`），每轮对话签发一次性随机令牌且仅限操作当前会话。

> [!IMPORTANT]
> **部署安全建议**：由于 Grok 具备读取运行用户可读文件及联网访问网页的能力，为防范恶意网页提示词注入诱导读取本机文件，**请务必使用专用的低权限系统用户（如 `grok`）运行本服务**，不要与存放生产核心密钥的 `root` 或高权限账号混用。

---

### 升级、测试与故障排查

#### 平滑升级与备份

升级前建议先备份 `bot.db` 数据库（升级仅替换二进制程序，会话上下文、长期记忆与定时任务均完整保留）：

```sh
# 1. 停止服务并备份数据库
sudo systemctl stop grok-tg-bot
sudo cp -p /var/lib/grok-tg-bot/bot.db /var/lib/grok-tg-bot/bot.db.bak-$(date +%Y%m%d)

# 2. 使用内置升级脚本（需已登录 gh CLI）或手动替换新二进制后启动
sudo sh deploy/upgrade.sh
# 若手动替换二进制，执行：sudo systemctl start grok-tg-bot
```

> `grok` CLI 本身可通过 `grok update` 独立更新。如需回滚，停止服务后恢复旧版二进制并重启即可。

#### 离线回归测试

本地已具备 Go 模块缓存时，可禁用外网下载运行全量单元测试：

```sh
# Linux / macOS
GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...
```

```powershell
# Windows PowerShell
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
go test -mod=readonly ./...
```

#### 常见故障排查

- **提示 `unknown model id`**：表示当前会话绑定的模型名称在最新版 `grok` CLI 中已下线或更名。请在报错平台发送 `/model` 查看当前可用模型列表，再点击按钮或发送 `/model <可用模型名>` 切换即可（**切勿先发 `/reset`**，切换模型可直接保留原有会话上下文）。
- **排查 CLI 与服务日志**：可在服务器以只读方式检查 `grok --version` 及 `sudo journalctl -u grok-tg-bot -n 60 --no-pager`（对外分享日志前请注意脱敏隐藏 Token、Session ID 与用户 ID）。

---
---

## English Documentation

- [Highlights](#highlights)
- [Quick Start & Deployment](#quick-start--deployment)
- [Environment Variables](#environment-variables)
- [Chat Commands](#chat-commands)
- [Feature Deep Dive](#feature-deep-dive)
  - [Messaging UX & Cross-Platform Session Handoff](#1-messaging-ux--cross-platform-session-handoff)
  - [Pronunciation Audio, AI Image Gen & File Sharing](#2-pronunciation-audio-ai-image-gen--file-sharing)
  - [SSRF-Safe Link Prefetch & Web Search](#3-ssrf-safe-link-prefetch--web-search)
  - [OS-Native Scheduled Tasks & Script Tasks](#4-os-native-scheduled-tasks--script-tasks)
  - [Proactive CLI Notifications (`grok-bot send`)](#5-proactive-cli-notifications-grok-bot-send)
  - [Security Architecture, Sandbox & Permissions](#6-security-architecture-sandbox--permissions)
- [Upgrade, Testing & Troubleshooting](#upgrade-testing--troubleshooting)

---

### Highlights

- **Zero API Token Billing**: Runs directly on top of an authenticated local `grok` CLI subscription (`grok login`). No pay-per-token LLM, TTS, or image-hosting API keys needed.
- **Per-Platform Contexts + One-Click Cross-Platform Handoff**: Regular conversations stay independent on Telegram and Discord by default, while `/sessions` (and interactive inline buttons) let you **hand off and continue any conversation seamlessly between Telegram and Discord**.
- **Shared Long-Term Memory & Dual-Platform Tasks**: Long-term memories (`/memory`) and scheduled tasks (`/task`) are automatically shared across Telegram and Discord, with scheduled task results delivered to both platforms simultaneously.
- **Rich Multimodal Capabilities**: Multi-image vision (including Telegram album merging), Grok 4.7 native `image_gen` & `image_edit`, SSRF-safe web image fetching, IPA + inline `.mp3` pronunciation audio bars, SSRF-safe link prefetching (GitHub, X/Twitter, articles), local file delivery, and contextual emoji reactions.
- **OS-Native Task Scheduler + Zero-Quota Script Tasks**: Synchronizes tasks natively with Linux `crontab`, macOS `launchd`, and Windows Task Scheduler. Bind deterministic jobs (`.py`, `.sh`, or binaries) via `/task script` to run directly without invoking Grok or consuming quota — silent on success, automatic dual-platform alerts on failure.
- **Built-In Push CLI (`grok-bot send`)**: Send messages or local report files to your Telegram/Discord DMs straight from server cron jobs, monitoring scripts, or Unix pipes.
- **Defense-in-Depth Security**: Strict numeric user ID allowlists, child-process secret environment scrubbing, `~/.grok/sandbox.toml` + `bubblewrap` kernel file deny lists, and ephemeral per-turn random-port localhost MCP tokens.

---

### Quick Start & Deployment

#### 1. Prerequisites

1. A host machine (Debian/Ubuntu Linux recommended; macOS and Windows are also supported) with a dedicated unprivileged system user (e.g., `grok`).
2. Install the official `grok` CLI and log in under that system user:
   ```sh
   su - grok
   grok login
   ```
3. Create a Telegram Bot via `@BotFather` and/or a Discord Bot via the Discord Developer Portal.
   - **Discord DM Setup**: `grok-bot` operates exclusively in Direct Messages and **does not require** the `Message Content Intent`. Invite the bot to any server you share using scopes `bot` and `applications.commands` so you can DM it. Newly registered slash commands may take a few minutes to propagate on first run.

#### 2. Download or Build the Binary

Download a prebuilt static binary from [GitHub Releases](https://github.com/rshun/grok-bot/releases) (verified via `SHA256SUMS`):

| OS / Architecture | Release Artifact |
| :--- | :--- |
| Linux x86_64 | `grok-bot-linux-amd64` (alias: `grok-bot`) |
| Linux ARM64 | `grok-bot-linux-arm64` |
| macOS Apple Silicon | `grok-bot-darwin-arm64` |
| macOS Intel | `grok-bot-darwin-amd64` |
| Windows x86_64 | `grok-bot-windows-amd64.exe` |

Or build from source:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o grok-bot ./cmd/grok-bot
```

#### 3. Configure & Run with `systemd` (Linux)

Copy `deploy/grok-tg-bot.env.example` to `/etc/grok-tg-bot.env` (restrict file permissions and never commit real credentials):

```dotenv
# Configure Telegram, Discord, or both
TELEGRAM_BOT_TOKEN=your_telegram_bot_token_here
ALLOWED_USER_IDS=123456789

DISCORD_BOT_TOKEN=your_discord_bot_token_here
DISCORD_ALLOWED_USER_IDS=987654321012345678

DATA_DIR=/var/lib/grok-tg-bot
BOT_ENV_FILE=/etc/grok-tg-bot.env
DEFAULT_MODEL=grok-4.7
GROK_BIN=grok

WEB_SEARCH=true
LINK_PREFETCH=true
ALLOW_SHELL=false
SHOW_PROGRESS=false

TIMEZONE=Asia/Shanghai
TASK_LIMIT=20
TASK_MIN_INTERVAL=60m
SCRIPT_DIRS=/home/grok/bin
SCRIPT_TIMEOUT=30m
```

Install the binary and `systemd` unit (ensure `User=` in `deploy/grok-tg-bot.service` matches the user that ran `grok login`):

```sh
sudo install -m 0755 grok-bot /usr/local/bin/grok-tg-bot
sudo install -d -o grok -g grok /var/lib/grok-tg-bot
sudo cp deploy/grok-tg-bot.service /etc/systemd/system/grok-tg-bot.service
sudo systemctl daemon-reload
sudo systemctl enable --now grok-tg-bot
```

---

### Environment Variables

| Variable | Required | Default | Description |
| :--- | :---: | :--- | :--- |
| `TELEGRAM_BOT_TOKEN` | Either | - | Telegram Bot token (at least one of Telegram or Discord is required) |
| `ALLOWED_USER_IDS` | If TG enabled | - | Comma-separated numeric Telegram user IDs allowed to use the bot |
| `DISCORD_BOT_TOKEN` | Either | - | Discord Bot token |
| `DISCORD_ALLOWED_USER_IDS` | If DC enabled | - | Comma-separated numeric Discord user IDs allowed to use the bot |
| `DATA_DIR` | No | `data` | Directory for SQLite (`bot.db`), workspace, and task logs |
| `BOT_ENV_FILE` | No | `/etc/grok-tg-bot.env` | Path to the bot `.env` file (automatically blocked from Grok reads/sends) |
| `DEFAULT_MODEL` | No | `grok-4.7` | Default Grok model for new chat sessions |
| `GROK_BIN` | No | `grok` | Path or name of the `grok` CLI executable |
| `GROK_TIMEOUT` | No | `10m` | Maximum timeout per Grok CLI turn |
| `GROK_MAX_TURNS` | No | `50` | Maximum tool-use turns per message (`0` = unlimited) |
| `QUEUE_LIMIT` | No | `8` | Maximum queued messages per chat |
| `WEB_SEARCH` | No | `true` | Allow Grok to search the web and open pages autonomously |
| `LINK_PREFETCH` | No | `true` | Automatically prefetch URLs in incoming messages via an SSRF-safe client |
| `ALLOW_SHELL` | No | `false` | Initial shell execution state on startup (toggleable live via `/shell`) |
| `SHOW_PROGRESS` | No | `false` | Show and dynamically edit a live progress message during tool/search steps |
| `TIMEZONE` | No | System local TZ | IANA timezone for scheduled tasks (e.g., `Asia/Shanghai`, `America/New_York`) |
| `TASK_LIMIT` | No | `20` | Maximum number of scheduled tasks stored |
| `TASK_MIN_INTERVAL` | No | `60m` | Minimum interval allowed for recurring scheduled tasks |
| `SCRIPT_DIRS` | No | `~/bin` | Comma-separated absolute directories allowed for `/task script` bindings |
| `SCRIPT_TIMEOUT` | No | `30m` | Maximum execution duration for a single script task run |

---

### Chat Commands

All major commands include **interactive inline buttons** on both Telegram and Discord, and most settings can also be managed in plain natural language:

| Command | Description |
| :--- | :--- |
| `/status` | View bot version, runtime status, OS scheduler state, permission flags, stats, and MCP overview (with quick-action buttons) |
| `/stop` | Immediately abort the currently running response or task (alias: `/cancel`, or send `"停止"` while running) |
| `/new [note]` | Archive the current session and start a fresh topic (long-term memories remain intact; includes a one-click switch-back button) |
| `/sessions` | List saved sessions across TG & Discord; `/sessions <id>` to switch or hand off cross-platform; `/sessions del <id>` to delete |
| `/model [name]` | View current and available models (with one-click selection buttons); switching models preserves chat context |
| `/usage` | Check remaining Grok subscription quota for the current billing period |
| `/memory` | View shared long-term memories; supports `add <text>`, `del <id>`, `clear`, or natural language |
| `/task` | Manage scheduled & script tasks (`<natural language>`, `cron`, `script`, `bind`, `unbind`, `list`, `run`, `pause`, `resume`, `skip`, `del`) |
| `/mcp` | Manage MCP servers: `/mcp` list, `doctor [name]`, `add <name> <URL or -- cmd...>`, `enable\|disable <name>`, `del <name>` |
| `/dir` | View writable directories; `/dir add rw /abs/path` to grant write access; `/dir del /abs/path` to revoke |
| `/shell` | View or toggle server command execution (`/shell on` / `/shell off`) |
| `/reset` | Clear the current chat's session binding and start fresh (shared `/memory` entries are preserved) |
| `/help` | Display command reference |

---

### Feature Deep Dive

#### 1. Messaging UX & Cross-Platform Session Handoff

- **Lightweight Receipts & Exact Quote-Replies**: Incoming messages receive an instant 👀 reaction on Telegram or a typing indicator on Discord without cluttering the chat with fixed receipt messages. Every response (including multi-chunk splits for long replies, queue notices, and error alerts) quote-replies the exact triggering message, gracefully falling back if the original message was deleted.
- **Per-Chat Serial Queue**: Messages in the same chat are processed strictly in order. Queue notices appear only when another turn is already active, up to `QUEUE_LIMIT`.
- **Optional Live Progress (`SHOW_PROGRESS=true`)**: When enabled, sends an initial status message and updates it every >= 3s as Grok searches or invokes tools, replacing it in place with the final answer.
- **Cross-Platform Handoff (`/sessions`)**: Archived and active sessions display `[来自 TG]` / `[来自 Discord]` tags so you can start a debugging thread on Discord desktop and pick up the exact same session on Telegram mobile.

#### 2. Pronunciation Audio, AI Image Gen & File Sharing

- **Standard Pronunciation (`grokbot__pronounce`)**: Ask how to pronounce any word, phrase, or sentence. Grok replies with IPA phonetic transcription, pronunciation tips, and an inline playable `.mp3` audio bar — pulling studio human dictionary recordings for single English words (`en-US` / `en-GB`) and broadcast-quality multilingual TTS for phrases and sentences.
- **AI Image Generation, Editing & Safe Web Images (`image_gen` / `image_edit` / `grokbot__send_web_image`)**: Powered natively by Grok 4.7 multimodal image generation and editing (defaulting to mobile-friendly `2:3` or `9:16` portrait framing unless landscape/square is requested). Web image requests are fetched server-side via an SSRF-safe client that validates magic bytes (`JPG`/`PNG`/`WebP`/`GIF`) and minimum dimensions. Strict pre-request and streaming content safety filters block NSFW/violent/illegal prompts, with a `media.ImageAuditor` interface hook reserved for visual post-auditing.
- **Image Vision & Local File Delivery**: Send up to 10 photos or uncompressed image files (<= 10MB each; Telegram media albums are automatically merged into one turn). Ask Grok to send local server files via `grokbot__send_file` (up to 10 files/turn; 50MB limit on Telegram, 10MB on Discord) or react to your message with an emoji via `grokbot__react`.

#### 3. SSRF-Safe Link Prefetch & Web Search

- **Automatic Link Prefetch (`LINK_PREFETCH=true`)**: Extracts up to 3 `http://` or `https://` URLs per message and prefetches clean context before calling Grok, while blocking loopback, RFC1918 private networks, CGNAT, and link-local addresses:
  - **GitHub**: Pulls PR, Issue, Commit, and Blob metadata, diffs, file lists, or source text.
  - **X / Twitter**: Extracts post author, full text, article titles, and quoted posts from `x.com/.../status/...` links.
  - **General Web / JSON / Text**: Extracts `<title>` and readable main content.
- **Autonomous Web Search (`WEB_SEARCH=true`)**: Enabled by default so Grok can search and browse external documentation as needed.

#### 4. OS-Native Scheduled Tasks & Script Tasks

##### AI Scheduled Tasks
Create tasks in natural language (e.g., `/task Summarize top tech news every weekday at 8:30 AM`) or with explicit 5-field cron syntax (`/task cron 30 8 * * 1-5 ...`).

- **OS-Native Scheduling**: Instead of relying on an in-memory ticker, `grok-bot` registers tasks directly with the host OS scheduler to invoke `grok-bot run-task <id> --data-dir <dir> --env-file <env>`:
  - **Linux / Unix**: Managed block inside the service user's `crontab` (`# BEGIN GROK-BOT TASKS` ... `# END GROK-BOT TASKS`).
  - **macOS**: `~/Library/LaunchAgents/com.grokbot.task.<id>.plist` via `launchctl`.
  - **Windows**: Windows Task Scheduler under `\GrokBot\Task_<id>`.
- Runs in an isolated session without polluting your main chat history and delivers `⏰` results to both Telegram and Discord.

##### Local Script Tasks (`/task script` / `/task bind`)
Offload deterministic or long-running jobs (e.g., automated ordering, scrapers, ETL reports) to local scripts while managing their schedule from chat — **zero Grok invocations and zero quota usage**:

```text
# Create a new script task: 5-field cron + script absolute path (no spaces) + optional label
/task script 59 7 * * 1-5 /home/grok/bin/auto_breakfast.py Weekday breakfast order

# Bind an existing task #3 to run a local script (schedule stays unchanged)
/task bind 3 /home/grok/bin/auto_breakfast.py

# Unbind script and restore #3 back to a Grok prompt task
/task unbind 3
```

- **Execution**: `.py` runs via `python3` (`python` first on Windows), `.sh` via `/bin/sh`, and other files directly as executables inside the script's directory, bounded by `SCRIPT_TIMEOUT` (default `30m`).
- **Strict Path Allowlist**: Scripts must reside inside `SCRIPT_DIRS` (default `~/bin`), pass realpath symlink resolution, and not be world-writable. Only explicit user commands (`/task script` / `/task bind`) can bind scripts — natural-language Grok parsing cannot create or swap script bindings.
- **Silent Success & Automatic Failure Alerts**: On exit code `0`, the bot stays quiet so the script can push custom notifications via `"$GROK_BOT_BIN" send`. On non-zero exit or timeout, the bot pushes the trailing output as an alert to both Telegram and Discord. Full output is logged to `<DATA_DIR>/task-logs/task-<id>.log` (2MB rotation).
- **Scrubbed Environment**: Bot tokens and user allowlists are stripped from the script's environment; `GROK_BOT_BIN`, `BOT_ENV_FILE`, `DATA_DIR`, and `GROK_BOT_TASK_ID` are provided automatically.

#### 5. Proactive CLI Notifications (`grok-bot send`)

External scripts, cron jobs, and monitoring hooks on the same host can push messages and files to your DMs without needing separate bot credentials:

```sh
# 1. Push to all configured platforms (Telegram + Discord)
grok-bot send "Pre-market check passed with zero anomalies"

# 2. Target a specific platform (-p tg or -p dc)
grok-bot send -p tg "Telegram only notification"
grok-bot send -p dc "Discord only notification"

# 3. Attach one or more local files (validated against the secret path deny list)
grok-bot send "Daily execution report" -f /srv/reports/fills.csv

# 4. Pipe command output from stdin
df -h | grok-bot send
```

#### 6. Security Architecture, Sandbox & Permissions

- **Child-Process Secret Scrubbing**: `TELEGRAM_BOT_TOKEN`, `DISCORD_BOT_TOKEN`, user allowlists, `DATA_DIR`, and `BOT_ENV_FILE` are stripped before spawning `grok`, preventing exposure via `/proc/self/environ`.
- **Filesystem Access & Kernel Sandbox (Linux / macOS)**:
  - **Reads**: `~/.grok/auth.json`, `~/.ssh`, `BOT_ENV_FILE`, `bot.db`, `*.env`, and `*.pem` are strictly blocked from both Grok reads and file sends.
  - **Writes**: Read-only by default. Persistent write access requires an explicit command (`/dir add rw /srv/out`) or direct user phrasing from an allowlisted user. System paths (`/`, `/etc`, `/proc`, `/sys`, `/root`, `~/.grok`, `~/.ssh`, `DATA_DIR`) can never be granted write access.
  - **Managed `sandbox.toml`**: `grok-bot` generates `~/.grok/sandbox.toml` (marked `# managed by grok-bot; do not edit`; never overwrites user-authored files). If `bubblewrap` (`bwrap`) is not installed on Linux, `grok-bot` gracefully omits `--sandbox` and relies on OS user permissions, prompt rules, and file-send deny lists; installing `bubblewrap` on Linux >= 5.13 automatically enables kernel sandboxing for new turns without restarting the daemon.
- **Shell Execution Gate (`/shell`)**: Disabled by default (`ALLOW_SHELL=false`); toggleable via `/shell on` and `/shell off`.
- **Ephemeral Per-Turn Local MCP**: Built-in actions (reactions, file sends, pronunciation, web images) run over a localhost random-port MCP server configured in `DATA_DIR/workspace/.grok/config.toml` with a single-turn random bearer token scoped strictly to the active chat.

> [!IMPORTANT]
> **Security Best Practice**: Because Grok can read files accessible to the service user and browse external web pages, prompt-injection payloads on untrusted websites could attempt to exfiltrate readable local files. **Always run `grok-bot` under a dedicated unprivileged OS user (e.g., `grok`)** that has no read access to unrelated secrets on the host.

---

### Upgrade, Testing & Troubleshooting

#### Safe Upgrade & Backup

Always back up `bot.db` before upgrading (replacing the binary preserves all sessions, memories, and scheduled tasks):

```sh
# 1. Stop service and back up SQLite database
sudo systemctl stop grok-tg-bot
sudo cp -p /var/lib/grok-tg-bot/bot.db /var/lib/grok-tg-bot/bot.db.bak-$(date +%Y%m%d)

# 2. Upgrade via helper script (requires authenticated gh CLI) or replace binary manually
sudo sh deploy/upgrade.sh
```

> Update the underlying Grok CLI independently via `grok update`. To roll back, restore the previous `grok-bot` binary and restart the service.

#### Offline Regression Tests

When the Go module cache is already populated locally, run the full test suite offline:

```sh
GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...
```

#### Troubleshooting

- **`unknown model id` error**: Indicates the model previously selected for that chat was deprecated or renamed in the `grok` CLI. Run `/model` in that chat to view currently available models and switch via `/model <name>` (**do not run `/reset` first**, as switching models preserves your conversation context).
- **Inspecting Logs**: Check `grok --version` and `sudo journalctl -u grok-tg-bot -n 60 --no-pager` in read-only mode. Always redact tokens, session IDs, and user IDs before sharing logs.
