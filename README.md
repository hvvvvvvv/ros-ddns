# ros-ddns

[![Go](https://img.shields.io/badge/Go-1.26.2%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

用 Go 编写的单二进制动态 DNS 守护进程：从 RouterOS 7 路由器读取接口地址，自动维护阿里云解析（Alidns）的 A / AAAA 记录，并可将地址同步到 RouterOS 防火墙的 `address-list`。

## 特性

- **IPv4 / IPv6 双栈**：每个同步项独立轮询；IPv6 支持按接口 MAC 派生的 EUI-64 地址。
- **一个地址，多个目标**：同一地址可同时更新多个域名，也可只维护 `address-list`。
- **稳定后写入**：新地址需在稳定窗口内保持不变才更新；支持启动校验、周期校验和失败自动重试。
- **内置 WebUI**：单个二进制，无需 Node.js、前端构建或外部 CDN；提供运行概览、配置编辑（保存即热重载）、实时日志和通知中心。
- **可观测**：HTTP 健康检查、状态查询和手动同步 API。
- **易部署**：原生支持 systemd / Windows 服务，另提供 Docker Compose 部署。
- **凭据安全**：凭据只从环境变量或配置文件读取，日志输出自动脱敏。

## 快速开始

### 准备

- RouterOS 7，启用 HTTPS 管理，并准备一个专用 REST 账号，见 [RouterOS 配置指南](docs/routeros-setup.md)。
- 域名托管在阿里云解析，准备具备解析记录读写权限的 RAM AccessKey，见 [阿里云配置指南](docs/aliyun-setup.md)。

### Docker Compose

```bash
cp deploy/docker/config.example.yaml data/config.yaml
cp .env.example .env
chmod 600 .env data/config.yaml
# 编辑 .env 与 data/config.yaml：凭据、RouterOS 地址、接口和域名
docker compose up -d --build
```

打开 <http://127.0.0.1:8080/>，用 WebUI 查看状态、继续配置。Linux 下容器以 `10001` 用户运行，如无法写入 `data/`，执行 `sudo chown -R 10001:10001 data`。

### 从源码运行

需要 Go 1.26.2 或更高版本。

```bash
cp configs/config.example.yaml config.yaml
export ROS_PASSWORD='路由器密码'
export ALI_ACCESS_KEY_ID='AccessKey ID'
export ALI_ACCESS_KEY_SECRET='AccessKey Secret'
export ROS_DDNS_API_TOKEN='一段随机长 token'

go run ./cmd/ros-ddns -config config.yaml -check   # 校验配置
go build -o ros-ddns ./cmd/ros-ddns
./ros-ddns -config config.yaml                     # 前台运行，Ctrl+C 停止
```

Windows PowerShell：

```powershell
$env:ROS_PASSWORD = '路由器密码'
$env:ALI_ACCESS_KEY_ID = 'AccessKey ID'
$env:ALI_ACCESS_KEY_SECRET = 'AccessKey Secret'
$env:ROS_DDNS_API_TOKEN = '一段随机长 token'

.\scripts\go.cmd run ./cmd/ros-ddns -config config.yaml -check
.\scripts\go.cmd build -o build\bin\ros-ddns.exe ./cmd/ros-ddns
.\build\bin\ros-ddns.exe -config config.yaml
```

程序不会自动加载 `.env` 文件；凭据也可以直接写进配置文件（建议 `0600` 权限）。配置文件不存在时，程序会生成一份默认配置并启动 WebUI，可直接在界面里完成设置。

## WebUI

| 运行概览 | 同步项目 |
| :---: | :---: |
| ![运行概览](docs/images/webui-dashboard.png) | ![同步项目](docs/images/webui-items.png) |

启动守护进程后访问 `http://<api.listen>/`（默认 <http://127.0.0.1:8080/>）；设置 `api.token` 后需登录。配置保存后热重载，`api.listen`、`api.token`、`log.format` 除外（需重启）。更多说明见 [WebUI 使用指南](docs/webui.md)。

## 配置

示例配置：[configs/config.example.yaml](configs/config.example.yaml)。完整字段说明见 [配置参考](docs/configuration.md)。

| 配置段 | 说明 |
| --- | --- |
| `log` | 日志级别与格式 |
| `runtime` | 轮询间隔、稳定窗口、校验周期等 |
| `state` | 状态缓存路径；留空表示只保存在内存 |
| `api` | WebUI / API 监听地址与访问 token |
| `routeros` | RouterOS 地址、账号与 TLS 设置 |
| `dns` | 阿里云解析 provider 与凭据 |
| `items` | 同步项：来源接口、域名、`address-list` |

凭据字段支持 `${VAR}` 形式的环境变量引用（在 YAML 解析后展开）。

## 命令行

| 参数 | 说明 |
| --- | --- |
| `-config` | 配置文件路径，默认 `config.yaml` |
| `-check` | 只加载和校验配置，不创建或修改配置文件 |
| `-version` | 输出版本、提交和构建时间 |
| `-service` | 服务管理：`install` / `uninstall` / `start` / `stop` / `restart` / `status` |
| `-service-user` | `install` 时记录的服务账号 |
| `-lock-file` | 单实例锁路径，默认使用 `state.path + ".lock"` |

```bash
# 以系统服务方式安装并启动（systemd / Windows 服务通用）
sudo ./ros-ddns -service install -config /var/lib/ros-ddns/config.yaml -service-user ros-ddns
sudo ./ros-ddns -service start
sudo ./ros-ddns -service status
```

安装、升级、回滚与 Docker 部署的完整步骤见 [生产部署指南](docs/deployment.md)。退出码：`0` 成功，`1` 运行失败，`2` 参数或配置错误。

## HTTP API

默认监听 `127.0.0.1:8080`。设置 `api.token` 后，`/api/v1` 下的接口需携带 `Authorization: Bearer <token>`；`/healthz` 和 `/readyz` 公开。

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/api/v1/status -H "Authorization: Bearer $ROS_DDNS_API_TOKEN"
curl -X POST http://127.0.0.1:8080/api/v1/items/home-ipv4/sync -H "Authorization: Bearer $ROS_DDNS_API_TOKEN"
```

| 接口 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查 |
| `GET /readyz` | 就绪检查，未就绪返回 `503` |
| `GET /api/v1/status` | 当前运行状态与各同步项结果 |
| `POST /api/v1/items/{name}/sync` | 手动触发指定项同步；异步执行，`202` 仅表示已受理 |

完整字段和示例见 [WebUI 使用指南](docs/webui.md) 与 [架构设计](docs/architecture.md)。

## 安全建议

- API 默认只监听回环地址；改为对外监听时必须设置非空 `api.token`，并限制来源或通过 HTTPS 反向代理访问。
- 配置文件、凭据文件和状态文件建议设为 `0600` 并限制所在目录权限。日志会脱敏密码、AccessKey、STS token 等凭据，但状态文件仍包含域名、IP、RecordId 等信息，不应公开。
- 为 RouterOS 和阿里云分别使用最小权限账号，并定期轮换凭据。

## 已知限制

- 一个进程只连接一台 RouterOS，一次部署只运行一个实例；多实例部署需隔离配置、端口、状态与锁文件。
- 仅支持阿里云解析的 A / AAAA 记录，不支持其他 DNS 服务商或 RouterOS 6 原生 API。
- DNS 只新增和更新记录，不会删除或清理孤儿记录；修改域名、RR 或线路后旧记录需要手动处理。
- 源地址不可用、程序停止或路由器重启期间，动态 `address-list` 可能过期或消失，恢复后自动补回，续期为尽力而为。
- EUI-64 仅用于合成 IPv6 地址，不分配地址也不保证可达；目标使用隐私地址或非 MAC IID 时需自行核实。
- 云端生效时间受轮询、重试、TTL 和解析套餐影响，不保证固定的全网生效时间。

## 文档

- [生产部署指南](docs/deployment.md)：单实例锁、systemd / Windows 服务、Docker、升级与回滚。
- [WebUI 使用指南](docs/webui.md)：页面操作、配置热重载、HTTP API、实时日志与通知。
- [配置参考](docs/configuration.md)：完整字段、默认值、环境变量展开与校验规则。
- [架构设计](docs/architecture.md)：组件、同步流程、状态机与失败模型。
- [RouterOS 配置指南](docs/routeros-setup.md)：HTTPS、REST 账号权限与验证。
- [阿里云配置指南](docs/aliyun-setup.md)：RAM 授权、记录写入、线路与 TTL。

## 许可证

本项目以 [MIT 许可证](LICENSE) 开源，Copyright (c) 2026 hvvvvvvv。
