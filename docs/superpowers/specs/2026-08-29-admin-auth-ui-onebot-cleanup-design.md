# 管理员登录界面、OneBot 清理与浏览器 UA 设计

## 状态

已确认登录页视觉原型 C：沿用现有工作台的浅灰背景、细边框、黑色主按钮和低饱和绿色状态色。本文用于确认实际认证边界，之后进入实现。

## 目标与边界

本次变更只处理三件事：

1. 删除项目内已经不再使用的 OneBot/QQ 配置和代码引用。
2. 让所有 0809 平台请求默认使用指定的 macOS Chrome User-Agent。
3. 将管理员认证从浏览器原生 Basic Auth 弹窗升级为工作台内的登录页面，错误以内联状态展示，不调用 `alert`。

不改变多账号业务接口、账号凭据 Vault、平台会话复用策略、签到/抽奖/订阅逻辑，也不增加 QQ 机器人功能。

## 当前实现事实

- 当前工作台的 `serve` 运行路径已经不读取 `ACCOUNT_*` 账号凭据；动态账号凭据由页面写入 AES-256-GCM 加密的本地 Vault。
- 项目当前状态文件为 v4，账号注册表只保存脱敏元数据。
- OneBot 配置只残留在项目私有环境文件中，当前 Go 代码没有有效的 OneBot 路由或客户端引用。
- 配置代码和示例已经有目标 Chrome UA，但项目私有环境文件仍覆盖为旧的 `SkyeLotteryBot/1.0`，因此运行时实际使用旧值。
- 管理员认证目前由 `withBasicAuth` 统一拦截，未认证请求返回 `WWW-Authenticate`，浏览器因此弹出无法定制样式的原生对话框。

## 方案决策

### OneBot/QQ 清理

- 从项目内私有环境文件删除 `ONEBOT_URL`、`ONEBOT_TOKEN`、`TARGET_QQ`。
- 对项目代码、文档、部署配置做全仓搜索；当前没有需要保留的 OneBot 实现，搜索结果应为零。
- 不触碰 `LOTTERY_BASE_URL`、工作台管理员配置、Vault 配置和平台账号业务代码。
- 已迁移到 v4 Vault 的旧 `ACCOUNT_*` 变量不参与正常 `serve`。实现时先做本地 Vault 可解密验证，再从项目私有运行环境中移除这些迁移遗留变量；`config.example.env` 只保留注释形式的迁移说明。

### User-Agent

项目私有环境文件的 `USER_AGENT` 固定为：

```text
Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36
```

配置代码默认值和示例文件继续保持相同值。运行时由 `lottery.NewClient` 和所有服务调用读取这一配置，不新增按账号覆盖。

### 管理员认证

#### 路由

- `GET /login`：公开登录页，设置现有 CSRF Cookie，返回嵌入的登录 HTML。
- `POST /api/admin/login`：公开登录接口，接收管理员用户名和密码；通过同源校验、CSRF 校验和常量时间比较验证后建立本地工作台会话。
- `POST /api/admin/logout`：已认证的退出接口，清除当前工作台会话 Cookie。
- `GET /`：已认证时返回现有工作台；未认证时重定向到 `/login?next=/`。
- 现有 `/api/*`：已认证时行为不变；未认证返回 JSON `401`，不再发送 `WWW-Authenticate`，防止浏览器重新弹原生 Basic Auth 对话框。

#### 会话

- 新增进程内会话表，键为 `crypto/rand` 生成的高熵随机 ID，不落盘、不进入日志、不进入 API 响应。
- Cookie 名称使用 `workbench_session`，属性为 `HttpOnly`、`SameSite=Lax`、`Path=/`；只有 HTTPS 请求设置 `Secure`，以兼容本机 HTTP 开发端口。
- 会话采用 12 小时绝对有效期，每次请求校验过期时间；服务重启后会话失效，需要重新登录。这不会创建或销毁 0809 平台账号会话。
- `withAdminAuth` 优先验证会话 Cookie，同时接受请求显式携带的正确 Basic Auth 作为兼容入口，但不再主动发出 Basic Auth challenge。这样旧的命令行健康检查可以继续使用，浏览器走新登录页。

#### 登录页交互

- 页面沿用已确认的 C 版工作台风格：单列浅色面板、细边框、黑色主按钮、绿色本机状态标识。
- 表单包含管理员账号、管理员密码、密码显隐按钮和提交按钮；输入框有可见焦点状态。
- 提交期间按钮禁用并显示“登录中”，避免重复提交。
- 账号或密码错误、请求失败、会话过期都显示在表单内部的错误区域，使用 `role="alert"` 和可读文本；不调用 `window.alert`。
- 成功后按白名单限制的 `next` 参数跳转，非法或缺失值回到 `/`，避免开放重定向。
- 页面不显示平台账号密码、Token、Cookie 或远端会话 ID。

#### 错误与安全边界

- 登录失败统一返回“管理员账号或密码不正确”，不区分用户名不存在和密码错误。
- 登录接口不触发任何 0809 平台登录，不创建远端平台会话。
- CSRF 中间件继续保护所有写请求，登录请求也必须带同源请求头和 CSRF 双提交令牌。
- 退出接口只撤销本地工作台会话，不调用平台账号登出接口。
- 管理员密码仍来自私有运行环境；本次不把管理员密码写进状态文件或账号 Vault。

## 数据流

```text
浏览器 GET /                 -> 未认证 -> 303 /login?next=/
浏览器 GET /login             -> 下发 CSRF Cookie + 登录 HTML
浏览器 POST /api/admin/login  -> 校验 Origin/CSRF + 管理员凭据
                              -> 内存创建工作台会话 + HttpOnly Cookie
浏览器 GET /                 -> Cookie 校验 -> 现有账号工作台
浏览器 POST /api/admin/logout -> 删除内存会话 + 清除 Cookie
```

OneBot 清理和 UA 变更不改变上述认证数据流；UA 只进入出站 0809 HTTP 请求头。

## 验证计划

1. `rg` 检查项目代码、文档、部署配置和项目环境中不再有 OneBot/QQ 配置或引用。
2. 用不输出值的配置检查确认 `USER_AGENT` 与指定字符串完全一致；增加/更新配置测试和出站请求头测试。
3. HTTP 测试覆盖：未认证页面重定向、登录页可访问、错误凭据返回 401 且无 `WWW-Authenticate`、成功登录设置安全 Cookie、Cookie 可访问受保护 API、退出后 Cookie 失效、Basic Auth 兼容入口仍可用。
4. 登录页静态检查确认没有 `alert(`，错误节点带 `role="alert"`，表单控件有标签，移动端没有横向溢出。
5. 运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、`git diff --check` 和项目 Makefile 的健康检查。

## 回滚边界

代码回滚只涉及管理员认证中间件、登录页和新增会话表；现有账号状态文件、加密 Vault 和平台账号会话不做迁移或删除。配置回滚只恢复 UA/已删除的死配置，不回滚业务数据。
