# 07 · 错误处理与重试

> 目标：用稳定方式区分鉴权错误、资源不存在、可重试错误和 lease 过期。

## APIError

服务端非 2xx 响应会转换成 `*proxyharbor.APIError`：

```go
var apiErr *proxyharbor.APIError
if errors.As(err, &apiErr) {
    log.Printf("status=%d code=%s message=%s request_id=%s",
        apiErr.StatusCode, apiErr.Code, apiErr.Message, apiErr.RequestID)
}
```

`Code` 对应服务端返回的 `reason` / error code，适合做业务分支。

## 判断函数

```go
switch {
case proxyharbor.IsUnauthorized(err):
    // key 错误、tenant 被禁用、admin 权限不足
case proxyharbor.IsNotFound(err):
    // lease / proxy / provider 不存在
case proxyharbor.IsRetryable(err):
    // transport error、429、5xx 等
case proxyharbor.IsLeaseExpired(err):
    // AutoReacquire=false 时本地 lease 过期
}
```

## SDK 自动重试

SDK 默认会对 transport error 和 408 / 429 / 5xx 做有限重试。不会重试：

- 400 bad request
- 401 / 403 鉴权问题
- 404 not found
- 409 conflict
- 410 lease expired / gone

这些错误通常需要业务修正参数、刷新 key 或重新获取 lease，而不是盲目重试。

## 推荐处理姿势

```go
proxy, err := client.GetProxy(ctx, proxyharbor.WithKey("worker-1"))
if err != nil {
    if proxyharbor.IsUnauthorized(err) {
        return fmt.Errorf("proxyharbor credentials rejected: %w", err)
    }
    if proxyharbor.IsRetryable(err) {
        return fmt.Errorf("proxyharbor temporarily unavailable: %w", err)
    }
    return err
}
```

## 不要吞掉错误

拿代理失败时，不建议 silently fallback 到直连。直连会绕过资源隔离，也可能把业务 IP 暴露给目标站点。

