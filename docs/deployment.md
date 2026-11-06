# 部署

## 构建

需要 Go 1.26.10+；Linux amd64 示例，ARM64 改用 `GOARCH=arm64`：

```bash
make build-linux # 输出 build/bin/ros-ddns-linux-amd64
# 没有 make 时：
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o build/bin/ros-ddns-linux-amd64 ./cmd/ros-ddns
```

Make 从 git tag / 提交注入 `VERSION`、`COMMIT`、`BUILD_DATE`，可用同名变量覆盖；直接 `go build` 默认显示 `dev`。
Compose 不自动读 git 版本，构建前在同一终端执行：

```bash
export VERSION="$(git describe --tags --always --dirty)"
export COMMIT="$(git rev-parse --short HEAD)"
export BUILD_DATE="$(git log -1 --format=%cI)"
```

## systemd

首次安装在服务器上执行，已有账号时跳过 useradd：

```bash
sudo useradd --system --user-group --home-dir /var/lib/ros-ddns --no-create-home --shell /usr/sbin/nologin ros-ddns
sudo install -d -o ros-ddns -g ros-ddns -m 0700 /var/lib/ros-ddns
sudo install -m 0755 build/bin/ros-ddns-linux-amd64 /usr/local/bin/ros-ddns
sudo /usr/local/bin/ros-ddns -service install \
  -config /var/lib/ros-ddns/config.yaml \
  -lock-file /var/lib/ros-ddns/state.json.lock -service-user ros-ddns
sudo /usr/local/bin/ros-ddns -service start
sudo /usr/local/bin/ros-ddns -service status
sudo /usr/local/bin/ros-ddns -service restart
sudo journalctl -u ros-ddns -f
```

- install 只注册服务，start 才启动并在配置缺失时生成文件。
- 服务不继承当前终端环境，凭据填入配置 / WebUI，或通过 systemd 环境配置注入。
- 服务工作目录是配置所在目录，手动运行则是当前目录，state / CA 等路径建议使用绝对路径。
- 远程访问默认回环 WebUI 可用 `ssh -L 8080:127.0.0.1:8080 user@server`，再打开本机 8080 端口。
- 更新二进制前执行 `-service stop`，备份旧二进制和配置，替换后 `-service start`；回滚时停服并恢复备份。

Windows 在管理员 PowerShell 中使用相同服务命令，并将二进制放在固定路径：

```powershell
.\scripts\go.cmd build -o build\bin\ros-ddns.exe ./cmd/ros-ddns
# 将二进制复制到固定目录后，例如 C:\ProgramData\ros-ddns：
C:\ProgramData\ros-ddns\ros-ddns.exe -service install -config C:\ProgramData\ros-ddns\config.yaml
C:\ProgramData\ros-ddns\ros-ddns.exe -service start
```

Windows 默认服务账号为 LocalSystem，需能读写配置目录；后续用同一程序的 `-service status/stop/restart/uninstall`。

## Docker Compose

在仓库根目录执行：

```bash
mkdir -p data
sudo chown 10001:10001 data
cp .env.example .env
chmod 600 .env
# 编辑 RouterOS 密码、随机 API token、ROS_DDNS_LISTEN=0.0.0.0:8080
docker compose up -d --build
docker compose logs -f --tail=100
```

- **容器使用 `10001:10001`，data 属主不正确会在创建锁文件时报 `permission denied`。**
- Compose 的 `env_file: .env` 注入变量，程序本身不读取 `.env`，变量中的 `$` / `#` 用单引号保护。
- 默认端口映射为 `127.0.0.1:8080:8080`，例如设置 `ROS_DDNS_HTTP_BIND=127.0.0.1:18080` 后访问本机 18080。
- 容器内 `ROS_DDNS_LISTEN` 保持 `0.0.0.0:8080`，修改 `.env` 后用 `docker compose up -d --force-recreate`。
- 首次 token 未设置或非法时自动生成、尝试写回 `data/config.yaml` 并打印到日志，写回成功后 `.env` 不再覆盖它。
- 首次启动后在 WebUI 添加服务商和项目，自签名 RouterOS 的 CA 文件可放入 `data/` 并配置 `/data/文件名`。

## Docker 升级、回滚与停止

先停容器备份 data，再切换源码版本并构建：

```bash
docker compose stop
sudo cp -a data data.backup # 使用新的备份目录名，避免覆盖旧备份
git pull --ff-only
# 重新执行上方 VERSION / COMMIT / BUILD_DATE 导出命令
docker compose up -d --build
docker compose logs --tail=100
docker compose down # 停止并移除容器，保留宿主机 data/
```

回滚时先停止容器，切到旧 git tag、重新导出版本变量并构建，必要时恢复对应 data 备份。

## 文件与排障

- state 缺失字段默认 `./data/state.json`，自动生成配置使用配置目录的 `state.json`；默认锁为 `state.path + ".lock"`，Compose 固定 `/data/state.json.lock`。
- 显式 `state.path: ""` 仅保留内存状态但仍有临时锁，可用 `-lock-file` 固定锁路径。
- 配置和 `.bak` 在同一目录，目录需可写；`.bak` 只保留最近一次保存前的内容。
- 锁冲突先用 `-service status` 或 `docker compose ps` 查重，不要删除运行中实例的锁文件。
- 页面打不开先看日志、监听地址和端口映射，同步失败再看 WebUI 项目错误。
- 退出码：`0` 成功，`1` 运行 / 服务失败，`2` 参数、`-check` 或手动启动配置错误；服务 status 仅 running 返回 `0`。
