# 星额台

这是一个本地 0809 多账号工作台。浏览器访问时先在 /login 完成管理员认证，工作台使用本地短期会话 Cookie；脚本和健康检查也可以显式发送 HTTP Basic Auth。账号数量不再受固定环境变量限制，可以在工作台中自助新增、编辑、停用、重新认证和删除。每个账号的操作相互独立：一个账号会话失效不会遮蔽其他账号的结果，也不会产生跨账号统计。

## 功能

- 单页展示多个账号，顶部胶囊 Tab 切换，每个账号一张独立管理卡片。
- 账号管理：新增、编辑显示名/登录名/密码、停用、启用、删除（需显式确认）。
- 认证健康状态徽标；只读「检查认证」；卡片上显式「重新认证」是唯一的密码登录入口。
- 显示有效订阅、有限订阅的剩余额度、到期时间和最近查询状态；所有额度以可溯源的美元快照返回，来源、换算规则版本与查询时间一并展示，无法确认换算规则时明确显示「额度换算待确认」而不是伪美元值。
- 对单个账号执行每日签到、领取每日赠送抽奖次数、查询抽奖次数、手动抽奖、查询活动信息、购买抽奖次数与当日通行证；全部保留幂等与结果核对。
- 定时抽奖完全由用户自定义：每个账号可配置任意多条计划，两种类型——固定时间（每天到点执行一次）或时间段随机（每天在自定义时段内随机时刻执行一次，北京时间 HH:MM，暂不支持跨零点）；没有配置就不会执行；重启后不重复执行；认证失效时计划记为跳过/失败并写入脱敏日志，绝不自动登录。
- 账户余额查询：显示剩余额度、已用额度与请求次数，余额以可溯源美元快照展示并保存为历史快照。
- 在状态文件中复用已有 Cookie、父访问令牌和活动令牌；令牌、Cookie 与密码仅保存在 AES-256-GCM 加密的本地金库（SecretVault）中。
- 运行日志保留七天，全部脱敏展示。

工作台不提供批量操作、跨账号统计或任务调度。登录名、密码、Cookie、令牌与远端会话标识永远不会进入页面、API 响应或日志。

## 认证与会话保护

认证统一经过 SessionBroker，优先级固定为：有效已保存令牌 → 已保存父令牌的无副作用验证 → 刷新 Cookie 换取新令牌 → **用户显式重新认证**。页面打开、普通查询、失败重试、自动抽奖或服务重启都不会触发密码登录；单账号的认证恢复串行化，等待请求复用第一次的结果。

平台存在约 50 个会话的数量上限。工作台通过可配置的会话上限（默认 50）、安全余量（默认 5）和持久会话保留数（默认 2，可配 1–3）做容量保护；仅在显式登录前检查容量。

平台的会话列表（`GET /api/user/sessions`）、定点撤销（`DELETE /api/user/sessions/{sid}`）与退出其他会话（`POST /api/user/sessions/revoke-others`）接口已由用户核实接入。登录/刷新后工作台从令牌的 `sid` 声明记录自己创建的会话台账。「会话清理」对话框展示实时会话列表（含登录方式、IP、解析后的设备摘要；原始 UA 不出服务端），撤销完全由用户手动决定：每个会话一个撤销按钮，顶部提供「退出其他所有会话」。撤销当前会话后账号标记为需要重新认证。显式登录前若达到容量阈值，工作台先自动清理台账能证明的旧工作台会话，仍不足时拒绝登录并给出指引；无法核实容量（令牌已失效）时放行登录，避免把账号锁死。

密码、Cookie、令牌保存在 AES-256-GCM 加密的 Vault 文件中（每次写入使用新随机 nonce，原子落盘，文件 0600、目录 0700）。账号注册表只保存脱敏元数据：显示名、掩码登录名（如 u***@example.test）、启用状态与认证健康状态。管理员登录只建立本地工作台会话，不会触发 0809 平台登录；所有变更接口受本地会话（或显式 Basic Auth）、同源校验与 CSRF 令牌（X-CSRF-Token 双提交）保护。

## 配置

1. 生成 32 字节 Vault 密钥并妥善保存：`openssl rand -base64 32`，写入私有环境文件的 `LOTTERY_VAULT_KEY`。密钥丢失后已保存的凭据无法恢复，需要逐账号重新认证。
2. 参照 `config.example.env` 设置 `STATE_PATH`、`LOTTERY_VAULT_PATH`、`WEB_USERNAME`、`WEB_PASSWORD`；文件权限设为 `600`。不要在环境文件中放置任何账号密码。
3. 会话保护参数 `LOTTERY_SESSION_LIMIT`、`LOTTERY_SESSION_SAFETY_MARGIN`、`LOTTERY_DURABLE_SESSIONS` 保持默认即可。
4. 保持 `WEB_ADDR` 绑定在 `127.0.0.1`，由 HTTPS 反向代理对外提供访问。

## 从旧版本迁移

旧版本使用五组 `ACCOUNT_*` 环境变量。一次性迁移命令把它们连同已保存的令牌、Cookie 和用户 ID 导入 Vault，然后原子地把状态文件升级到版本 4；任何一次 Vault 写入校验失败都不会改动状态文件，版本 4 的状态文件重复执行迁移会直接返回「no migration needed」。

```bash
# 以包含旧 ACCOUNT_* 变量的私有环境文件运行一次
./lottery-bot migrate
# 之后正常运行；环境变量中的账号凭据不再被读取
./lottery-bot serve
```

## 运行

在项目目录中使用 Makefile 启动。默认只读取项目内的 `lottery-bot.env`、`data/state.json` 和 `data/vault.json`；Makefile 会拒绝指向其他目录的 `ENV_FILE`。

```bash
make run
```

默认监听 `127.0.0.1:18090`。部署前运行：

```bash
go test ./...
```

## Web API

浏览器接口使用 GET /login 建立本地管理员会话；脚本可显式发送 HTTP Basic Auth。写接口还要求同源与 X-CSRF-Token（GET /login 或 GET / 下发的 Cookie 值）。

- GET /login：管理员登录页面（公开访问并下发 CSRF Cookie）。
- POST /api/admin/login：校验管理员凭据并建立本地工作台会话。
- POST /api/admin/logout：撤销当前本地工作台会话。
- GET /：多账号工作台页面（需要本地会话或显式 Basic Auth）。
- `GET /api/health`：服务健康检查。
- `GET /api/accounts`：账号卡片列表（脱敏元数据 + 认证健康 + 业务快照）。
- `POST /api/accounts`：新增账号（`label`、`login_name`、`password`），仅写入 Vault，不触发登录。
- `PATCH /api/accounts/{account_id}`：编辑显示名、状态、登录名或密码。
- `DELETE /api/accounts/{account_id}`：删除账号，请求体需 `{"confirmation":"DELETE"}`；先停用，再删除凭据与账号级业务数据。
- `POST /api/accounts/{account_id}/validate`：只读认证检查，不创建会话。
- `POST /api/accounts/{account_id}/reauthenticate`：显式重新认证，请求体需 `{"confirm":true}`。
- `GET /api/accounts/{account_id}/session-preview`：实时会话列表（会话 ID、是否当前、登录方式、IP、解析后的设备摘要与时间戳；原始 UA 不出服务端）。
- `POST /api/accounts/{account_id}/sessions/revoke`：撤销指定会话（`{"sid":"..."}`）；撤销当前会话会标记账号需要重新认证。
- `POST /api/accounts/{account_id}/sessions/revoke-others`：退出除当前会话外的所有设备（`{"confirm":true}`）。
- `POST /api/accounts/{account_id}/sessions/cleanup`：仅撤销台账可证明的工作台旧会话（容量保护内部使用，UI 不做候选门控）。
- `POST /api/accounts/{account_id}/checkin`：每日签到。
- `POST /api/accounts/{account_id}/claim`：领取每日赠送抽奖次数（不自动抽奖）。
- `POST /api/accounts/{account_id}/draw`：手动抽奖一次。
- `POST /api/accounts/{account_id}/draw-history`：读取平台 `lottery/api/dashboard` 返回的最近中奖记录（仅返回脱敏的奖品、状态、时间与美元额度）。
- `POST /api/accounts/{account_id}/activity`：刷新活动信息。
- `POST /api/accounts/{account_id}/purchase-draw`：购买一次抽奖（价格来自实时活动信息）。
- `POST /api/accounts/{account_id}/unlock-pass`：购买当日通行证。
- `POST /api/accounts/{account_id}/balance`：查询账户余额（剩余/已用额度与请求次数）。
- `POST /api/accounts/{account_id}/token-usage`：查询当日消耗 Token、调用次数与美元额度（复用已有会话）。
- `GET|PUT /api/accounts/{account_id}/draw-schedule`：读取或整体替换该账号的定时抽奖计划列表（`fixed` 指定时刻 / `random` 时段随机）。
- `POST /api/draw-count/query`、`POST /api/subscriptions/query`：单账号只读查询。
- `GET /api/auto-draw-status`、`GET /api/runtime-logs`：当日计划状态（含自定义计划标签）与脱敏运行日志。

空账号、`all` 和未知账号会被拒绝；停用账号上的业务动作返回 409。

## 验证

测试覆盖认证保护、CSRF 与同源校验、账号 CRUD 与凭据隔离、删除确认与账号级清理、显式重新认证、无隐式登录回归探针、公开响应脱敏、幂等动作、自动抽奖计划与精确额度换算。会话清理的远端对接留待平台契约确认后的独立计划实现。
