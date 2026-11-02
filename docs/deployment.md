# 生产部署与单实例运行

本页给出 OS 服务管理与 Docker Compose 的运行基线。程序仍是内嵌 WebUI 的单个 Go 二进制；配置字段见 [配置参考](configuration.md)，设备和云账号准备见 [RouterOS](routeros-setup.md) 与 [阿里云](aliyun-setup.md) 指南。以下命令供部署服务器执行，不代表已经完成服务器、容器或真实 DNS 写入验收。

## 1. 单实例锁

两个进程同时对账可能交错修改相同 DNS / address-list，并覆盖状态缓存。守护进程会在加载配置、设置日志之后，创建任何服务或 HTTP 监听之前取得单实例锁。`-version` 不加载配置；`-check` 只加载、校验并输出摘要，不获取锁，也不验证锁或状态目录可写性。

锁路径按以下顺序选择：

1. 非空 CLI 参数 `-lock-file <路径>`；相对路径按进程工作目录解析。
2. 否则使用启动配置的 `state.path + ".lock"`；例如默认 `./data/state.json.lock`。
3. 若 `state.path` 为空，使用 `filepath.Join(os.TempDir(), "ros-ddns-" + hex(SHA256(规范配置路径)的前8字节) + ".lock")`，即 16 个小写十六进制字符。配置路径转为绝对路径并解析符号链接，Windows 统一大小写，Unix 保留大小写；相对 / 绝对写法和符号链接别名使用同一锁。不按配置内容散列；临时目录取决于进程环境。

配置文件、`配置路径 + ".bak"`、非空 `state.path` 和有效锁路径必须互不重合。启动及 `-check` 在任何写入或锁文件截断之前检查文件身份，拒绝相对路径、符号链接、现有硬链接以及 Windows 大小写别名造成的冲突，返回退出码 `2`；尚不存在的目标会解析已有父目录，不创建文件。WebUI 校验及保存也检查当前锁路径，冲突保存返回 `422`。该检查不防止外部进程在检查后更换链接或文件，应避免并发修改这些路径。

没有 YAML 锁字段，空的 `-lock-file` 表示使用默认规则，不能关闭互斥。Windows 使用 `LockFileEx` 的非阻塞独占锁，Unix 使用 `flock(LOCK_EX | LOCK_NB)`，不是靠 PID 或文件是否存在判断。Windows 锁定 EOF 之外的一个字节，使诊断 JSON 仍可被其他进程读取；不会因此扩展文件。

取得 OS 锁后才截断、写入并 sync JSON，包含 `pid`、`startedAt`、`configPath`、`version`，不含凭据。缺失的父目录以 `0700` 创建，文件创建并收紧为 `0600`；Windows 的实际访问控制仍需配置 ACL。冲突立即返回退出码 `1`，stderr 包含锁诊断，可读时包含持有者 PID、启动时间、配置路径；不创建服务或监听端口。其他获取错误（例如权限不足）也返回 `1`。

正常退出释放 OS 锁并关闭文件；异常退出由操作系统释放。**锁文件保留，不能用文件存在或旧 PID 判断仍在运行，也不要在实例运行时删除或替换锁文件。** Unix 删除会让后来者锁住另一个 inode，破坏互斥。再次成功获取会覆盖旧诊断；持有者刚获取或意外退出时，JSON 也可能不完整。

热重载 `state.path` **不会移动已经持有的锁**。服务安装记录的启动参数与 Docker 镜像的启动命令都显式传入 `-lock-file`，因此运行中的互斥路径不会随 `state.path` 改变；仍建议停服后修改状态路径再启动，便于核对磁盘状态。任何需要与守护进程互斥的外部操作都必须传入同一 `-lock-file`。服务管理器的独立临时目录、容器独立临时目录、不同配置路径或不同主机不会自动共享临时锁；生产部署应使用下面的持久锁路径。

| 部署 | 配置 | 状态 | 固定锁 |
| --- | --- | --- | --- |
| 服务安装（systemd / Windows 服务） | Linux `/var/lib/ros-ddns/config.yaml`；Windows 如 `C:\ProgramData\ros-ddns\config.yaml` | 配置文件同目录的 `state.json` | `-lock-file`（安装时显式传入，推荐） |
| Docker | `/data/config.yaml` | `/data/state.json` | `/data/state.json.lock` |

这是同一文件上的协作互斥，不是分布式锁、远端目标锁或 HA 协调。使用支持这些 OS 锁语义的本地文件系统；网络文件系统需单独验证。不要通过换锁路径来并发操作同一组目标。确需独立部署时，必须同时隔离目标、配置、状态、锁和监听端口，并自行确认不会管理相同资源。

## 2. 原生服务管理（systemd / Windows 服务）

二进制自带服务管理（`github.com/kardianos/service`）：Linux 上生成 systemd unit（也支持 upstart / sysv / openrc 回退，未验证），Windows 上注册 SCM 服务。安装与控制动作需要 root / 管理员权限；仓库不再提供 unit 文件和安装脚本。

### 2.1 前提与构建

目标为 Linux（systemd）或 Windows。运行主机需可信 CA 证书、到 RouterOS / 阿里云的网络，以及正确的系统时间。编译需 Go 1.26.2 或更高；已有匹配架构的二进制则无需在服务器安装 Go。

```bash
# 在源码根目录，Linux/CI；生成静态 Linux amd64 二进制
make build-linux VERSION=v0.1.0 COMMIT=local BUILD_DATE=2026-10-31T00:00:00Z
# 没有 make 时的等价构建（使用默认构建元数据）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -o build/bin/ros-ddns-linux-amd64 ./cmd/ros-ddns
```

ARM64 主机应改为 `GOARCH=arm64` 并使用相应输出名。Windows 可直接设置这些 Go 环境变量后交叉编译；不要求 Windows 安装 make。

### 2.2 准备账号、目录、二进制与配置

Linux 示例（无 systemd 建账号需求时可使用现有服务账号；`ros-ddns` 为推荐名）：

```bash
sudo groupadd --system ros-ddns 2>/dev/null || true
sudo useradd --system --gid ros-ddns --home-dir /var/lib/ros-ddns \
  --no-create-home --shell /usr/sbin/nologin ros-ddns 2>/dev/null || true
sudo install -d -o ros-ddns -g ros-ddns -m 0700 /var/lib/ros-ddns
sudo install -d -o root -g root -m 0755 /usr/local/bin

# 用临时文件 + rename 替换，避免截断正在运行的二进制
tmp=$(mktemp /usr/local/bin/.ros-ddns.install.XXXXXX)
sudo install -o root -g root -m 0755 build/bin/ros-ddns-linux-amd64 "$tmp"
sudo mv -f "$tmp" /usr/local/bin/ros-ddns
```

配置文件是可选的：可以直接留空，守护进程启动（手动或服务方式）会自动生成默认配置；也可以先复制示例并编辑：

```bash
sudo install -o ros-ddns -g ros-ddns -m 0600 \
  configs/config.example.yaml /var/lib/ros-ddns/config.yaml
```

编辑 `routeros`、`dns.providers`、`items`。服务方式运行**不继承 shell 环境**，因此依赖环境变量的字段（如 `${ROS_PASSWORD}`、`${ALI_ACCESS_KEY_ID}`）应改为字面值，或由服务管理器显式提供环境；否则服务方式启动时会把无法加载的配置保留为 `.invalid-<时间戳>` 并生成默认配置；手动运行则直接报错退出 `2`，不修改原文件。凭据可以留空或填写错误值：守护进程仍会启动，同步失败会记录日志与页内通知并退避重试。

服务模式下（Linux 与 Windows 相同）**相对路径以配置文件所在目录为基准**：进程启动时会切换到该目录，`state.path`、`tls.caFile`、`.bak` 都相对它解析。推荐直接使用绝对路径，或保持示例中的 `./data/state.json` / 默认配置中的 `state.json`。

Windows 示例：以管理员身份把二进制放到稳定路径（如 `C:\ProgramData\ros-ddns\ros-ddns.exe`），配置放在同一目录或单独目录；服务默认以 LocalSystem 运行，需确保该账号可读写配置目录（用 `icacls` 收紧到 `SYSTEM` / `Administrators`）。环境变量同样不会从管理员 shell 继承。

### 2.3 安装、启动与配置

```bash
sudo /usr/local/bin/ros-ddns -service install \
  -config /var/lib/ros-ddns/config.yaml \
  -lock-file /var/lib/ros-ddns/state.json.lock \
  -service-user ros-ddns
sudo /usr/local/bin/ros-ddns -service status
sudo /usr/local/bin/ros-ddns -service start
```

- `-service install` 只注册并启用服务（systemd `enable` / Windows 自动启动），**不启动服务、不读取配置**；配置缺失时打印 warning 与后续命令。`-config` 会被记录为绝对路径。
- `-service status` 输出 `platform=... name=ros-ddns status=running|stopped|not-installed`；running 返回 `0`，其余返回 `1`。
- 守护进程启动（含 `-service start` 与手动运行）的开箱即用行为：
  - 配置文件不存在 → 生成内嵌默认配置（`0600`，父目录缺失时以 `0700` 创建），写日志与页内通知。默认 `api.listen: 127.0.0.1:8080`、token 为空，打开 WebUI 即可继续配置。
  - 配置文件存在但无法加载（YAML / 结构错误、环境引用未展开等）→ 服务方式：原文件改名为 `config.yaml.invalid-<时间戳>` 保留，写入默认配置后继续启动，Error 日志与通知中心给出两个路径；手动运行：报错退出 `2`，原文件保持不变。
  - 凭据缺失或错误、RouterOS / 阿里云不可达 → 服务照常运行；相关条目进入 degraded，日志与通知中心记录错误并退避重试，修复配置后自动恢复。
  - 默认配置含一个占位条目（`pppoe-out1` / `example.com`），在完成配置前会周期性报错，这是预期行为；可在 WebUI 中替换或删除。
- 校验配置可交互执行（`-check` 不加载服务环境，含 `${VAR}` 的字段需自行导出）：

```bash
sudo -u ros-ddns /usr/local/bin/ros-ddns -config /var/lib/ros-ddns/config.yaml -check
curl -fsS http://127.0.0.1:8080/healthz
curl -sS -i http://127.0.0.1:8080/readyz
```

- WebUI 保存会热重载可热重载字段并写入同一配置文件（`.bak` 备份）；`api.listen`、`api.token`、`log.format` 修改需要 `sudo /usr/local/bin/ros-ddns -service restart`（WebUI 会返回 `422` 提示重启）。
- 日志：systemd 用 `sudo journalctl -u ros-ddns.service -f`；Windows 在“事件查看器 → Windows 日志 → 应用程序”中查看服务生命周期错误，运行期日志见 WebUI“日志”页与页内通知。

### 2.4 升级、重启与回滚

先备份配置、`.bak`、状态与 CA 文件，并保留旧二进制：

```bash
sudo cp -p /usr/local/bin/ros-ddns /usr/local/bin/ros-ddns.previous
tmp=$(mktemp /usr/local/bin/.ros-ddns.install.XXXXXX)
sudo install -o root -g root -m 0755 build/bin/ros-ddns-linux-amd64 "$tmp"
sudo mv -f "$tmp" /usr/local/bin/ros-ddns
sudo /usr/local/bin/ros-ddns -service restart
sudo /usr/local/bin/ros-ddns -service status
```

确认版本、日志、探针和实际目标后再清理旧版本。unit 由库按安装时的参数生成，升级二进制不需要改 unit；若使用了 2.6 的加固 drop-in，需要自行保留。回滚时恢复旧二进制并 `-service restart`，必要时恢复兼容的配置 / 状态。单个 `.bak` 是最近一次 WebUI 保存备份，不是完整的发布回滚历史；本地回滚不会撤销已经发生的远端 DNS / 列表修改。

### 2.5 卸载

```bash
sudo /usr/local/bin/ros-ddns -service uninstall
```

`uninstall` 会先停止运行中的服务，再注销并移除 unit / Windows 事件源，但保留配置文件、状态与用户账号。确认备份完成后，可手动删除 `/usr/local/bin/ros-ddns`、`/var/lib/ros-ddns`、可选加固 drop-in 以及 `ros-ddns` 用户 / 组；Windows 删除二进制与数据目录。删除本地文件不会删除远端 DNS 或清理 RouterOS 列表。

### 2.6 可选 systemd 加固

库生成的默认 unit 只含启动所需字段。需要旧安装脚本那套加固时，创建 drop-in 即可（不要直接编辑生成的文件）：

```bash
sudo mkdir -p /etc/systemd/system/ros-ddns.service.d
sudo tee /etc/systemd/system/ros-ddns.service.d/hardening.conf >/dev/null <<'EOF'
[Service]
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictRealtime=true
CapabilityBoundingSet=
EOF
sudo systemctl daemon-reload
sudo /usr/local/bin/ros-ddns -service restart
```

`ProtectSystem=strict` 会使配置目录之外不可写：保持 `WorkingDirectory` 为配置目录（安装时会写入），并把状态 / CA 文件放在该目录内。卸载服务不会删除 drop-in 目录，需要手动清理。

## 3. Docker Compose

### 3.1 前提与准备

目标为支持 Linux 容器的 Docker Engine / Docker Desktop 与 Compose v2。首次构建需能拉取构建镜像和依赖；`Dockerfile` 使用 `golang:1.26-bookworm` 多阶段静态编译，运行镜像为带 CA 与时区数据的 `alpine:3.22`，只复制二进制。构建元数据可由 `VERSION`、`COMMIT`、`BUILD_DATE` build args 注入，也可执行 `make docker-build`。构建上下文排除 `.env`、`.secrets`、本地配置、缓存与数据目录，凭据不应通过构建参数传入。

以下为 Linux 宿主机首次准备命令；已有文件时保留并编辑，不要重新复制覆盖：

```bash
mkdir -p data
cp deploy/docker/config.example.yaml data/config.yaml
cp .env.example .env
chmod 600 .env
# 编辑 .env 与 data/config.yaml，替换占位凭据、URL、接口、域名等
sudo chown 10001:10001 data data/config.yaml
sudo chmod 700 data
sudo chmod 600 data/config.yaml
```

容器固定以 `10001:10001` 运行，绑定目录及现有状态 / `.bak` / CA 文件也必须允许该身份访问。宿主机普通用户可能需要 sudo 编辑 `0700` 目录。Docker Desktop 的 bind mount 权限行为取决于文件共享后端，应验证容器内可写并用宿主机 ACL 限制读取。SELinux 主机如遇挂载拒绝，需要管理员为该私有数据挂载配置适当标签。

程序本身不读取 `.env`，Compose 的 `env_file` 将其注入进程。不要打印或分享展开后的 `docker compose config` 输出，它可能包含凭据；只校验语法可用 `docker compose config --quiet`。env 示例的单引号可防止 Compose 插值普通 `$` 等字符；含引号等特殊内容时按 Compose env 文件语法处理。更新凭据后需要重新创建容器，`docker compose restart` 不会重新注入 env。

容器配置使用 `api.listen: 0.0.0.0:8080`，让发布端口可达；由于这不是回环监听，**必须设置非空 API token**，即使宿主机仅发布到 `127.0.0.1` 也一样。默认宿主机绑定 `127.0.0.1:8080:8080`；端口被占用时在 `.env` 中设置 `ROS_DDNS_HTTP_BIND=127.0.0.1:18082` 等值，容器内仍监听 8080。远程访问通过受控 TLS 反向代理；代理须保留正确 Host / 转发协议语义。不要直接把无 TLS 的管理接口公开到互联网。健康探针公开，不需要 token。

### 3.2 构建、校验与运行

```bash
docker compose config --quiet
docker compose build
# 静态校验不获取锁，不访问 RouterOS / 阿里云
docker compose run --rm --no-deps ros-ddns -config /data/config.yaml -check
docker compose up -d
docker compose ps
docker compose logs --tail=100 -f ros-ddns
curl -fsS http://${ROS_DDNS_HTTP_BIND:-127.0.0.1:8080}/healthz
curl -sS -i http://${ROS_DDNS_HTTP_BIND:-127.0.0.1:8080}/readyz
```

Compose 每 30 秒通过容器内 `wget` 请求 `healthz`，超时 5 秒，启动宽限 15 秒，连续 3 次失败标记 unhealthy。Docker 的 `unless-stopped` 处理进程退出，**不会仅因 unhealthy 自动重启**；需由运维监控处理。healthcheck 只证明 HTTP 存活，不证明实际 DNS 写入。容器内程序按前台模式运行：同样会在配置缺失时生成默认配置，但默认 `api.listen` 为 `127.0.0.1:8080`；如果要通过发布端口访问 WebUI，仍需先准备 `data/config.yaml`（`0.0.0.0:8080` + 非空 token）。

运行时根文件系统只读，去除所有 capabilities，并启用 no-new-privileges；`/tmp` 使用 tmpfs，`/data` 是可写持久挂载。状态固定为 `/data/state.json`，锁由 Dockerfile 的 `CMD` 固定为 `/data/state.json.lock`（不随 state.path 热重载移动）。不要只绑定单个 `config.yaml` 文件或把 `/data` 挂成只读，否则 WebUI 的临时文件、rename 和 `.bak` 保存无法正常工作。热重载状态路径也应留在可写目录内。

`docker compose stop` / `down` 发送停止信号并给予 20 秒退出时间。锁文件留在宿主机 data 中是正常现象；停止后可直接重启，不用删除锁。不要与宿主机服务或另一个容器同时管理相同目标。容器内程序使用同一单实例锁；静态校验可执行 `docker compose run --rm --no-deps ros-ddns -config /data/config.yaml -check`，对账由运行中的容器完成。

### 3.3 WebUI、更新与清理

WebUI 地址是 `http://127.0.0.1:8080/`。配置保存更新宿主机 `data/config.yaml`，备份为 `data/config.yaml.bak`；环境引用仍保留，但注释和排版可能被规范化。环境变化、API 地址 / token、日志格式修改需重新创建容器；其他支持字段可由 WebUI 热重载。手动编辑磁盘配置后也需重启 / 重建才能加载。

升级前备份 data 和 `.env` 到权限受限的位置，并保留旧镜像：

```bash
docker image tag ros-ddns:local ros-ddns:previous
docker compose build --pull
docker compose up -d --force-recreate
docker compose logs --tail=100 ros-ddns
```

检查版本、探针、WebUI 与目标结果。需回滚时先 `docker compose down`，将 `ros-ddns:previous` 重新标记为 `ros-ddns:local`，恢复必要的兼容配置 / 状态，再 `docker compose up -d --no-build`；回滚过程中不要重建覆盖旧镜像。不要用 `.bak` 代替独立的发布备份。

正常下线使用 `docker compose down`，它保留 bind mount 的 data 与 `.env`；即使 `down -v` 也不会清理这个宿主机目录。确认所有进程已停止、备份完成且无需恢复后，再手动删除 data、`.env` 和不再使用的镜像。注意本地数据清理不会撤销远端变更。

## 4. 安全与验收清单

配置、`.bak`、状态和锁即使没有明文凭据，也可能暴露域名、IP、接口、RecordId 和路径。配置中的凭据使用文件权限保护（服务方式 `0600`、目录 `0700`），WebUI 与 API 只对已认证客户端可见；服务模式下不再有独立的 root-only env 文件，凭据直接位于配置文件中，因此配置文件的权限即凭据权限。环境变量（交互模式或容器注入）仍可被宿主机管理员读取，不是抵抗管理员的秘密存储。不要公开日志、配置 API 响应或未经检查的诊断包。

- 静态配置校验通过；域名、接口、CA、token 和目录权限均已核对。
- 守护进程启动（手动或服务方式）在配置缺失时生成默认配置；服务方式下无法加载的配置会保留为 `.invalid-<时间戳>` 后生成默认配置。WebUI 可打开；凭据缺失或错误时进程保持运行、日志与通知持续报错并重试；修复后热重载恢复。
- 启动日志显示取得预期锁；第二个使用同一锁的实例立即失败且不发起同步；持锁时 `-check` 仍成功。
- `healthz` 为 200，`readyz` 最终为 200；同时确认项目 phase / lastError，不能把探针当作 DNS 写入验收。
- WebUI 能读取、保存允许热重载的配置，磁盘配置及 `.bak` 所有者 / 权限正确；env 引用保留。
- 正常停止后可再启动，历史锁文件无需删除；更改状态路径时安排完整停启。
- 在服务器分别验证服务安装与可选加固 drop-in、Docker bind mount / healthcheck、日志和 20 秒停止窗口。
- 在专用测试目标验证 RouterOS 与 DNS 读写、续期、故障恢复、升级及回滚。`-check` 只做静态校验，不验证阿里云权限或真实 DNS 写入，本地单元测试也不替代这一步。
