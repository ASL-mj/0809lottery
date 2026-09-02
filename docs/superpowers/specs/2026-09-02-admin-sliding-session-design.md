# 管理员滑动会话有效期设计

## 目标

将星额台管理员登录后的本地工作台会话从当前 `12 小时`改为滑动 `7 天`有效期。用户只要持续访问工作台，服务端会话和浏览器 Cookie 都会续期到当前时间之后 7 天；连续 7 天没有访问则会话自然失效并要求重新登录。

本需求只影响星额台管理员认证，不改变 0809 账号的远端 Token、Cookie、刷新令牌或平台会话生命周期。

## 现状与边界

- 管理员会话由 `internal/web/admin_auth.go` 的进程内 `adminSessionStore` 保存。
- `adminSessionTTL` 同时用于服务端过期时间和 `workbench_session` Cookie 的 `Expires`/`MaxAge`。
- 会话目前只校验有效性，不在请求过程中更新过期时间。
- 服务重启后进程内管理员会话会清空，现有行为保持不变。
- HTTP Basic Auth 是兼容入口，不创建或续期 Cookie 会话。

## 设计

### 有效期常量

将管理员会话 TTL 设为 `7 * 24 * time.Hour`，保留单一常量作为服务端和 Cookie 的共同口径。

### 会话续期 API

将会话存储的校验操作从只读 `valid(token, now) bool` 扩展为能返回续期时间的原子操作，例如 `renew(token, now) (expiresAt time.Time, ok bool)`：

1. 校验 Token 非空并在锁内查找记录。
2. 清理已过期记录；过期判断使用 `!expiresAt.After(now)`。
3. 找到有效记录后，将记录更新为 `now.Add(adminSessionTTL)`。
4. 返回新的过期时间；无效或过期 Token 返回失败，不新增会话。

保留一个只读校验包装或调整现有测试辅助方法，避免把续期逻辑散落到路由处理器中。

### 管理员认证中间件

`withAdminAuth` 在 Cookie 会话认证成功时拿到新的 `expiresAt`，继续处理请求，并重新写出同名 `workbench_session` Cookie。Cookie 属性保持当前安全配置：`Path=/`、`HttpOnly`、按请求判断 `Secure`、`SameSite=Lax`；只更新 `Expires` 与 `MaxAge`。

认证优先级保持不变：

- 有效 Cookie 会话：续期并放行。
- Cookie 会话无效：尝试显式 Basic Auth；Basic Auth 放行但不设置 Cookie。
- 两者都无效：API 返回 401，页面重定向到 `/login`。

登录成功仍创建新会话并下发 7 天 Cookie；注销仍删除服务端记录并下发 `MaxAge=-1` 的清除 Cookie。

### 并发与安全

- 续期、过期清理和撤销在同一互斥锁内完成，避免并发请求把已撤销会话重新续期。
- 不扩大 Token 权限或持久化范围；会话 Token 仍为随机 32 字节并仅存于 HttpOnly Cookie。
- 续期不记录 Token、Cookie 或管理员凭据到日志。
- 每次请求都会续期，因此高频请求可能持续保持会话；这是滑动会话的预期行为。没有请求超过 7 天仍会失效。

## 测试与验收

新增回归测试：

1. 登录响应的 Cookie `MaxAge` 和 `Expires` 约为 7 天。
2. 有效 Cookie 请求成功后，服务端过期时间被延长到请求时间 + 7 天，并响应新的 Cookie。
3. 续期后的会话在原始 7 天边界之后仍可用，只要中间有请求续期。
4. 超过最后一次访问 7 天的会话仍返回未认证。
5. 撤销后并发或后续请求不能续期旧 Token。
6. Basic Auth 请求仍兼容且不会额外下发工作台会话 Cookie。
7. 注销会删除会话并清除 Cookie。

验收命令：`go test ./... -count=1`、`go vet ./...`、`make build`、`git diff --check`。本地真实浏览器验证只检查管理员会话的 Cookie 续期，不触碰 0809 账号登录和远端会话。
