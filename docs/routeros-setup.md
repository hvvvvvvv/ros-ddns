# RouterOS 7 配置指南

本页用于准备 REST、证书、专用用户和地址列表，并在自己的设备上手工验证。所有真实设备步骤均为待执行的操作清单，不代表已完成现场验收。程序命令和 HTTP 合同见 [README](../README.md)。

## 1. 前提条件

使用支持 REST 的 RouterOS 7。HTTPS REST 早于 7.9 已提供；HTTP `www` 的 REST 支持从 7.9 开始，生产连接使用 `www-ssl`。REST 使用 HTTP Basic Auth，不走 native API 的 `api` / `api-ssl` 服务，本项目没有 RouterOS 6 原生 API 支持。[MikroTik REST API](https://help.mikrotik.com/docs/spaces/ROS/pages/47579162/REST%2BAPI)

运行机器须能访问路由器管理地址。IPv6 来源必须实际出现在 `/ipv6/address`；仅取得 DHCPv6-PD 租约或地址池，不代表所配置接口已持有可选地址。程序不会向外部“我的 IP”服务查询，也不会从 PD 池直接构造源地址。

## 2. 启用 REST：证书与 www-ssl

### 2.1 配置 HTTPS

在设备上按实际证书文件名导入证书和私钥，选定服务端证书：

```text
/ip service print detail
/certificate import file-name=router-server.crt
/certificate import file-name=router-server.key passphrase="<私钥导入密码>"
/certificate print detail
/ip service set www-ssl certificate=<证书名> disabled=no
```

证书 SAN 应包含 `routeros.url` 使用的 IP 或 DNS 名，证书链和有效期也必须通过校验。系统已信任的 CA 可直接使用；私有 CA 在客户端配置 PEM 文件：

```yaml
routeros:
  url: https://192.168.88.1
  username: ddns-api
  password: ${ROS_PASSWORD}
  timeout: 10s
  tls:
    insecureSkipVerify: false
    caFile: ./certs/router-ca.pem
```

`caFile` 相对进程工作目录，客户端将其加入系统信任池；系统已信任时可省略。代码与内嵌默认配置均使用 `insecureSkipVerify: false`，默认验证证书。`true` 与非空 `caFile` 互斥；临时跳过验证不能检验证书部署是否正确。

配置 URL 只写 origin 加可选基础路径，例如 `https://router.example.net/router`，客户端追加 `/rest`。hostname 必须非空；端口可省略，显式提供时须为十进制 1–65535，不能留空。userinfo、查询串、fragment、末尾 `/rest` 均会被拒绝；账号密码用独立字段。`routeros.timeout` 缺省为 `10s`，显式零或负值无效。

这些是严格校验与实际同步的要求：没有启用项时 routeros 可缺失或全空，只校验已提供的 URL 格式、timeout > 0 和 TLS 互斥；有启用项时要求 URL、用户名并完整校验。守护进程启动不会因 RouterOS 配置不全或客户端构造失败退出，需要源读取的项进入轮询退避，每轮失败记录 WARN，并由页内通知报告错误，首次成功读取前 ready 为 false。配置或 CA 构造问题修正后需成功保存或重启，以重新构造客户端。

### 2.2 明文 www 仅用于隔离测试

```text
/ip service set www disabled=no address=192.168.88.10/32
```

仅在无人可窃听的隔离测试环境用 `http://`；Basic Auth 及流量明文可被读取。测试完成后按设备原有用途恢复服务设置。正式部署使用 HTTPS。[MikroTik 的 HTTP 风险说明](https://help.mikrotik.com/docs/spaces/ROS/pages/47579162/REST%2BAPI)

### 2.3 限制服务来源

```text
/ip service set www-ssl address=192.168.88.10/32
```

用路由器实际看到的客户端源 IP 替换示例，经过 NAT 时可能不是主机本地地址。此限制也影响同服务上的 WebFig HTTPS 管理访问，配置时保留必要管理来源。

## 3. 专用用户与权限

读接口和列表需要 `read`，REST 登录需要 `rest-api`，维护列表需要 `write`。配置专用组和用户，限制来源：

```text
/user group add name=ddns-api policy=read,write,rest-api
/user add name=ddns-api group=ddns-api password="<强密码>" address=192.168.88.10/32 comment="ros-ddns"
/user group print detail where name=ddns-api
/user print detail where name=ddns-api
```

`api` 不能代替 `rest-api`，不要复用 admin。只读源并更新 DNS、不维护 RouterOS 列表的部署可使用 `read,rest-api`，并按实际操作验证。RouterOS 的 write 是较宽的配置权限，不能按 list 名或 comment 限制；comment 是管理约定，不能代替权限和来源隔离。[MikroTik 用户策略](https://help.mikrotik.com/docs/spaces/ROS/pages/8978504/User)

## 4. 防火墙与管理来源

服务和用户来源限制之外，还需核对现有 input 链。以下仅是允许测试主机访问 HTTPS 的规则示例：

```text
/ip firewall filter add chain=input action=accept protocol=tcp dst-port=443 src-address=192.168.88.10/32 comment="ros-ddns REST"
```

规则须位于会丢弃该流量的规则之前，并与原有管理策略配合；单独添加 accept 不等于已经拒绝其他来源。若使用 IPv6 管理地址，还需检查 `/ipv6 firewall filter`，IPv4 规则不会覆盖 IPv6。修改前保留自己的管理通道。

## 5. REST 与程序冒烟测试（手工）

### 5.1 四类 GET

从运行 ros-ddns 的主机执行；`--user ddns-api` 交互询问密码，系统信任证书时可省略 `--cacert`。

```bash
curl --fail --silent --show-error --user ddns-api --cacert ./certs/router-ca.pem \
  'https://192.168.88.1/rest/ip/address'
curl --fail --silent --show-error --user ddns-api --cacert ./certs/router-ca.pem \
  'https://192.168.88.1/rest/ipv6/address'
curl --fail --silent --show-error --user ddns-api --cacert ./certs/router-ca.pem \
  'https://192.168.88.1/rest/ip/firewall/address-list?list=ddns-home-ipv4'
curl --fail --silent --show-error --user ddns-api --cacert ./certs/router-ca.pem \
  'https://192.168.88.1/rest/ipv6/firewall/address-list?list=ddns-home-ipv6'
```

PowerShell 使用 `curl.exe`，单行示例：

```powershell
curl.exe --fail --silent --show-error --user ddns-api --cacert ./certs/router-ca.pem https://192.168.88.1/rest/ip/address
```

检查 JSON 数组中的 interface、actual-interface、address 以及 disabled / invalid；IPv6 还要检查 global、link-local、deprecated。REST 通常将属性值编码为字符串。空数组也算 GET 成功，不能证明选中了地址；GET 不能验证列表写权限。生产验证不要用 `-k` 跳过 TLS。[REST JSON 与读取说明](https://help.mikrotik.com/docs/spaces/ROS/pages/47579162/REST%2BAPI)

### 5.2 配置校验

先按 README 构建程序、设置环境变量并编辑本地配置。在仓库根目录执行：

```powershell
.\scripts\go.cmd run ./cmd/ros-ddns -config build/configs/config.local.yaml -check
$LASTEXITCODE
```

Linux 对已构建程序执行：

```bash
./build/bin/ros-ddns -config build/configs/config.local.yaml -check
echo $?
```

`-check` 严格校验并输出文本摘要，不联网、不读取 CA 内容、不获取锁、不修复或回写配置；启动时本可自动修复的非法值也会使检查失败。守护进程启动修复范围见 [配置参考](configuration.md#11-启动期自动修复与回写)，启动后可再次检查磁盘配置。`-once` 与 `-dry-run` 已移除，因此没有只读预演/单轮模式：需要读取真实 RouterOS 源地址时启动守护进程，在 WebUI / 日志中核对 sourceAddress、recordValue、phase 与 lastError（此时会进行真实对账，请使用测试目标）。

### 5.3 实际同步与状态

下面会实际修改所配置目标；测试时使用独立列表和测试 DNS 记录。

```powershell
.\build\bin\ros-ddns.exe -config build/configs/config.local.yaml
```

Linux 使用相同参数调用 `./build/bin/ros-ddns`。有正 stableFor 时，持续运行才能获得后续稳定观察。

在另一终端查询守护进程：

```powershell
curl.exe -sS -i http://127.0.0.1:8080/readyz
curl.exe -sS http://127.0.0.1:8080/api/v1/status -H "Authorization: Bearer $env:ROS_DDNS_API_TOKEN"
curl.exe -sS -X POST http://127.0.0.1:8080/api/v1/items/home-ipv4/sync -H "Authorization: Bearer $env:ROS_DDNS_API_TOKEN"
```

回环监听且生效 token 为空时可省略 Authorization；有 token 时 `/api/v1/*` 必须认证。非回环启动若 token 为空或非法会自动生成、尝试写回并在启动日志打印一次，服务方式还写 OS 服务日志；使用自动 token 时，应把当前日志中的值用于上述命令，程序不会替当前终端设置环境变量。ready 的 `200 {"ready":true}` 要求没有启动配置错误项，并且所有启用项曾成功读取源，无候选也算；同步应检查各项及各目标错误。sync 的 `202` 只表示排队，未知 / 禁用 / 配置错误项为 `404`。完整响应见 [API 合同](architecture.md#10-http-api-与状态结构)。

## 6. 地址列表生命周期

### 6.1 所有权与主机身份

```text
/ip firewall address-list print detail where list="ddns-home-ipv4"
/ipv6 firewall address-list print detail where list="ddns-home-ipv6"
```

所有权要求 family、list 和 comment 作用域匹配；默认 comment 为 `ros-ddns:<name>`，按完全相等匹配，`ros-ddns:home-other` 不属于 `ros-ddns:home`。

期望值是单个主机。相同裸地址、IPv4 `/32` 或 IPv6 `/128` 可满足目标；一般子网不能代替主机。例如受管 `203.0.113.0/24` 必须被替换才能满足 `203.0.113.0`，受管 IPv6 `/64` 也不能代替同地址的 `/128`。状态 entries 保留前缀语义，不会将子网伪装成裸主机。

### 6.2 受管与外部条目

| 现状 | 下次实际对账的处理 |
| --- | --- |
| 受管旧地址 | 删除后再补期望值 |
| 受管同主机被 disabled | 删除后按配置重新添加启用项（若已有外部启用匹配项则无须重复添加） |
| 受管静态改动态、动态改静态、期限不匹配 | 删除后按配置重建 |
| 受管同主机、启用、静态 / 动态标志及期限一致 | 保留，直到需要续期或其他差异出现 |
| 外部启用的匹配主机 | 满足存在性，不重复添加，不接管属性 |
| 外部禁用的匹配主机 | 保持原样，报冲突并使项 degraded，需人工启用或移除 |
| 其他外部项 | 保持原样 |

受管项同时检查 dynamic 标志和 timeout：静态配置要求 `Dynamic=false` 且返回 timeout 为空；动态配置要求 `Dynamic=true` 且期限匹配，否则删除并按需重建。期限比较可识别 `1h` 与 `01:00:00` 等价，返回 `59m` 则与配置 `1h` 不一致。所有应删除的受管项先处理，任一删除失败都会阻止新增。REST 新增收到 HTTP 400 的 `already have such entry` 时返回 `ros.ErrEntryExists`，服务记录失败，源可用时该项为 degraded；下一次列表读取才能确认有效目标或仍存在的冲突，不会直接标记 synced。切换不是原子替换，可能暂时缺少目标条目。

外部条目始终不修改；同地址重复项也不保证全部去重。改 list / comment、移除条目或禁用条目不会清理旧作用域。

### 6.3 静态、动态与续期

空 timeout 新增时不发送该参数，形成静态条目；有限 timeout 形成动态条目，会自然到期，重启后也会消失。静态条目保存在设备持久存储中。[MikroTik 地址列表持久性](https://help.mikrotik.com/docs/spaces/ROS/pages/130220135/Address-lists)

启用项的 `addressList.timeout` 在本地解析并检查时间语义：支持 `1h30m`、`1w2d3h4m5s`、`00:10:00`、`10:00` 和裸秒数 `3600`，时钟的分 / 秒必须小于 60，总时长不得超过 `35w3d13h13m56s`；`01:60`、`36w` 及溢出值使严格校验失败，守护进程启动则将该项标为 error、不调度，其余有效项继续。这仍不代表设备已接受。`0` / `0s` 允许加载且作为非空 timeout 发送，但不能据此安排有效的到期前续期；静态条目用空字符串，动态条目应配置正常正期限。禁用草稿不检查列表语义，但仍须通过类型 / 解码检查。完整规则见 [配置参考](configuration.md#38-itemsaddresslist)。

守护进程通常在配置正期限约一半时通过每项独立的列表专用 worker 读取列表、删除并重建受管动态项。续期不调用 DNS，也不等待 DNS 请求或退避；同项列表操作通过列表锁串行，刷新不会在每次源轮询都发生。续期失败以独立退避重试，不改变 DNS 重试时机。静态受管项没有续期定时器。外部 dynamic 匹配项通常按其返回期限约一半安排复查，不延长其寿命；到期消失后，如源仍可用，可创建自己的条目。动态期限为空、无法解析或不大于 `1ns` 时使用有界复查，默认 1 分钟，正 verifyInterval 更短时取其值。

`verifyInterval: 0s` 仅关闭全目标周期校验，动态列表独立续期仍运行。服务重启后所有恢复的列表缓存都标记待重查，包括配置为静态的列表；即使同时设置 `syncOnStart: false`、`verifyInterval: 0s`，首次获得可用稳定源值的轮询仍会触发列表重读，确认后才能 synced，并重新安排必要的续期 / 复查。重启后的首次待重查列表由独立列表 worker 在源可用后立即读取，与启动 DNS 对账并行，不等待 DNS Ensure 完成。

续期是尽力而为：RouterOS 临近到期不可达、源无候选、总时限耗尽或重试退避，都可能让条目先过期。恢复后再添加，不能保证全程存在。守护进程停止或路由器重启也会打断续期。无源时程序不主动删除，但无法阻止设备自然到期。

列表只是供防火墙引用的数据，不会自动创建规则，也不会为接口分配地址。

## 7. IPv6 与 EUI-64 注意事项与验证

如果 PD 前缀分配到 LAN bridge，应选择实际持有地址的接口；仅选择发起 PD 请求的 PPPoE 接口可能无候选。多前缀时可用 `select.prefix`，但固定旧前缀可能在运营商换前缀后失配。

`eui64-mac` 可属于目标终端，不必等于源接口 MAC；前提是所选地址前 64 位与目标网段一致，终端确实使用该 EUI-64 IID。只支持 48 位、非全零单播 MAC；启用项的非法 MAC 会被 `-check` / WebUI 保存 / PUT 拒绝，守护进程启动则保留该项为 error，停止其轮询与触发，其余有效项继续。程序不会回退到基础地址。

| 测试向量 | 值 |
| --- | --- |
| 前缀 | `2001:db8:1234:5678::/64` |
| MAC | `00:1A:2B:3C:4D:5E` |
| IID | `021a:2bff:fe3c:4d5e` |
| 合成地址 | `2001:db8:1234:5678:21a:2bff:fe3c:4d5e` |

这是代码测试向量，文档地址不能用于公网部署。实现总取前 64 位，不要求源掩码是 `/64`。临时隐私地址和非 MAC IID 不能这样推断。

手工确认：

1. 在 RouterOS 用 `/ipv6 address print detail` 核对源地址、接口和标志。
2. 在实际目标设备确认合成地址已配置；目标是终端就去终端检查。
3. 从另一台有 IPv6 的主机访问实际服务，核对路由、防火墙和监听；单凭 ping 失败不能区分 ICMP 过滤与地址不可达。
4. 查询 AAAA 并对照实际可达地址。把合成地址加入列表不会使它自动可达。

## 8. 手工端到端验证清单

在独立测试列表 / 记录和可接受的维护窗口执行，以下描述预期行为。

1. 完成所需地址族和列表的 GET，核对来源限制、TLS、账号和实际接口。
2. 执行 `-check` 核对文本摘要；再启动守护进程，在 WebUI / 日志中核对 sourceAddress、recordValue、lastError 与目标结果。`-check` 不证明写权限，实际读取与写入由守护进程完成。
3. 启动守护进程；需要稳定窗口时等待后续成功观察。查询 status，检查目标确认值和错误，不以 ready 代替成功。
4. 在列表验证目标裸 IP 或主机路由、comment、启用状态和期限；启用 EUI-64 时按第 7 节验证实际可达性。
5. 在测试列表禁用受管项，触发 sync，确认删除重建；修改 timeout 并重启加载，确认静态 / 动态及期限变化被修复。
6. 测试受管子网与目标主机的区别，确认 `/24` / `/64` 不会错误满足主机；测试匹配的 `/32` / `/128` 可满足目标。
7. 放入外部启用匹配主机，确认不接管、不重复添加；改为外部 disabled 时确认 degraded 与冲突信息，程序不会替它启用或删除。
8. 测试动态正期限，如 `2m`，配合 `verifyInterval: 0s` 持续运行，观察约半个期限刷新，并确认列表续期不调用 DNS，DNS 请求缓慢或处于退避时也不阻塞列表续期。静态受管条目没有该刷新。
9. 在隔离环境模拟临近到期断连，允许条目自然过期，恢复后检查重建；模拟删除失败，确认本轮新增被阻止。不要把短暂空缺误报为连续存在保证。
10. 改变源地址 / 前缀，观察稳定窗口后 DNS 更新与列表先删后加；无合格源时不主动清空目标，动态项仍可能自然到期。
11. 按 [DNS 检查](aliyun-setup.md#8-验证-dns-解析) 核对权威和递归结果；禁用或移除测试绑定后确认旧记录 / 旧列表不会被自动清理，最后手工清理测试数据。

## 9. 常见问题与数据保护

| 现象 | 检查方向 |
| --- | --- |
| 连接超时 / 拒绝 | www-ssl、REST 端口、路由、input 链；不要用 native API 端口 |
| RouterOS 401 / 403 | 密码、实际来源 IP、read / write / rest-api；GET 成功不代表写权限足够 |
| TLS 失败 | CA、证书链、有效期、SAN 与 URL 主机；`-check` 不读证书 |
| error 且提示配置错误 | 启用项的 source、MAC、目标及 provider 引用；修正后严格保存或重启，错误项不会参与轮询或手动同步 |
| RouterOS 配置 / 构造错误持续重试 | 补全 URL、用户名，核对 timeout 与 TLS / CA；每轮 WARN 后退避，修正配置并成功保存或重启后重建客户端 |
| waiting 且 no eligible address | interface / actual-interface、scope、prefix、index；WebUI / status 显示错误 |
| 受管 disabled 或期限变化未修复 | 是否真正触发对账、源是否有效稳定、删除权限及列表 lastError |
| foreign disabled 冲突 | 人工核实归属后启用或移除外部条目，程序保留它 |
| 列表没有自己的 comment | 可能由外部启用匹配主机满足目标；核对 family / list |
| 动态项消失 | 停机、重启、源失效、到期附近网络错误及退避；恢复后观察补回 |
| AAAA 已写但不可达 | 目标是否实际配置合成地址、路由、防火墙和服务监听 |

配置摘要隐藏凭据，服务对错误中的已配置密码、API token、AK / STS 的原值、JSON 字符串转义及 URL query 转义表示做脱敏，长匹配值按子串、短匹配值按边界 token 处理；展开错误也会清洗，但不保证任意上游文本或上述变体以外的编码形式都无秘密。日志、API 和默认 `./data/state.json` 仍可能含域名、IP、RecordId 等，需限制权限。

API 默认回环监听，token 可空；非回环启动时若 token 为空或非法会自动生成并尝试写回字面值，在安装日志脱敏器之前打印一次，服务方式另写 OS 服务日志，通知不含 token。成功回写会替换原环境占位符，若需通过环境变量管理，应预先设置合法非空 token。限制启动日志与配置 / `.bak` 的读取权限；状态路径和 API 端口应与其他进程隔离。状态缓存打开失败会 WARN 并退化为仅内存，启动成功不代表缓存已持久化。
