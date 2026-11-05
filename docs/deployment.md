# 生产部署与单实例运行

本页给出 OS 服务管理与 Docker Compose 的运行基线。程序仍是内嵌 WebUI 的单个 Go 二进制；配置字段见 [配置参考](configuration.md)，设备和云账号准备见 [RouterOS](routeros-setup.md) 与 [阿里云](aliyun-setup.md) 指南。以下命令供部署服务器执行，不代表已经完成服务器、容器或真实 DNS 写入验收。

## 1. 单实例锁

两个进程同时对账可能交错修改相同 DNS / address-list，并覆盖状态缓存。守护进程先只读探测配置、计算启动修正后的生效值并检查配置、备份、状态和候选锁的文件身份，再取得单实例锁；缺失文件的默认配置生成、启动修正回写及后续保存都在持锁期间执行。锁内重新读取并处理配置、复核路径与锁，再创建同步服务和 HTTP 监听。已有文件的 YAML / 类型解码等错误会使启动失败，原文件保持不变。`-version` 不加载配置；`-check` 严格加载、校验并输出摘要，不修复、不获取锁、不生成或修改文件，也不读取 CA 内容或验证锁、状态目录可写性。

锁路径按以下顺序选择：

1. 非空 CLI 参数 `-lock-file <路径>`；相对路径按进程工作目录解析。
2. 否则使用启动修正后生效的 `state.path + ".lock"`；例如默认 `./data/state.json.lock`。
3. 若显式 `state.path: ""`，使用 `filepath.Join(os.TempDir(), "ros-ddns-" + hex(SHA256(规范配置路径)的前8字节) + ".lock")`，即 16 个小写十六进制字符。空字符串是合法的仅内存模式，不触发修复；缺失或 `null` 则使用默认状态路径。配置路径转为绝对路径并解析符号链接，Windows 统一大小写，Unix 保留大小写；相对 / 绝对写法和符号链接别名使用同一锁。不按配置内容散列；临时目录取决于进程环境。

仅配置文件缺失时，未指定 `-lock-file` 的启动先使用配置目录下的 `state.json.lock` 作为候选锁。生成默认配置后，若有效锁路径与已持有的锁不同（例如 `ROS_DDNS_STATE_PATH` 覆盖了默认状态路径），本次启动报错并退出，不换锁重试，也不再次改写配置；应使用 `-lock-file` 固定所有实例共同使用的锁路径后再启动。已有文件无法解析或解码时直接失败，不使用默认文件继续运行。

配置文件、`配置路径 + ".bak"`、非空 `state.path` 和有效锁路径必须互不重合。启动及 `-check` 在任何写入或锁文件截断之前检查文件身份，包括相对路径、符号链接、现有硬链接以及 Windows 大小写别名。守护进程对非空、无法解析或与配置 / `.bak` 冲突的 state.path 先按 [启动修复规则](configuration.md#11-启动期自动修复与回写) 回退为 `./data/state.json`，再检查生效路径；剩余冲突致命，手动退出 `2` / 服务退出 `1`。`-check` 不修复，冲突直接返回 `2`。尚不存在的目标会解析已有父目录，不创建文件。WebUI 校验及保存也检查当前锁路径，冲突保存返回 `422`。该检查不防止外部进程在检查后更换链接或文件，应避免并发修改这些路径。

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

目标为 Linux（systemd）或 Windows。运行主机需可信 CA 证书、到 RouterOS / 阿里云的网络，以及正确的系统时间。编译需 Go 1.26.10 或更高；已有匹配架构的二进制则无需在服务器安装 Go。

```bash
# 在源码根目录，Linux/CI；生成静态 Linux amd64 二进制。
# 未显式指定时，make 目标从当前 git 检出推导 VERSION（tag 存在时为 tag，
# 如 v0.1.0）、短 COMMIT 与提交时间；也可覆盖：make build-linux VERSION=v0.1.0
make build-linux
# 没有 make 时的等价构建；go build 不会自动注入版本，需自行传 -ldflags
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-X ros-ddns/internal/version.Version=$(git describe --tags --always --dirty) \
    -X ros-ddns/internal/version.Commit=$(git rev-parse --short HEAD) \
    -X ros-ddns/internal/version.BuildDate=$(git log -1 --format=%cI)" \
  -o build/bin/ros-ddns-linux-amd64 ./cmd/ros-ddns
```

ARM64 主机应改为 `GOARCH=arm64` 并使用相应输出名。Windows 可直接设置这些 Go 环境变量后交叉编译；不要求 Windows 安装 make。

### 2.2 准备账号、目录、二进制与配置

Linux 示例（无 systemd 建账号需求时可使用现有服务账号；`ros-ddns` 为推荐名）。命令块在子 shell 中运行，任一步失败即停止：

```bash
(
set -e
getent group ros-ddns >/dev/null || sudo groupadd --system ros-ddns
id -u ros-ddns >/dev/null 2>&1 || sudo useradd --system --gid ros-ddns --home-dir /var/lib/ros-ddns \
  --no-create-home --shell /usr/sbin/nologin ros-ddns
sudo install -d -o ros-ddns -g ros-ddns -m 0700 /var/lib/ros-ddns
sudo install -d -o root -g root -m 0755 /usr/local/bin

# 用临时文件 + rename 替换，避免截断正在运行的二进制
tmp=$(sudo mktemp /usr/local/bin/.ros-ddns.install.XXXXXX)
sudo install -o root -g root -m 0755 build/bin/ros-ddns-linux-amd64 "$tmp"
sudo mv -f "$tmp" /usr/local/bin/ros-ddns
)
```

配置文件可在启动前省略：守护进程启动（手动或服务方式）发现文件缺失时，会生成内嵌默认配置（`0600`，缺失父目录 `0700`）。模板保留 `log`、`runtime`、`state`、`api`、`routeros` 段及其中的 `${VAR:-默认值}` 环境引用，不含 `dns` 与 `items` 段。启动后在 WebUI 添加 DNS 服务商、凭据与同步项目，或手工补充配置。以下为手动运行前导出变量的示例：

```bash
export ROS_PASSWORD='...'
export ROS_DDNS_API_TOKEN='replace-with-your-random-ascii-token' # 替换为自行生成的随机值
# 可选：ROS_DDNS_ROUTEROS_URL、ROS_DDNS_ROUTEROS_USERNAME、ROS_DDNS_LISTEN、ROS_DDNS_CA_FILE
```

服务方式运行**不继承操作终端的 shell 环境**：没有显式提供环境时，生成配置中的引用会展开为模板默认值（空 RouterOS 密码、回环监听），之后可编辑生成文件或用 WebUI 配置。若配置里写了无默认值的 `${VAR}` 且变量未设置，手动启动退出 `2`、服务方式退出 `1`，两者都保持原文件不变。必须参数从占位符成功展开并解码后语义非法时，通常仅在内存回退，日志注明“来自环境变量占位符，保留原文件”；非回环 token 自动生成是回写例外。没有启用项时 routeros 可全空，`dns.providers`、`items` 可为空；有效运行项的凭据缺失或错误时，同步失败记录日志与页内通知并退避重试。

服务模式下（Linux 与 Windows 相同）**相对路径以配置文件所在目录为基准**：进程启动时会切换到该目录，`state.path`、`tls.caFile`、`.bak` 都相对它解析。推荐直接使用绝对路径；默认配置生成的 `state.path` 是配置目录下的绝对路径。

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

守护进程启动（手动与 OS 服务方式相同）按以下规则处理：

| 情况 | 行为 |
| --- | --- |
| 配置文件不存在 | 生成内嵌默认配置（文件 `0600`，缺失父目录 `0700`），写日志与页内通知；默认回环监听、token 为空，环境引用可覆盖 |
| 已有文件的 YAML 语法、多文档、重复键、未知字段、环境展开或任何类型 / 解码错误 | 致命退出：手动 `2` / 服务 `1`；原文件不改名、不替换，非法 duration 或非整数 TTL 也属于此类 |
| 指定必须参数的显式语义非法值 | 回退内置默认值并尝试回写，先备份 `config.yaml.bak`、保留原权限后原子替换；WARN 与 warning 级 `config_autofix` 提示结果 |
| 必须参数来自环境占位符且展开、解码后语义非法 | 通常仅内存回退，不改占位符；仅有这类修正时不创建 `.bak`；缺失 / `null` 通常直接使用默认值，不回写 |
| 非回环监听且 token 为空或非法 | 用 `crypto/rand` 生成 32 字节随机数的 hex token，生效并尝试写回字面值，即使原字段是占位符；启动日志打印一次，服务方式也写 OS 服务日志 |
| 回写失败 | 继续使用内存修正值，WARN 与通知提示“回写失败 / 未回写”；未持久化的 token 下次启动可能变化 |
| 无效启用项或 provider 构造失败 | 相关项在 status 中为 error，不轮询或触发；其他有效项继续，ready 为 false |
| RouterOS 配置不全 / 客户端构造失败 | 进程继续，相关项进入源轮询退避；每轮失败 WARN、页内通知报告错误，未成功读取前 ready 为 false |
| 状态缓存打开失败 | WARN 并退化为仅内存缓存，继续启动 |

必须参数为 `log`、`runtime`、`state`、`api`；routeros、dns、items 等非必须参数缺失或不完整不阻止启动。生成的模板不含 DNS 服务商和同步项，items 可全禁用，不产生占位条目的周期性报错。默认 `routeros.tls.insecureSkipVerify: false`；自签名路由器需改为 `true` 或提供 `caFile`。

修复范围仅限 [配置参考 §1.1](configuration.md#11-启动期自动修复与回写) 所列语义错误；显式空 state.path 合法，maxBackoff 小于生效 pollInterval 时即使缺失也会修正并补写。非空非法 api.listen 仍致命（手动 `2` / 服务 `1`），监听绑定失败均退出 `1`。合法 token 经 trim 后为空或 1–256 个 `0x21`–`0x7e` 字符，不含空格或控制字符；回环可空，非回环必须有生效的非空 token。通知只提示生成结果及“见启动日志”，不含 token。

普通网络、认证或对账错误留在服务内退避重试。配置错误项与客户端构造错误须在修正配置并成功保存或重启后重建。`-check` 仍严格，不修复也不获取锁；可在启动回写后再次核对磁盘配置，若只有内存回退则仍可能报告原非法值。交互校验不加载服务环境，含 `${VAR}` 的字段需提供同样环境：

```bash
sudo -u ros-ddns /usr/local/bin/ros-ddns -config /var/lib/ros-ddns/config.yaml -check
curl -fsS http://127.0.0.1:8080/healthz
curl -sS -i http://127.0.0.1:8080/readyz
```

- WebUI 保存 / PUT 是严格事务：校验或候选构造失败不改文件、保留旧服务；成功后写同一配置文件（`.bak` 备份）并重建服务。`api.listen`、`api.token`、`log.format` 在页面只读，需编辑配置后执行 `sudo /usr/local/bin/ros-ddns -service restart`；PUT 拒绝这些有效值的变化。
- 日志：systemd 用 `sudo journalctl -u ros-ddns.service -f`；Windows 在“事件查看器 → Windows 日志 → 应用程序”查看服务启动错误、修复告警及自动生成的 token，运行期日志也可从 WebUI“日志”页查看。自动 token 在安装日志脱敏器之前打印一次，应限制服务日志访问；通知中心不含 token 明文。

退出码 `0` 表示成功或正常停止，`1` 包括锁、默认配置生成、监听、运行及服务方式的致命配置错误，`2` 包括参数、严格 `-check` 或手动启动的致命配置错误。启动修复、回写失败及可容忍的同步错误不会单独使进程退出。

### 2.4 升级、重启与回滚

先备份配置、`.bak`、状态与 CA 文件，并保留旧二进制：

```bash
(
set -e
sudo cp -p /usr/local/bin/ros-ddns /usr/local/bin/ros-ddns.previous
tmp=$(sudo mktemp /usr/local/bin/.ros-ddns.install.XXXXXX)
sudo install -o root -g root -m 0755 build/bin/ros-ddns-linux-amd64 "$tmp"
sudo mv -f "$tmp" /usr/local/bin/ros-ddns
sudo /usr/local/bin/ros-ddns -service restart
sudo /usr/local/bin/ros-ddns -service status
)
```

确认版本、日志、探针和实际目标后再清理旧版本。unit 由库按安装时的参数生成，升级二进制不需要改 unit；若使用了 2.6 的加固 drop-in，需要自行保留。回滚时恢复旧二进制并 `-service restart`，必要时恢复兼容的配置 / 状态。单个 `.bak` 会被启动回写或 WebUI 保存覆盖，不是完整的发布回滚历史；本地回滚不会撤销已经发生的远端 DNS / 列表修改。

### 2.5 卸载

```bash
sudo /usr/local/bin/ros-ddns -service uninstall
```

`uninstall` 会先停止运行中的服务，再注销并移除 unit / Windows 事件源，但保留配置文件、状态与用户账号。确认备份完成后，可手动删除 `/usr/local/bin/ros-ddns`、`/var/lib/ros-ddns`、可选加固 drop-in 以及 `ros-ddns` 用户 / 组；Windows 删除二进制与数据目录。删除本地文件不会删除远端 DNS 或清理 RouterOS 列表。

### 2.6 可选 systemd 加固

库生成的默认 unit 只含启动所需字段，不自带 `ReadWritePaths`。需要旧安装脚本那套加固时，创建 drop-in 即可（不要直接编辑生成的文件）：

```bash
sudo mkdir -p /etc/systemd/system/ros-ddns.service.d
sudo tee /etc/systemd/system/ros-ddns.service.d/hardening.conf >/dev/null <<'EOF'
[Service]
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/ros-ddns
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

启用 `ProtectSystem=strict` 后，配置目录同样受只读限制，需要通过 `ReadWritePaths` 显式放行写入目录。`WorkingDirectory` 只决定相对路径的基准，不会授予写权限；例外目录必须按 2.2 预先创建，并允许服务用户写入，`ReadWritePaths` 本身不会创建目录或改变文件权限。配置、`.bak`、状态与锁文件都应位于可写例外中，其父目录也需要可写以支持临时文件和 rename；修改这些路径时须同步调整例外。CA 文件只需服务用户可读，可以放在可写例外之外。卸载服务不会删除 drop-in 目录，需要手动清理。

## 3. Docker Compose

### 3.1 前提与准备

目标为支持 Linux 容器的 Docker Engine / Docker Desktop 与 Compose v2。首次构建需能拉取构建镜像和依赖；`Dockerfile` 使用 `golang:1.26-bookworm` 多阶段静态编译，运行镜像为带 CA 与时区数据的 `alpine:3.22`，只复制二进制。构建元数据可由 `VERSION`、`COMMIT`、`BUILD_DATE` build args 注入；`make docker-build` 会自动带入当前 git 版本（tag 存在时为 `v0.1.0`）并写入镜像 OCI 标签，例如 `make docker-build IMAGE=ros-ddns:v0.1.0`。直接 `docker build` 需显式传 `--build-arg`，否则镜像内版本为 `dev` / `unknown`。构建上下文排除 `.env`、`.secrets`、本地配置、缓存与数据目录，凭据不应通过构建参数传入。

以下为 Linux 宿主机首次准备命令；`data/` 必须先存在并允许 `10001` 写入，容器首次启动会在其中生成 `config.yaml`：

```bash
mkdir -p data
cp .env.example .env
chmod 600 .env
# 编辑 .env：ROS_PASSWORD，并预先设置合法非空 ROS_DDNS_API_TOKEN；设置
# ROS_DDNS_LISTEN=0.0.0.0:8080；可按需覆盖 ROS_DDNS_ROUTEROS_URL、
# ROS_DDNS_ROUTEROS_USERNAME、ROS_DDNS_CA_FILE 及 log/runtime/state 参数
sudo chown 10001:10001 data
sudo chmod 700 data
```

容器固定以 `10001:10001` 运行，绑定目录及现有状态 / `.bak` / CA 文件也必须允许该身份访问。宿主机普通用户可能需要 sudo 编辑 `0700` 目录。Docker Desktop 的 bind mount 权限行为取决于文件共享后端，应验证容器内可写并用宿主机 ACL 限制读取。SELinux 主机如遇挂载拒绝，需要管理员为该私有数据挂载配置适当标签。

程序本身不读取 `.env`，Compose 的 `env_file` 将其注入进程。默认模板通过环境引用读取 RouterOS 凭据、API token / listen 与 `log`、`runtime`、`state` 参数；首次启动后在 WebUI 中添加 Aliyun 服务商、同步项目，凭据允许稍后填写，或手工补充配置。非回环启动自动生成 token 会尝试把 token 引用替换为字面值，见下文。

不要打印或分享展开后的 `docker compose config` 输出，它可能包含凭据；只校验语法可用 `docker compose config --quiet`。env 示例的单引号可防止 Compose 插值普通 `$` 等字符；含引号等特殊内容时按 Compose env 文件语法处理。更新 `.env` 中的变量后需要重新创建容器，`docker compose restart` 不会重新注入 env。

容器首次生成的配置使用 `.env` 的 `ROS_DDNS_LISTEN=0.0.0.0:8080` 才能让发布端口可达。建议在首次启动前设置合法非空 `ROS_DDNS_API_TOKEN`（1–256 个可打印 ASCII 字符，不含空格 / 控制字符）。若非回环监听下 token 为空或非法，程序会自动生成、尝试写回字面 token，并在启动日志打印一次；即使宿主机只发布到 `127.0.0.1` 也适用。**成功回写后，修改 `.env` 中的 `ROS_DDNS_API_TOKEN` 不再覆盖 token**：需修改 `data/config.yaml` 中的字面值，或恢复 `${ROS_DDNS_API_TOKEN:-}` 引用并重新创建容器；API token 不能通过 WebUI 热重载。回写失败时生成值仅本次进程有效，应从当前启动日志取得。

未设置 `ROS_DDNS_LISTEN` 时生成默认回环监听，宿主机发布端口将无法访问 WebUI。默认宿主机绑定 `127.0.0.1:8080:8080`；端口被占用时在 `.env` 中设置 `ROS_DDNS_HTTP_BIND=127.0.0.1:18082` 等值，容器内仍监听 8080。远程访问通过受控 TLS 反向代理；代理须保留正确 Host / 转发协议语义。不要直接把无 TLS 的管理接口公开到互联网。健康探针公开，不需要 token。自签名 RouterOS 默认校验证书：可把 CA 挂载到 `/data/router-ca.pem` 并用 `ROS_DDNS_CA_FILE` 指定，或在 WebUI / 生成配置中把 `insecureSkipVerify` 改为 `true`；若在配置中写环境引用，展开值必须是 `true` / `false`。

### 3.2 构建、校验与运行

```bash
# 1) 基础构建：不注入版本，镜像内 -version / WebUI 显示 dev
docker compose config --quiet
docker compose build
docker compose up -d

# 2) 需要标准版本号时：先从 git 计算元数据，再构建并用现有镜像启动
VERSION=$(git describe --tags --always --dirty) \
COMMIT=$(git rev-parse --short HEAD) \
BUILD_DATE=$(git log -1 --format=%cI) \
docker compose build
docker compose up -d --no-build

docker compose ps
docker compose logs --tail=100 -f ros-ddns
# 首次启动生成 data/config.yaml 后可做静态校验
docker compose run --rm --no-deps ros-ddns -config /data/config.yaml -check
curl -fsS http://${ROS_DDNS_HTTP_BIND:-127.0.0.1:8080}/healthz
curl -sS -i http://${ROS_DDNS_HTTP_BIND:-127.0.0.1:8080}/readyz
```

`docker compose up -d --build` 会以 Compose 文件中的默认值（`dev` / `unknown` / `unknown`）重建并覆盖现有镜像，不会自动读取 git tag；只有显式导出上面三个变量（或作为一次性环境变量传给命令）时镜像才带标准版本。Windows PowerShell 可用 `$env:VERSION = "$(git describe --tags --always --dirty)"`、`$env:COMMIT = "$(git rev-parse --short HEAD)"`、`$env:BUILD_DATE = "$(git log -1 --format=%cI)"` 设置后再执行 compose 命令。

Compose 每 30 秒通过容器内 `wget` 请求 `healthz`，超时 5 秒，启动宽限 15 秒，连续 3 次失败标记 unhealthy。Docker 的 `unless-stopped` 处理进程退出，**不会仅因 unhealthy 自动重启**；需由运维监控处理。healthcheck 只证明 HTTP 存活，不证明实际 DNS 写入。容器内程序按前台模式运行：配置缺失时在 `/data/config.yaml` 生成模板，已有文件解析 / 解码失败退出 `2`、原文件不变。模板从进程环境读取所引用参数，但自动 token 回写会替换其占位符。DNS 服务商与同步项目需启动后添加；若 `.env` 未设置 `ROS_DDNS_LISTEN`，生成的默认监听为 `127.0.0.1:8080`，宿主机发布端口无法访问 WebUI。

运行时根文件系统只读，去除所有 capabilities，并启用 no-new-privileges；`/tmp` 使用 tmpfs，`/data` 是可写持久挂载。默认状态路径为 `/data/state.json`，锁由 Dockerfile 的 `CMD` 固定为 `/data/state.json.lock`（不随 state.path 热重载移动）。不要只绑定单个 `config.yaml` 文件或把 `/data` 挂成只读，否则启动修正回写与 WebUI 的临时文件、rename 和 `.bak` 保存无法正常工作。启动回写失败仅内存生效，WebUI 保存失败则保留旧服务；锁获取或缺失文件生成失败仍致命。热重载状态路径也应留在可写目录内。

`docker compose stop` / `down` 发送停止信号并给予 20 秒退出时间。锁文件留在宿主机 data 中是正常现象；停止后可直接重启，不用删除锁。不要与宿主机服务或另一个容器同时管理相同目标。容器内程序使用同一单实例锁；静态校验可执行 `docker compose run --rm --no-deps ros-ddns -config /data/config.yaml -check`，对账由运行中的容器完成。

### 3.3 WebUI、更新与清理

WebUI 地址是 `http://127.0.0.1:8080/`。首次打开后，在 Config 页添加 DNS 服务商，再到「同步项目」页添加接口与域名等信息；AK / Secret 可稍后填写，未完成的项目可停用保存为草稿。保存更新宿主机 `data/config.yaml`，备份为 `data/config.yaml.bak`；未编辑的环境引用保留，启动已生成的字面 token 不会自动变回占位符，注释和排版可能被规范化。环境变化、API 地址 / token、日志格式修改需重新创建容器；其他支持字段可由 WebUI 严格校验后热重载。手动编辑磁盘配置后也需重启 / 重建才能加载。

升级前备份 data 和 `.env` 到权限受限的位置，并保留旧镜像：

```bash
docker image tag ros-ddns:local ros-ddns:previous
VERSION=$(git describe --tags --always --dirty) \
COMMIT=$(git rev-parse --short HEAD) \
BUILD_DATE=$(git log -1 --format=%cI) \
docker compose build --pull
docker compose up -d --force-recreate
docker compose logs --tail=100 ros-ddns
```

不注入上述变量时，升级后的镜像会显示 `dev` / `unknown` 版本；需要版本可追溯时保留命令前的变量赋值。检查版本、探针、WebUI 与目标结果。需回滚时先 `docker compose down`，将 `ros-ddns:previous` 重新标记为 `ros-ddns:local`，恢复必要的兼容配置 / 状态，再 `docker compose up -d --no-build`；回滚过程中不要重建覆盖旧镜像。不要用 `.bak` 代替独立的发布备份。

正常下线使用 `docker compose down`，它保留 bind mount 的 data 与 `.env`；即使 `down -v` 也不会清理这个宿主机目录。确认所有进程已停止、备份完成且无需恢复后，再手动删除 data、`.env` 和不再使用的镜像。注意本地数据清理不会撤销远端变更。

## 4. 安全与验收清单

配置、`.bak`、状态和锁即使没有明文凭据，也可能暴露域名、IP、接口、RecordId 和路径。配置中的凭据使用文件权限保护（生成文件 `0600`、目录 `0700`，回写保留原权限），设置 token 后配置 API 仅供已认证客户端使用，静态 WebUI 与探针仍公开。服务安装不单独生成 env 文件；字面凭据及自动生成的 token 可能直接位于配置中，因此配置文件权限也保护凭据。自动 token 在安装脱敏器前打印一次，服务方式另写 OS 服务日志，须限制日志读取。环境变量仍可被宿主机管理员读取，不是抵抗管理员的秘密存储。不要公开日志、配置 API 响应或未经检查的诊断包。

- 静态配置校验通过；域名、接口、CA、token 和目录权限均已核对。`-check` 不修复；启动修复后再次检查磁盘配置，仅内存回退可能仍使严格校验失败。
- 在独立测试配置中确认手动和服务方式仅在文件缺失时生成默认配置；已有文件的语法、未知字段、类型 / 解码错误均退出且原文件字节不变。生成配置不含 DNS 服务商或同步项，允许空 providers、空 items 或全部禁用。
- 验证指定必须参数的字面非法值回退、`.bak` 原文备份及权限保留；环境占位符语义非法时仅内存回退（无其他回写则无 `.bak`）；显式空 state.path 不修复；maxBackoff 小于生效 pollInterval 时修正并补写。
- 验证非回环缺少合法 token 时自动生成、生效并尝试回写字面值，启动日志 / 服务日志可取得 token，`config_autofix` 通知不包含 token；检查 `.env` token 管理是否仍有对应占位符。
- 在测试配置中验证回写失败不退出、WARN / 通知说明仅内存生效；无效启用项与有效项并存，前者为 error 且不可触发、ready 为 false，后者继续运行；RouterOS 构造和状态缓存打开失败按上述规则处理。
- 添加有效同步项后，验证凭据缺失或错误时进程保持运行、日志与通知报错并退避重试；修复配置并成功热重载后恢复。
- 启动日志显示取得预期锁；第二个使用同一锁的实例立即失败且不发起同步；持锁时 `-check` 仍成功。
- `healthz` 为 200；无配置错误且所需源轮询成功后 `readyz` 为 200，错误项或尚未成功读取源时为 503；同时检查 phase / lastError，不能把探针当作 DNS 写入验收。
- WebUI 能读取、严格校验并保存允许热重载的配置；校验 / 候选构造失败时文件及旧服务不变；磁盘配置及 `.bak` 所有者 / 权限正确，未编辑的 env 引用保留。
- 正常停止后可再启动，历史锁文件无需删除；更改状态路径时安排完整停启。
- 在服务器分别验证服务安装与可选加固 drop-in、Docker bind mount / healthcheck、日志和 20 秒停止窗口。
- 在专用测试目标验证 RouterOS 与 DNS 读写、续期、故障恢复、升级及回滚。`-check` 只做静态校验，不验证阿里云权限或真实 DNS 写入，本地单元测试也不替代这一步。
