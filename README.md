# ros-ddns

[![Go](https://img.shields.io/badge/Go-1.26.10%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

用 Go 编写的单二进制动态 DNS 守护进程：从 RouterOS 7 路由器读取接口地址，自动维护阿里云解析（Alidns）的 A / AAAA 记录，并可将地址同步到 RouterOS 防火墙的 `address-list`。

## 特性

- **IPv4 / IPv6 双栈**：每个同步项独立轮询；IPv6 支持按接口 MAC 派生的 EUI-64 地址。
- **一个地址，多个目标**：同一地址可同时更新多个域名，也可只维护 `address-list`。
- **稳定后写入**：新地址需在稳定窗口内保持不变才更新；支持启动校验、周期校验和失败自动重试。
- **内置 WebUI**：单个二进制，无需 Node.js、前端构建或外部 CDN；提供运行概览、配置编辑（保存即热重载）、实时日志和通知中心。
- **可观测**：HTTP 健康检查、状态查询和手动同步 API。
- **易部署**：原生支持 systemd / Windows 服务，另提供 Docker Compose 部署。
- **凭据管理**：支持环境变量或配置文件；常规日志脱敏，启动自动生成的 API token 会在启动日志中打印一次。

## 快速开始

### 准备

- RouterOS 7，启用 HTTPS 管理，并准备一个专用 REST 账号，见 [RouterOS 配置指南](docs/routeros-setup.md)。
- 域名托管在阿里云解析，准备具备解析记录读写权限的 RAM AccessKey，见 [阿里云配置指南](docs/aliyun-setup.md)。

### Docker Compose

```bash
mkdir -p data
sudo chown 10001:10001 data   # 容器以该用户写入 data/
cp .env.example .env
chmod 600 .env
# 编辑 .env：RouterOS 凭据、预先设置合法非空 API token、ROS_DDNS_LISTEN=0.0.0.0:8080；
# 容器首次启动生成 data/config.yaml，DNS 服务商与同步项目在 WebUI 添加
docker compose up -d --build
```

首次启动后打开 <http://127.0.0.1:8080/>，在 WebUI 中添加 DNS 服务商及其凭据、同步项目，并按需调整 RouterOS 设置。Linux 下容器以 `10001` 用户运行，`data/` 必须允许该用户写入：`sudo chown 10001:10001 data`。首次启动生成的 `data/config.yaml` 属主同样是 `10001`。

非回环监听若未提供合法非空 token，启动会自动生成、尝试写回配置，并在启动日志打印一次。成功回写后 `api.token` 是字面值，之后修改 `.env` 的 `ROS_DDNS_API_TOKEN` 不会覆盖它；希望保留环境变量管理时应在首次启动前设置，详见 [Docker 部署](docs/deployment.md#31-前提与准备)。

> `docker compose up -d --build` 不会自动读取 git tag，镜像默认显示 `dev` 版本；需要版本可追溯时，先按 [部署指南 §3.2](docs/deployment.md#32-构建校验与运行) 导出 `VERSION` / `COMMIT` / `BUILD_DATE` 再构建。

### 从源码运行

需要 Go 1.26.10 或更高版本（Go 1.21+ 会按 `go.mod` 的 `go` 要求自动下载匹配工具链）。

```bash
# 可选：启动前用环境变量覆盖默认配置模板中的值
export ROS_PASSWORD='路由器密码'
export ROS_DDNS_API_TOKEN='replace-with-your-random-ascii-token' # 替换为自行生成的随机值

go build -o ros-ddns ./cmd/ros-ddns
./ros-ddns -config config.yaml                     # 首次启动生成 config.yaml，前台运行
./ros-ddns -config config.yaml -check              # 生成后静态校验（可在另一终端）
```

Windows PowerShell：

```powershell
$env:ROS_PASSWORD = '路由器密码'
$env:ROS_DDNS_API_TOKEN = 'replace-with-your-random-ascii-token' # 替换为自行生成的随机值

.\scripts\go.cmd build -o build\bin\ros-ddns.exe ./cmd/ros-ddns
.\build\bin\ros-ddns.exe -config config.yaml        # 首次启动生成 config.yaml
.\build\bin\ros-ddns.exe -config config.yaml -check # 生成后静态校验
```

程序不会自动加载 `.env` 文件。配置文件不存在时，首次守护进程启动会生成内嵌默认配置并启动 WebUI，默认监听 `127.0.0.1:8080`。首次启动后打开 WebUI 添加 DNS 服务商、凭据和同步项目，也可以手工编辑生成的文件（建议 `0600` 权限）。

## WebUI

| 运行概览 | 同步项目 |
| :---: | :---: |
| ![运行概览](docs/images/webui-dashboard.png) | ![同步项目](docs/images/webui-items.png) |

启动守护进程后访问 `http://<api.listen>/`（默认 <http://127.0.0.1:8080/>）；设置 `api.token` 后需登录。配置保存后热重载，`api.listen`、`api.token`、`log.format` 除外（需重启）。更多说明见 [WebUI 使用指南](docs/webui.md)。

## 配置

默认配置仅在守护进程启动时发现配置文件缺失才生成，手动和服务方式相同（内嵌模板见 [internal/config/default.yaml](internal/config/default.yaml)）。模板保留 `log`、`runtime`、`state`、`api`、`routeros` 段，不含 `dns` 与 `items` 段；`dns.providers`、`items` 为空或 items 全部禁用都合法。`routeros`、`dns`、`items` 等非必须参数缺失或不完整也不阻止启动。默认 `routeros.tls.insecureSkipVerify: false`；自签名路由器需改为 `true` 或提供 `caFile`。完整字段说明见 [配置参考](docs/configuration.md)。

| 配置段 | 说明 |
| --- | --- |
| `log` | 必须参数组：日志级别与格式；缺失时使用默认值 |
| `runtime` | 必须参数组：轮询间隔、稳定窗口、校验周期等；缺失时使用默认值 |
| `state` | 必须参数组：缺失 / `null` 使用默认路径；显式 `path: ""` 是合法的仅内存模式 |
| `api` | 必须参数组：默认回环监听、token 可空；非回环启动时缺少合法 token 会自动生成并尝试回写 |
| `routeros` | 非必须参数组：严格校验在有启用项时要求 URL、用户名；无启用项时可全空，只校验已提供值的格式 |
| `dns` | 非必须参数组：provider 集合可为空；已定义 provider 的格式仍受严格校验，AK / Secret 可稍后填写 |
| `items` | 非必须参数组：可为空或全禁用；禁用草稿仅校验 name 非空、格式及跨全部项唯一，启用项完整校验 |

“必须参数”不表示必须写全 YAML 字段。启动时，指定必须参数的显式非法语义值会回退默认并尝试原子回写，先备份为 `config.yaml.bak`、保留原权限，记录 WARN 和 `config_autofix` 通知。环境占位符展开后的语义非法值通常只在内存回退；非回环自动生成 token 会替换原占位符。回写失败不阻止启动。YAML 语法、重复键、未知字段、类型 / 解码错误及非空非法 `api.listen` 仍致命，原文件不改名、不替换为默认配置。完整范围与例外见 [启动期自动修复与回写](docs/configuration.md#11-启动期自动修复与回写)。

启动容忍只用于守护进程：无效启用项在状态中显示 `phase: "error"` 和配置错误，不参与轮询或触发，有效项继续，`ready=false`；RouterOS 配置 / 客户端错误进入轮询退避，状态缓存打开失败退化为仅内存。WebUI 保存、PUT 和 `-check` 仍严格，保存校验或候选构造失败保留旧文件与服务。

配置字段支持 `${VAR}` / `${VAR:-默认值}` 环境变量引用（YAML 解析后展开），手工编写的配置同样支持。默认模板保留 `log`、`runtime`、`state`、`api`、`routeros` 中的环境变量引用，例如 `ROS_DDNS_LOG_LEVEL`、`ROS_DDNS_POLL_INTERVAL`、`ROS_DDNS_SYNC_ON_START`、`ROS_DDNS_STATE_PATH`、`ROS_DDNS_LISTEN`、`ROS_DDNS_API_TOKEN`、`ROS_DDNS_ROUTEROS_URL`、`ROS_PASSWORD` 等，完整清单见 [配置参考](docs/configuration.md#2-环境变量展开)。

## 命令行

| 参数 | 说明 |
| --- | --- |
| `-config` | 配置文件路径，默认 `config.yaml` |
| `-check` | 严格加载和校验，不修复、不生成文件、不回写、不获取锁；会报告启动时本可自动修复的非法值 |
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

安装、升级、回滚与 Docker 部署的完整步骤见 [生产部署指南](docs/deployment.md)。退出码：`0` 成功或正常停止；`1` 获取锁、生成默认配置、监听或运行失败，也用于服务方式的致命配置错误；`2` 参数错误、`-check` 校验失败或手动启动的致命配置错误。启动自动修复、回写失败和可容忍的同步错误不会单独导致退出。启动后可再次 `-check` 验证磁盘配置。

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

- API 默认只监听回环地址；对外监听建议预先设置合法非空 `api.token`（1–256 个可打印 ASCII 字符，不含空格 / 控制字符），并限制来源或通过 HTTPS 反向代理访问。未提供合法 token 时启动会自动生成、尝试回写并打印一次，服务方式也写入 OS 服务日志。
- 配置文件、凭据文件和状态文件建议设为 `0600` 并限制所在目录权限。常规日志会脱敏密码、AccessKey、STS token 等凭据；自动 token 的启动日志须限制访问。状态文件仍包含域名、IP、RecordId 等信息，不应公开。
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
