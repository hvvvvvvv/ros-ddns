# WebUI 与 HTTP API

启动程序后打开 <http://127.0.0.1:8080/>；自定义地址见 `api.listen`，Docker 使用映射后的宿主机端口。

## 页面

- **Dashboard**：查看源地址、记录值、同步阶段和错误，点击「立即同步」触发一次同步。
- **Items**：添加、复制、启用或禁用项目，编辑接口、域名和地址列表。
- **Config**：编辑基础参数、RouterOS 和 DNS 服务商。
- **Logs**：查看实时日志，按级别筛选、搜索、暂停显示或复制结果。
- **Notifications**：查看配置变更、异常与恢复通知，仅在页面内显示。

## 编辑与保存

- Config 使用表单编辑，与 Items 共享草稿；「保存并应用」提交完整配置并热重载。
- `api.listen`、`api.token`、`log.format` 只读，需修改配置文件并重启。
- API token 只显示设置状态，配置接口返回 `********` 掩码，保存时保留原值。
- 禁用项目可作为未完成草稿，只校验 name 格式和唯一性；启用后完整校验，类型错误始终拒绝。
- 「校验配置」不保存、不联网；保存冲突返回 `409`，保存校验失败返回 `422`。
- 保存会生成同目录 `.bak` 并可能丢失 YAML 注释和排版，未编辑的环境引用会保留。
- 手工改文件不会自动重载，需重启；刷新或关闭页面前先保存草稿。
- 禁用或移除项目不会自动删除已有 DNS 记录或列表条目。

## 日志与同步排障

- `log.level: info` 看地址变化、实际写入和异常恢复等关键事件。
- `log.level: debug` 看每次 RouterOS 请求、选址和对账细节，可在 Config 保存生效。
- WebUI 筛选不能补回服务端未记录的日志，重启也会清空内存日志和通知。
- 「立即同步」只表示请求已排队，仍需有可用且稳定的源地址，结果看 Dashboard 的错误和记录值。
- `ready` 表示无启动配置错误项且所需源轮询曾成功，不代表 DNS 已同步。

## 认证与代理

- 登录框输入 `api.token`，令牌保存在当前标签页会话；回环监听且 token 为空时可无令牌连接。
- 自动生成的 token 到进程启动日志、`docker compose logs` 或 systemd 日志中查找。
- API 使用 `Authorization: Bearer <token>`，不支持 URL token；`/healthz`、`/readyz` 无需认证。
- 远程访问使用 HTTPS 代理，在站点根路径转发，保留 `Host`、`Authorization`、`X-Forwarded-Proto`，关闭 SSE 缓冲并允许长连接。

## HTTP API

除探针外均需按配置携带 Bearer；配置接口使用 `Content-Type: application/json`。

| 方法与路径 | 用途 / 请求 | 返回 |
| --- | --- | --- |
| `GET /healthz` | 存活检查 | `200` |
| `GET /readyz` | 就绪检查 | `200` / `503` |
| `GET /api/v1/status` | 查看项目及目标状态 | `200` |
| `POST /api/v1/items/{name}/sync` | 无需请求体，异步同步 | `202` |
| `GET /api/v1/meta` | 版本、启动时间、认证和就绪状态 | `200` |
| `GET /api/v1/config` | 读取未展开的配置 | `{path, data, hash}` |
| `POST /api/v1/config/validate` | `{data}`，完整配置对象 | `200`，检查 `valid` 和 `errors` |
| `PUT /api/v1/config` | `{baseHash, data}`，hash 取自 GET | `200`，保存并热重载 |
| `GET /api/v1/logs`、`GET /api/v1/notifications` | 历史日志 / 通知 | `200`，各路径加 `/stream` 为 SSE |

常见错误：`400` 请求格式、`401` token、`403` 来源检查、`404` 项目不存在或不可触发、`409` 配置冲突、`422` 校验失败或修改了需重启字段；校验接口本身用 `200 + valid:false` 表示配置不合法。

```bash
curl http://127.0.0.1:8080/healthz
curl -H "Authorization: Bearer $ROS_DDNS_API_TOKEN" http://127.0.0.1:8080/api/v1/status
curl -X POST -H "Authorization: Bearer $ROS_DDNS_API_TOKEN" http://127.0.0.1:8080/api/v1/items/home/sync
```

字段和示例见 [配置参考](configuration.md)，服务日志命令见 [部署指南](deployment.md)。
