# 配置参考

`log`、`runtime`、`state`、`api` 是必须参数组，缺失字段使用默认值；其余可先留空或不完整，再到 WebUI 补齐。

## 加载与校验

- 只接受单份 YAML，严格解码：未知字段、重复键、语法、环境展开或类型错误会使启动失败。
- 配置缺失时，守护进程生成默认文件；`-check` 不生成、不修复、不写文件，也不联网。
- 启动仅修复下表中的显式非法值并尝试回写，先备份为 `.bak`；回写失败仍可运行。
- 缺失或 `null` 通常直接取默认值、不回写，`maxBackoff` 不足和非回环 token 生成除外。
- 环境占位符展开后可解码但值非法时，仅在内存回退、保留占位符；非回环 token 生成除外。
- 非回环 token 缺失或非法时自动生成、尝试写回字面值并打印一次，成功写回后原环境变量不再覆盖它。
- `-check`、WebUI 校验与保存均严格；启动时无效启用项显示 `error`、不参与同步，有效项继续且 `ready=false`。

| 字段 | 启动修复条件 → 值 |
| --- | --- |
| `log.level` / `log.format` | 不在允许列表 → `info` / `text` |
| `runtime.pollInterval` | ≤ 0 → `10s` |
| `runtime.stableFor` | < 0 → `0s` |
| `runtime.reconcileTimeout` | ≤ 0 → `30s` |
| `runtime.verifyInterval` | < 0 → `10m` |
| `runtime.syncOnStart` | 不修复；布尔类型错误直接退出 |
| `runtime.maxBackoff` | 小于生效的 pollInterval → `max(5m, pollInterval)`，缺失时也补写 |
| `state.path` | 非空且无法解析或与配置 / `.bak` 重合 → `./data/state.json` |
| `api.listen` | 空 → `127.0.0.1:8080`；非空但格式非法仍退出 |
| 回环 `api.token` | 非法 → 空字符串 |

## 环境变量

先解析 YAML 再展开标量，程序不自动读取 `.env`，由 shell、服务管理器或 Compose 注入。

| 写法 | 结果 |
| --- | --- |
| `${VAR}` | 使用变量，未设置时报错 |
| `${VAR:-默认}` | 仅未设置时使用默认值，已设置为空则仍为空 |
| `$$` | 字面量 `$`；普通 `$VAR` 不展开 |

数字字段使用 YAML 数字，布尔引用只能展开为 `true` / `false`；duration 使用 `10s`、`5m` 等字符串。

## 基础字段

| 字段 | 默认值 | 用法 |
| --- | --- | --- |
| `log.level` | `info` | `debug` / `info` / `warn` / `error`，可热重载 |
| `log.format` | `text` | `text` / `json`，修改需重启 |
| `runtime.pollInterval` | `10s` | 源地址轮询间隔，> 0 |
| `runtime.stableFor` | `0s` | 新地址持续稳定多久才写入，≥ 0 |
| `runtime.reconcileTimeout` | `30s` | 单项一次同步总时限，> 0；独立列表续期另计同等时限 |
| `runtime.verifyInterval` | `10m` | 全目标周期检查，`0s` 关闭，动态列表仍独立续期 |
| `runtime.syncOnStart` | `true` | 启动时强制检查，仍需源地址可用且稳定 |
| `runtime.maxBackoff` | `5m` | 失败重试最大间隔，不小于 pollInterval |
| `state.path` | `./data/state.json` | 缺失 / null 取默认，显式 `""` 仅内存；自动生成配置指向配置目录的 `state.json` |
| `api.listen` | `127.0.0.1:8080` | `host:port`，端口 1–65535，修改需重启 |
| `api.token` | 空 | 去首尾空白后允许 1–256 个无空格的可打印 ASCII 字符，修改需重启 |

回环地址（如 `127.0.0.1`、`::1`、`localhost`）允许空 token；其他监听严格校验时必填，守护启动可自动生成。

## RouterOS

| 字段 | 默认值 | 用法 |
| --- | --- | --- |
| `routeros.url` | 空 | 有启用项时必填，HTTP(S) 地址，不带 `/rest`、账号、查询串或 fragment |
| `routeros.username` | 空 | 有启用项时必填 |
| `routeros.password` | 空 | 账号密码，可引用 `${ROS_PASSWORD}` |
| `routeros.timeout` | `10s` | 每次请求超时，> 0 |
| `routeros.tls.insecureSkipVerify` | `false` | 跳过 TLS 验证，与非空 caFile 互斥 |
| `routeros.tls.caFile` | 空 | PEM CA 文件，相对进程工作目录 |

没有启用项时 URL、用户名可空，已填值仍需合法；自动生成模板的 URL / 用户名为 `https://192.168.88.1` / `admin`。

## DNS 服务商

`dns.providers` 可为空；每个自定义名称对应一个服务商实例，以下字段位于 `dns.providers.<名称>`。

| 字段 | 默认值 | 用法 |
| --- | --- | --- |
| `type` | 无 | 必填，目前仅 `aliyun` |
| `endpoint` | `alidns.aliyuncs.com` | API 主机名 |
| `regionId` | `cn-hangzhou` | 区域 ID |
| `accessKeyId` / `accessKeySecret` | 空 | 可先留空，同步时需要有效 AK |
| `securityToken` | 空 | STS 临时凭据配套 token，不自动刷新 |
| `timeout` | `10s` | 单次请求超时，> 0 |
| `qps` | `5` | 实例共享请求限速，整数 ≥ 1 |

已定义但未使用的 provider 也需通过字段校验；AK 错误在同步时报告，配置校验不验证云端权限。

## 同步项目

`items` 可为空或全禁用；表内字段相对于 `items[]`，启用项至少配置一个域名或地址列表目标。

| 字段 | 默认值 | 用法 |
| --- | --- | --- |
| `name` | 无 | 全部项目中唯一，匹配 `^[a-z0-9][a-z0-9_-]{0,63}$` |
| `disabled` | `false` | 禁用草稿只校验 name，类型 / 解码错误仍拒绝 |
| `source.interface` | 无 | RouterOS 实际持有源地址的接口名 |
| `source.family` | 无 | `ipv4` → A，`ipv6` → AAAA |
| `source.eui64-mac` | 空 | 仅 IPv6，48 位非全零单播 MAC，取源地址前 64 位合成 |
| `source.select.scope` | `public` | `public` 排除私网 / CGNAT；`any` 允许私网，仍排除回环、链路本地等无效候选 |
| `source.select.prefix` | 空 | 可选同地址族 CIDR 过滤 |
| `source.select.index` | `0` | 候选按 IP 升序排列后的索引，整数 ≥ 0 |
| `domains[].provider` | 无 | 引用 provider 名称 |
| `domains[].domain` | 无 | 托管域，如 `example.com` |
| `domains[].rr` | 无 | 相对记录名，如 `home`，根域用 `@`，支持 `*` |
| `domains[].ttl` | `600` | 秒，整数 1–86400，云端还受套餐限制 |
| `domains[].line` | `default` | 线路 API 标识，区分大小写，空值取默认 |
| `addressList.list` | 无 | RouterOS 列表名，省略整个 addressList 则不维护列表 |
| `addressList.timeout` | 空 | 空为静态，正期限为动态；支持 `1h30m`、`00:10:00`、`"3600"`，最大 `35w3d13h13m56s` |
| `addressList.comment` | `ros-ddns:<name>` | 精确匹配受管条目，其他 comment 不接管；外部启用的同主机条目可满足目标 |

- 目标冲突只检查启用项：DNS 用 provider + domain + rr + 类型 + line（不含 TTL），列表用地址族 + list + comment。
- 动态受管列表约在期限半程删除重建续期，期间可能短暂缺项；静态项不续期。
- `timeout: 0s` 可加载但无法安排有效的到期前续期；静态用空字符串，动态用正期限。
- 配置、`.bak`、state 与 lock 路径不得重合，启动修复后仍冲突会拒绝；相对路径及锁用法见 [部署指南](deployment.md)。

## 最小 DNS 示例

在 [README 基础配置](../README.md) 后追加，并向进程注入 AK 环境变量：

```yaml
dns:
  providers:
    ali:
      type: aliyun
      accessKeyId: ${ALI_ACCESS_KEY_ID}
      accessKeySecret: ${ALI_ACCESS_KEY_SECRET}
items:
  - name: home
    source: {interface: pppoe-out1, family: ipv4}
    domains:
      - {provider: ali, domain: example.com, rr: home}
```

## 仅 address-list 示例

在基础配置后追加，无需 `dns`；私网地址使用 `scope: any`：

```yaml
items:
  - name: lan
    source: {interface: bridge, family: ipv4, select: {scope: any}}
    addressList: {list: ddns-lan, timeout: ""}
```

账号准备与排障见 [RouterOS](routeros-setup.md) 和 [阿里云](aliyun-setup.md)。
