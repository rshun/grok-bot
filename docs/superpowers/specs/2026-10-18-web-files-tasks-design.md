# 联网搜索、目录读写、定时任务：设计说明

日期：2026-10-18
分支：`dev/web-files-tasks`（基于 `origin/master` c1ea5ae）

## 1. 背景和目的

grok-bot 现在只能做纯文字问答：联网搜索、文件工具、shell 全部关闭。用户希望在 Telegram / Discord 私聊里：

1. 让 Grok 能联网搜索和打开网页读全文；
2. 让 Grok 能读、写用户指定并授权的目录，其他地方碰不到；
3. 用一句自然语言创建定时任务，到点由 Grok 执行并把结果发回这个聊天。

三项一起实现，在同一个分支上完成。

## 2. 目标、非目标、反目标

**目标**

- 联网：Grok 自己决定何时搜索（`web_search`）和打开网页（`web_fetch`），bot 不实现自己的搜索。
- 文件：在聊天里用命令管理允许访问的目录（只读 / 可读写），限制由 grok 沙箱在内核层面强制执行，不只靠提示词。
- 定时：`/task` 后面写自然语言，由 Grok 解析成时间规则；到点用独立的新会话执行，结果发回创建任务的聊天。

**非目标**

- 不开放 shell（`run_terminal_cmd` 等继续禁用）。
- 不支持群聊，仍然只处理私聊。
- 不做 cron 表达式，不做任务结果的历史存档。
- 不在 Windows 上支持文件功能（grok 沙箱只支持 Linux / macOS）。

**反目标（即使功能能用，出现以下情况也算失败）**

- Grok 能读到 grok 登录凭证（`~/.grok/auth.json`）、bot 的环境文件（含 Bot Token）或 `bot.db`。
- 已有聊天在升级后，未经 `/reset` 就拿到了不受目录限制的文件工具。
- 升级后已有聊天的上下文被意外清空。
- 定时任务把内容混进用户平时聊天的上下文。
- Grok 能自己修改沙箱配置，从而扩大访问范围。

## 3. 约束

- 不新增 Go 依赖（全局规范红线）。cron、TOML 等都不引入库。
- 数据库结构变更必须向后兼容：旧版程序遇到新列、新表能照常运行，方便回滚。
- 生产环境是 Debian + systemd，服务以专用系统用户运行（已执行过 `grok login`）。grok CLI 版本 1.0.62。
- 现有行为不变：每个聊天按顺序处理、队列上限、引用原消息回复、Discord 斜杠命令先延迟确认。
- grok 沙箱的特性（来自 grok 文档 `18-sandbox.md`、`22-permissions-and-safety.md`）：
  - 会话创建时的沙箱 profile 会随会话保存，**恢复会话时不能换成别的 profile**，否则直接报错。
  - 自定义 profile 写在 `~/.grok/sandbox.toml`（全局）或 `.grok/sandbox.toml`（项目）。显式指定的自定义 profile 一旦无法生效，grok 会拒绝启动，不会在没有限制的情况下继续运行。
  - `read_only` / `read_write` 只接受目录的字面路径，不支持通配符；`deny` 由内核强制执行，支持通配符。
  - 内置的 `strict` profile：只能读工作目录、系统路径和 `~/.grok`，只能写工作目录、`~/.grok/sessions` 和临时目录，子进程默认断网。
  - `web_search` / `web_fetch` 是在 grok 进程内部发请求的，不受子进程断网的影响。

## 4. 已定下的方案（用户决定）

| 事项 | 决定 |
|---|---|
| 搜索方式 | AI 搜索，`web_search` 和 `web_fetch` 都放开 |
| 目录配置方式 | 在聊天里用命令动态添加或删除（用户已知晓：聊天账号被盗时风险更高） |
| 已有聊天 | 保留原上下文，但不开放文件工具；`/reset` 开新会话后才能用 |
| 定时任务创建方式 | 自然语言，由 Grok 理解 |
| 定时任务执行上下文 | 每次执行都用一个独立的新会话 |

## 5. 设计

### 5.1 联网搜索

- `grok.Client.Reply` 不再传 `--disable-web-search`；`disallowedTools` 里去掉 `web_search`、`web_fetch`。
- 新增环境变量 `WEB_SEARCH`，默认 `true`；设为 `false` 时恢复现在的行为（继续传 `--disable-web-search`，并保留这两个禁用工具）。*（我定的：这样不用重新编译就能关掉。）*
- 系统提示词按实际开放的能力拼接，例如开了联网就加一句"可以搜索网页和打开网页"。

### 5.2 目录读写

**命令**（Telegram 文字命令和 Discord 斜杠命令都提供）：

- `/dir`：列出当前允许的目录和权限，并说明这个聊天的会话能不能用文件功能。
- `/dir add ro /绝对路径`、`/dir add rw /绝对路径`：添加只读 / 可读写目录；目录已存在时改为新权限。
- `/dir del /绝对路径`：移除。

*（我定的：目录名单所有聊天共用，允许名单里的用户都可以管理。名单里本来就都是受信任的人，没必要按聊天分开。）*

**添加目录时的检查**（不通过就拒绝，并说明原因）：

- 必须是绝对路径，`filepath.Clean` 之后不能含通配符，不能有首尾空白；
- 路径必须存在且是目录（以服务用户的身份 `os.Stat`）；
- 不能是 `/`，也不能是下面这些敏感位置，或者它们的上级目录、下级目录：`~/.grok`、`~/.ssh`、`DATA_DIR`（里面有 `bot.db`）、`/etc`、`/proc`、`/sys`、`/dev`、`/boot`、`/root`，以及 `BOT_ENV_FILE`（新增环境变量，默认 `/etc/grok-tg-bot.env`）所在的位置；
- 最终能访问到哪里，还受服务用户自身的 Unix 权限限制。这是最后一道防线，文档里要写明。

**沙箱 profile**：

- 所有**新**会话一律带 `--sandbox grokbot`。profile 继承 `strict`，再加上：
  - `read_only` = 名单里的只读目录，`read_write` = 名单里的可读写目录；
  - `deny` = `~/.grok/auth.json`、`~/.ssh`、`BOT_ENV_FILE`、`DATA_DIR/bot.db`、`DATA_DIR/bot.db-wal`、`DATA_DIR/bot.db-shm`（展开为绝对路径，不用通配符）、`**/.env`、`**/*.pem`；
  - `restrict_network = false`，避免 MCP 服务器（子进程）被断网。
- 名单为空时，profile 仍然生效，这时只能访问工作目录，相当于"有文件工具，但没东西可读"。
- profile 写在服务用户的 `~/.grok/sandbox.toml`。原因：`strict` 下 Grok 写不了 `~/.grok/`（`sessions` 子目录除外），而工作目录是可写的，profile 不能放在那里。
  - 文件第一行带 bot 的标记注释，bot 每次调用 grok 之前，按数据库里的名单重新生成整个文件（先写临时文件再改名替换，避免写到一半）。
  - **如果文件已存在但没有标记**（说明是人工写的），bot 不覆盖它；文件功能视为不可用，同时记一条错误日志，`/dir` 也提示管理员手动合并。
- 名单改了之后，在**下一次**调用 grok 时生效。已经带 `grokbot` 的会话，如果 grok 每次恢复会话都重新读取 profile 内容，就能直接用上新名单；如果不重新读取，需要 `/reset`。这一点必须在服务器上实测（见第 7 节），`/dir add` 的回复文案按实测结果来写。

**文件工具开放规则**：

- `chats` 表新增一列 `sandbox TEXT NOT NULL DEFAULT ''`，记录会话创建时用的 profile。
- 新会话（包括 `/reset` 之后、会话失效后自动重建的）写入 `grokbot`。
- 只有 `sandbox = 'grokbot'` 的会话才开放 `read_file`、`list_dir`、`grep`、`search_replace`、`write`，系统提示词也同时说明可以读写哪些目录。
- `sandbox = ''` 的老会话：恢复时**不传** `--sandbox`（传了会报错），文件工具继续禁用，上下文照常保留。`/dir` 里提示"发送 /reset 后可以使用文件功能"。
- 当前平台不支持沙箱（Windows），或者 `sandbox.toml` 由人工维护时，新会话也不传 `--sandbox`、不开放文件工具，`sandbox` 列留空。这时 `/dir add` 和 `/dir del` 照常修改名单，但回复里说明"当前文件功能不可用"以及原因。

### 5.3 定时任务

**创建**：`/task 每天早上九点帮我总结科技新闻`

1. bot 用一个独立的新会话调用 grok，带上 `--json-schema`，并且禁用全部工具。提示里给出当前时间和时区，让 Grok 只输出结构化结果：
   - `kind`：`once` / `daily` / `weekly` / `interval`
   - `at`（`once` 时的本地日期时间）、`time`（`HH:MM`）、`weekdays`（0–6）、`every_minutes`
   - `prompt`：到点要让 Grok 做的事
   - `summary`：一句中文复述，例如"每天 09:00：总结科技新闻"
   - 解析不出来时返回 `error`，附一句说明
2. bot 校验结果，任何一项不通过都回复"没看懂时间，请换个说法"：
   - 一次性任务的时间必须在将来；
   - 间隔不少于 `TASK_MIN_INTERVAL`（默认 60 分钟），防止消耗太多额度；
   - 每个聊天最多 `TASK_LIMIT` 个任务（默认 20）。
3. 校验通过后保存，回复：`已创建 #3：每天 09:00：总结科技新闻。下次执行：10-19 09:00。`

*（我定的：直接创建、不再二次确认。复述加编号已经让用户看清楚了，错了用 `/task del` 删掉就行。）*

**管理**：`/task list`（编号、复述、下次执行时间），`/task del 编号`。Discord 斜杠命令为 `/task`，子命令 `add`（参数 text）、`list`、`del`（参数 id）。

**执行**：

- 后台每 30 秒检查一次 `next_run <= now` 的任务。*（我定的）*
- 到点后，把任务按"来自这个聊天的一条消息"放进**该聊天的队列**。这样同一个聊天不会同时跑多个 grok，也沿用现有的队列上限：队列满时这次执行跳过，并记日志。不管执行了还是跳过，`next_run` 都照常推到下一次，避免每 30 秒重试一遍。
- 执行用**新会话**（新的 UUID，用 `-s`），使用该聊天当前的模型、联网设置和沙箱规则。新会话一律带 `grokbot`，文件权限跟随当前目录名单。会话 ID **不写回** `chats` 表。
- 结果用普通消息发回聊天（不引用任何消息），开头加一行 `⏰ 定时任务 #3：每天 09:00：总结科技新闻`。失败时发送用户能看懂的错误提示。
- 执行后计算下一次运行时间；一次性任务执行后删除。*（我定的：执行完就删，不留历史。）*
- bot 停机期间错过的执行**不补跑**，启动时把过期任务直接推到下一次；过期的一次性任务发一条"已错过"后删除。*（我定的）*
- 时区：新增环境变量 `TIMEZONE`（IANA 名称，例如 `Asia/Shanghai`），默认用服务器本地时区。*（我定的）*

**发送**：定时任务没有触发它的原消息，所以各平台需要一个能"主动发送到某个聊天"的发送器。Telegram 用 `chat_id`，Discord 的私聊频道 ID 就是 `chat_id`，两边的 `Bot.Send` 都已经支持。main 里按平台名把发送器交给调度器。

### 5.4 数据库变更

只做新增，旧程序可以照常读写：

```sql
-- 老会话标记
ALTER TABLE chats ADD COLUMN sandbox TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS dirs (
    path TEXT PRIMARY KEY,
    mode TEXT NOT NULL CHECK (mode IN ('ro', 'rw')),
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    spec TEXT NOT NULL,        -- 解析后的规则 JSON
    prompt TEXT NOT NULL,
    summary TEXT NOT NULL,
    next_run TEXT NOT NULL,    -- UTC RFC3339
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_next_run ON tasks (next_run);
```

- `ALTER TABLE` 之前先用 `PRAGMA table_info(chats)` 判断列是否已存在，保证重复启动不会出错。
- 升级前备份 `DATA_DIR/bot.db`（README 里给出 `sqlite3 .backup` 或者停服务后复制的命令）。
- 迁移后只读验证：`PRAGMA table_info(chats);` 应包含 `sandbox` 列；`SELECT count(*) FROM chats WHERE sandbox = '';` 应等于升级前的聊天数；`SELECT name FROM sqlite_master WHERE type = 'table';` 应包含 `dirs` 和 `tasks`。
- 回滚：直接换回旧二进制即可。旧代码只按列名读 `session_id`、`model`，用不到的列和表会被忽略。

### 5.5 配置汇总（新增环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `WEB_SEARCH` | `true` | 是否允许联网搜索和打开网页 |
| `BOT_ENV_FILE` | `/etc/grok-tg-bot.env` | 用于禁止 Grok 读取这个文件，也禁止把它加入目录名单 |
| `TIMEZONE` | 服务器本地 | 定时任务的时区 |
| `TASK_LIMIT` | `20` | 每个聊天最多几个任务 |
| `TASK_MIN_INTERVAL` | `60m` | 周期任务的最小间隔 |

`deploy/grok-tg-bot.env.example` 和 README 同步更新，示例里只写占位值。

## 6. 留给实现者决定的

- 代码放在哪：建议新增 `internal/sandbox`（生成 profile 和校验路径）、`internal/schedule`（解析规则、计算下次执行时间、调度循环），`store` 里新增对应的增删查方法。
- `--json-schema` 的具体写法，以及 `ParseHeadless` 要不要为结构化输出做调整。
- 系统提示词的具体措辞。
- `/help` 和 Discord 命令描述的文案。
- 测试怎么组织。要求至少覆盖：
  - 各种开关组合下，传给 grok 的参数是否正确；
  - 路径校验，包括所有敏感位置以及它们的上级、下级目录；
  - profile 文件的生成；有标记 / 没有标记时是否覆盖；
  - 四种任务规则下一次执行时间的计算，包括跨天、跨周、错过执行；
  - 数据库迁移可以重复执行；
  - 老会话不开放文件工具、恢复时不传 `--sandbox`。

## 7. 必须在服务器上实测的风险

Windows 开发机不支持 grok 沙箱，以下几点只能在测试实例（Debian）上验证。有一项不通过，就停下来请用户决定，不要自行放宽限制：

1. **禁止访问 `~/.grok/auth.json` 会不会让 grok 自己登录失败。** 如果会，要换一种方式保护凭证，例如改用 `read-only` 作为基础 profile，或者干脆接受这个风险。这一项需要用户决定。
2. **基于 `strict` 时，现有 MCP 服务器还能不能正常启动、调用。** `strict` 限制子进程只能读工作目录和系统路径，Node / Python 写的 MCP 服务器可能读不到自己的文件。如果不能，就要在"把 MCP 需要的目录加进只读名单"和"放宽基础 profile"之间选一个，这一项也需要用户决定。
3. **名单改了之后，带 `grokbot` 的老会话恢复时能不能用上新名单**（决定 `/dir add` 的提示文案）。
4. **`~/.grok/sandbox.toml` 在 `strict` 下是否确实写不进去**（用 Grok 自己尝试写一次来确认）。
5. 内核版本至少 5.13，`deny` 里的读拒绝需要 `bubblewrap`。服务器上缺这个包时 grok 会拒绝启动。安装与否由用户决定，bot 不自动安装。

## 8. 验证方式

- 离线回归测试：`GOTOOLCHAIN=local GOPROXY=off go test -mod=readonly ./...`
- 测试实例上手动检查：
  - 问一个需要最新信息的问题，确认会搜索；设 `WEB_SEARCH=false` 后确认不再搜索。
  - 老聊天：确认上下文还在、读不了文件，`/dir` 提示用 `/reset`。
  - `/reset` 之后：`/dir add ro /srv/test` 能读，写入时被拒绝；`/dir add rw` 之后能写；名单以外的目录、`~/.grok/auth.json`、环境文件都读不到。
  - `/task 两分钟后提醒我喝水`：到点收到消息，任务自动删除。
  - `/task 每 60 分钟…`：`/task list` 显示正确的下次执行时间，`/task del` 删除后不再执行。
  - 重启服务后任务还在，错过的执行不补跑。

## 9. 回滚

- 源码：本分支没有合并前，直接切回 `master` 即可。
- 服务器：恢复上一个版本的二进制，重启服务。新增的列和表不影响旧程序，不需要动数据库（具体操作要先确认再执行）。
- `~/.grok/sandbox.toml`：旧程序不会用到它，可以留着。确实要移除时，由用户确认后手动移动到备份目录。
