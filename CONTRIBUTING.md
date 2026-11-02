# 贡献指南

本文件面向在本仓库开发、构建、测试和验收的贡献者，整理本地约定、验证要求与历史验收记录。用户使用说明见 [README](README.md)，部署细节见 [生产部署指南](docs/deployment.md)。

## 开发环境

- Go 1.26.2 或更高版本，见 [go.mod](go.mod)。
- Linux / macOS 直接使用 `go`；Windows 使用 `scripts\go.cmd`，它在仓库内维护 Go 缓存（`build/.go-cache`）。
- 可选：GNU Make（Linux / CI）、Docker 与 Docker Compose v2。

## 构建与测试

```bash
make build        # 构建当前平台二进制
make build-linux  # 静态交叉编译 Linux amd64
make test         # 单元测试
make vet          # 静态检查
make fmt          # gofmt 格式化（会改写文件）
make check        # fmt + vet + test
make docker-build # 构建容器镜像
```

Windows 下没有 make 时：

```powershell
.\scripts\go.cmd build -o build\bin\ros-ddns.exe ./cmd/ros-ddns
.\scripts\go.cmd test ./...
.\scripts\go.cmd vet ./...
```

提交前至少运行 `make test vet`（或等效命令）并确认 `gofmt` 通过。注意 `make check` 会写入格式化结果，而程序自身的 `-check` 只校验配置，两者不同。

## 本地目录约定（build/）

源码区只保留源码、文档和项目元数据；构建产物、Go 缓存、本地配置、运行状态、日志、凭据与调试文件统一放在 `build/`（已被 `.gitignore` 与 `.dockerignore` 忽略，不要提交）：

| 路径 | 内容 | 可删除性 |
| --- | --- | --- |
| `build/bin/` | 构建产物（`ros-ddns.exe`、Linux 交叉编译） | 可删，重新构建即可 |
| `build/.go-cache/` | Go 缓存（`GOPATH` / `GOMODCACHE` / `GOCACHE`） | 可删，下次构建变慢 |
| `build/configs/` | 本地配置 `config.local.yaml` 及 WebUI 生成的 `.bak` | 需保留或自行备份 |
| `build/data/` | 本地实例状态缓存与单实例锁 | 需保留（含 RecordId 恢复线索） |
| `build/logs/` | 守护进程运行日志 | 可删 |
| `build/secrets/` | 本机凭据 `ros-ddns.env` | **不可删**，保持仅本用户可读 |
| `build/tmp/` | 一次性调试产物 | 可随时整体删除 |

`make clean` 只删除 `build/bin`，`make clean-tmp` 删除 `build/tmp` 与 `build/logs`，`make clean-cache` 删除 Go 缓存；三者都不会触碰配置、状态与凭据。手动开发可用 `scripts/dev.cmd`（Windows，内部以 `-ExecutionPolicy Bypass` 调用 `scripts/dev.ps1`）：从 `build/secrets/ros-ddns.env` 注入环境变量，支持 `-Check` / 后台守护两种模式，日志写入 `build/logs/`，`-Stop` 停止已启动实例。

## 验证与验收

- `-check` 只加载并校验配置：不获取单实例锁、不连接 RouterOS 或阿里云、不读取 CA 文件内容，也不创建或修改配置文件。
- 本地单元测试与 `-check` 不能替代真实环境验收。真实 RouterOS / 阿里云写入必须在自己的环境执行，按 [RouterOS 指南 §8](docs/routeros-setup.md#8-手工端到端验证清单) 与 [阿里云指南 §9](docs/aliyun-setup.md#9-手工端到端验证清单) 逐项确认。
- 部署侧验收清单见 [生产部署指南 §4](docs/deployment.md#4-安全与验收清单)。
- `-once` 与 `-dry-run` 模式已移除；现行版本执行真实对账，单次同步请使用 WebUI / API 的「立即同步」。
- 文档中的操作清单不代表已完成写入验收；验收结论以实际执行记录为准。

## 提交约定

- 一个提交只做一件事，提交信息说明动机与影响（如 `feat:` / `fix:` / `docs:` / `chore:`）。
- 不要提交 `build/`、凭据、日志、状态文件或本机路径。
- 行为变更需同步文档：配置字段更新 [配置参考](docs/configuration.md) 与 [示例配置](configs/config.example.yaml)；用户可见行为更新 [README](README.md) 或对应 docs。

## 历史验收记录

以下记录来自项目早期的内部验收，用于追溯当时的验证范围；其中的观测值不是部署默认值，公开仓库中已隐去主机名与网络标识。

### 部署验收（2026-10-31）

环境：内部测试主机，Debian 13 / systemd 257 / Docker 29.1.5 / Docker Compose v5.0.2；使用 Linux amd64 静态二进制 `v0.1.0-p0verify` 和同源码构建的 `ros-ddns:local` 镜像。

本次验收发生在 dry-run 模式移除之前（当时所有运行均带 `-dry-run`，仅读取 RouterOS，未写 DNS 或 address-list），因此只验收部署、锁、权限、探针和配置保存，不构成真实写入验收。下表中 systemd 相关行记录的是已移除的旧安装脚本方案（仓库不再包含该脚本），仅作历史参考；新的 `-service` 方案尚未在 Linux 服务器上验收。

| 项目 | 结果 |
| --- | --- |
| 旧方案 systemd 安装（历史） | 安装器创建用户/目录/unit 并 enable；二进制 `0755 root:root`，配置 `0600 ros-ddns:ros-ddns`，env `0600 root:root`，目录 `0700/0750` |
| 旧方案 systemd 运行（历史） | 服务以 `ros-ddns`（UID 999）运行；unit 含显式 `-lock-file`；`healthz` 200、`readyz` 200 |
| 单实例 | 第二进程退出 `1`，报告持有者 PID；持锁时 `-check` 返回 `0` 且不连接远端 |
| 锁与恢复 | 锁文件 `0600`；SIGKILL 后旧方案 systemd 自动重启并取得新锁；`systemctl restart` 正常；停止后锁文件保留、可再次启动 |
| 配置保存 | `GET /api/v1/config` 的 token 为掩码；原样 `PUT` 返回 `200`，磁盘配置与 `.bak` 均保持 `0600`，服务保持 active |
| Docker 构建 | 多阶段构建成功；镜像内版本为 `v0.1.0-p0verify`；Compose `config --quiet` 通过 |
| Docker 运行 | 以 UID/GID 10001 非 root 运行，rootfs 只读，`cap_drop: ALL`；healthcheck 为 healthy；`healthz/readyz` 200 |
| Docker 锁与保存 | 同 `/data` 的第二个容器退出 `1`（锁冲突）；WebUI 保存写入宿主机 `data/config.yaml` 和 `.bak`，权限保持 `0600` |
| 停机与清理 | `docker compose stop` 正常退出 `0`，锁文件保留，重启后探针恢复；验收后已删除 unit、用户/组、安装目录、二进制、容器、网络、镜像、临时源码和 root-only env，确认无进程、无监听端口、无残留目录/镜像 |

本次使用临时 root-only env 注入测试凭据，验证后已删除；日志与 Compose 日志中未匹配到凭据值。Docker 的异常退出自动重启未单独模拟，正常 stop/start 与旧方案 systemd SIGKILL 恢复已验证。共享 Docker 构建缓存未清理；如需彻底回收磁盘，请由管理员评估后执行 `docker builder prune`。

### WebUI 验收（dry-run 时期）

以下是 dry-run 模式移除前完成的真实 RouterOS Playwright 验收记录（该模式已移除，截图 header 可能仍显示 Dry run 徽标；真实地址与 MAC 已隐去）：

1. 从真实 `lan1` 读取 IPv6，Dashboard 正常显示来源地址，并由接口 MAC 派生出对应的 EUI-64 `recordValue`。
2. 将来源设为不存在的接口，出现无合格地址的错误状态；通过 Config **表单**修正回 `lan1`，保存并应用，完成热重载。
3. 重新读取到真实源地址后，项目恢复到无错误状态（当时为 dry-run 模式），并收到恢复通知 `item_recovery`；配置应用事件为 `config_applied`。
4. 当时日志可见 `dry-run: would ensure DNS record` 和 `dry-run: would add address-list entry` 计划；该次验收没有写 DNS 或地址列表。表单保存仍会写配置文件与 `.bak`。

这次验收证明了真实源读取、EUI-64 展示、表单修复、热重载和页内恢复通知链路；不验证阿里云写权限、DNS 解析传播或 RouterOS 列表写权限，真实写入检查另见 [阿里云指南](docs/aliyun-setup.md) 与 [RouterOS 指南](docs/routeros-setup.md)。
