# RouterOS 配置

使用 RouterOS 7 REST，运行 ros-ddns 的主机需能访问路由器管理地址。

## 创建账号并启用 HTTPS

在 RouterOS 终端执行，将示例源 IP、密码和证书名替换为实际值：

```text
/user group add name=ddns-api policy=read,write,rest-api
/user add name=ddns-api group=ddns-api password="<强密码>" address=192.168.88.10/32
/certificate import file-name=router-server.crt
/certificate import file-name=router-server.key passphrase="<私钥密码>"
/certificate print detail
/ip service set www-ssl certificate=<证书名> disabled=no
/ip service set www-ssl address=192.168.88.10/32
```

- 仅读取地址、更新 DNS 时，组权限用 `read,rest-api`；维护地址列表还需 `write`。
- REST 使用 `www-ssl` 和 `rest-api` 权限，无需开启 native `api` / `api-ssl`。
- `www-ssl address` 也限制 WebFig 访问，修改时保留自己的管理来源。
- 已配置 HTTPS 证书时跳过导入，证书 SAN 应匹配连接时使用的 IP 或域名。

## 连接配置

```yaml
routeros:
  url: https://192.168.88.1
  username: ddns-api
  password: ${ROS_PASSWORD}
  tls:
    caFile: ./certs/router-ca.pem
```

向进程注入 `ROS_PASSWORD`；系统已信任证书时省略 tls，私有 CA 使用 PEM 文件。
自签名证书也可设置 `tls.insecureSkipVerify: true`，但不能同时填写 caFile。
URL 不要加 `/rest`，默认请求超时为 `10s`；完整字段见 [配置参考](configuration.md)。

从运行主机测试读取，curl 会询问密码（Windows 用 `curl.exe`）：

```bash
curl --fail --user ddns-api --cacert ./certs/router-ca.pem https://192.168.88.1/rest/ip/address
curl --fail --user ddns-api --cacert ./certs/router-ca.pem https://192.168.88.1/rest/ipv6/address
```

## 地址列表

- `timeout: ""` 创建静态条目；正期限如 `1h` 创建动态条目，到期或设备重启后消失。
- 动态受管条目约在期限半程删除重建续期，可能短暂缺项，停机或网络中断时也可能过期。
- `verifyInterval: 0s` 不关闭动态列表续期，列表续期不调用 DNS。
- `0` / `0s` 虽可加载，但不能安排有效的到期前续期；静态用空值，动态用正期限。
- 仅管理同地址族、list 和精确 comment 的条目，默认 comment 为 `ros-ddns:<项目名>`。
- 外部 comment 不接管，已启用的同主机条目可满足目标，禁用的匹配条目需人工处理。
- 列表不会自动创建防火墙规则；禁用或删除项目也不会自动清理旧条目。

```text
/ip firewall address-list print detail where list="ddns-home"
/ipv6 firewall address-list print detail where list="ddns-home-v6"
```

## IPv6 与 EUI-64

- 选择实际持有 `/ipv6/address` 地址的接口，只有 DHCPv6-PD 地址池还不够。
- `source.eui64-mac` 仅支持 48 位非全零单播 MAC，取源地址前 64 位合成，目标设备必须实际使用该地址。
- 测试向量：`2001:db8:1234:5678::/64` + `00:1a:2b:3c:4d:5e` → `2001:db8:1234:5678:21a:2bff:fe3c:4d5e`。
- EUI-64 不适用于隐私地址，也不会给设备分配地址或保证可达。

## 排障

| 现象 | 处理 |
| --- | --- |
| 连不上 / 超时 | 检查 IP、端口、www-ssl、服务来源限制及防火墙 input 规则 |
| `401` | 核对用户名、密码和用户来源地址限制 |
| `403` / 权限不足 | 检查 read、rest-api，写列表还需 write |
| TLS 错误 | 核对 CA、SAN、证书有效期和主机时间，修改后保存配置或重启 |
| 没有候选地址 | 检查接口实际地址及 scope / prefix / index，私网需 `scope: any` |

DNS 账号准备见 [阿里云配置](aliyun-setup.md)，运行状态和日志见 [WebUI](webui.md)。
