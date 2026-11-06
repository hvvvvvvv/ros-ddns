# ros-ddns

从 RouterOS 7 读取接口地址，同步到阿里云 DNS 的 A / AAAA 记录和 RouterOS `address-list`，单二进制内置 WebUI。

## Docker 快速开始

准备好 [RouterOS REST 账号](docs/routeros-setup.md)；需要 DNS 时再准备 [阿里云 RAM AccessKey](docs/aliyun-setup.md)。

在仓库根目录执行（需要 Docker Compose v2）：

```bash
mkdir -p data
sudo chown 10001:10001 data
cp .env.example .env
chmod 600 .env
# 编辑 .env：ROS_PASSWORD、ROS_DDNS_API_TOKEN、ROS_DDNS_LISTEN=0.0.0.0:8080
# 按需设置 ROS_DDNS_ROUTEROS_URL、ROS_DDNS_ROUTEROS_USERNAME
docker compose up -d --build
```

**`data/` 属主必须是 `10001:10001`，否则容器创建锁文件会报 `permission denied`。**

打开 <http://127.0.0.1:8080/>，端口可在 `.env` 的 `ROS_DDNS_HTTP_BIND` 修改。
首次启动生成 `data/config.yaml`，在 Config 填写服务商和凭据，在 Items 添加同步项目。
容器内监听保持 `0.0.0.0:8080`；只改宿主机端口时无需改它。

## 源码运行

需要 Go 1.26.10+；在仓库根目录执行：

```bash
go build -o ros-ddns ./cmd/ros-ddns
./ros-ddns -config config.yaml
```

Windows PowerShell：

```powershell
.\scripts\go.cmd build -o build\bin\ros-ddns.exe ./cmd/ros-ddns
.\build\bin\ros-ddns.exe -config config.yaml
```

配置不存在时会生成默认文件并启动 WebUI，默认地址同上；停止前台进程按 `Ctrl+C`。

## 最小配置

可以先保存以下基础配置，再通过 WebUI 添加目标：

```yaml
log: {level: info, format: text}
runtime: {pollInterval: 10s, stableFor: 0s}
state: {path: ./data/state.json}
api: {listen: '127.0.0.1:8080', token: '${ROS_DDNS_API_TOKEN:-}'}
routeros:
  url: https://192.168.88.1
  username: ddns-api
  password: ${ROS_PASSWORD:-}
```

- `dns`、`items` 可先不填，DNS 和仅地址列表示例见 [配置参考](docs/configuration.md)。
- 配置支持 `${VAR}`、`${VAR:-默认值}`；程序不自动加载 `.env`，源码运行时用 shell 注入变量。
- 启动会修复指定必须参数的非法值并尝试回写，语法和类型错误仍会退出，范围见配置参考。
- 非回环监听缺少合法 token 时会自动生成、尝试写回并在启动日志打印一次；成功写回后 `.env` 不再覆盖该 token。
- 自签名 RouterOS 证书需配置 `tls.caFile`，或使用 `tls.insecureSkipVerify: true`，两者不能同时设置。

## 常用命令

```bash
./ros-ddns -config config.yaml -check # 严格校验，不联网、不生成或修改文件
./ros-ddns -version
sudo ./ros-ddns -service install -config /var/lib/ros-ddns/config.yaml
sudo ./ros-ddns -service start
sudo ./ros-ddns -service stop
sudo ./ros-ddns -service status
```

立即同步使用 WebUI；服务安装、日志和升级命令见 [部署指南](docs/deployment.md)。

## 文档

- [配置参考](docs/configuration.md)：字段、默认值和示例。
- [部署指南](docs/deployment.md)：systemd、Windows 服务和 Docker。
- [WebUI 与 HTTP API](docs/webui.md)：配置、同步、日志和通知。
- [RouterOS 配置](docs/routeros-setup.md) / [阿里云配置](docs/aliyun-setup.md)：账号准备和排障。

## License

[MIT](LICENSE) © 2026 hvvvvvvv。
