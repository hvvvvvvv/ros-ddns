# 配置参考

本页对应最终配置加载器与校验代码，完整双栈示例见 [`configs/config.example.yaml`](../configs/config.example.yaml)。CLI 用法见 [README](../README.md#cli-合同)。

## 1. 解析与默认值

加载顺序为：解析单份 YAML 文档 → 展开标量中的环境引用 → 严格解码 → 默认值及规范化 → 语义校验。空文件、多文档、重复键和未知字段会失败；嵌套对象、别名及合并键也受严格解析约束。

配置没有 `version`，没有条目级或 `domains[]` 级记录 `type`。`source.family: ipv4` 派生 A，`ipv6` 派生 AAAA；`dns.providers.<名字>.type` 则是 provider 类型。格式演进通过严格解析体现，升级后需重新执行 `-check`，不会静默忽略旧字段。

表中的“缺省”指没有提供字段；明确写出的零值不等于缺省。Go duration、TTL、QPS 字段写 YAML `null` 时沿用缺省处理，写 `0s` / `0` 时则按显式值校验。`pollInterval`、`reconcileTimeout`、RouterOS / provider `timeout` 必须大于零；`stableFor` 和 `verifyInterval` 允许零。`maxBackoff` 必须不小于正的 `pollInterval`。

Go duration 使用 `time.ParseDuration`，例如 `250ms`、`10s`、`1h30m`；不支持 `1d`，普通数字不能表示秒数。解析器可将 `0` 解析为零时长，但仍受上述零值规则约束。`addressList.timeout` 是单独的 RouterOS 格式，见 [列表字段](#38-itemsaddresslist)。

配置校验不连接 RouterOS / 阿里云，不读取 `caFile` 的内容，也不检查状态目录是否可写。运行 `-check` 成功只代表静态配置通过；它向 stdout 输出人类可读的 Summary，凭据只显示 `set` / `unset`，不是 JSON。

## 2. 环境变量展开

先解析 YAML，再展开标量内容（包括标量键）；注释不参与展开。展开后的标量始终为字符串，不会再次按 YAML 语法解释，也不会再次展开其内部的环境引用。

| 写法 | 行为 |
| --- | --- |
| `${VAR}` | 变量未设置则报错；已设置则使用其值 |
| `${VAR:-default}` | 仅在变量未设置时使用 `default` |
| `${VAR:-}` | 变量未设置时使用空字符串 |
| `$$` | 一个字面 `$`，例如 `$${VAR}` 得到字面 `${VAR}` |
| `$VAR` 或其他裸 `$` | 保留原样 |

变量名必须由字母或 `_` 开头，后接字母、数字或 `_`；非法名字或未闭合的 `${...` 会报错。已设置的空变量也优先于 `default`，与 shell 的 `:-` 用法不同。空值之后仍须通过字段自身的非空要求。

```yaml
routeros:
  password: ${ROS_PASSWORD}
api:
  token: ${ROS_DDNS_API_TOKEN:-}
# ${NOT_SET} 留在注释中，不会触发缺失变量错误
```

以上是片段。环境值中的空格、`#`、`: `、引号、反斜杠和换行会保留在字符串中，不会注入新的 YAML 字段；不必为了这些内容手动做 YAML 转义。不过字段规范化仍会处理指定字段的首尾空白，例如 `api.token` 会 trim，而 RouterOS 密码、AccessKey ID / Secret、STS token 保留原值。

数字与布尔字段必须写 YAML 字面量，例如 `qps: 5`、`ttl: 600`、`syncOnStart: true`；不能用 `${QPS}` 或 `${SYNC_ON_START}` 让字符串自动转型，显式 YAML 类型标签也不能改变展开结果。duration 本身是字符串，可以通过环境变量提供。

展开、解码和语义校验错误会清洗已展开值及相关诊断表示，尽量保留字段位置和错误原因。程序不自动读取 `.env`；环境变量由 shell、服务管理器或部署系统传入。以 OS 服务方式运行时进程不继承 shell 环境：含无默认值 `${VAR}` 的配置在服务启动时会加载失败（原文件会被保留为 `.invalid-<时间戳>`），应使用字面值或在 WebUI 中配置；开启 `-service` 前也建议先用 `-check` 配合相同环境验证。

## 3. 完整字段参考

### 3.1 根对象

| 字段 | 类型 | 必填 / 缺省 | 约束 |
| --- | --- | --- | --- |
| `log` | object | 否；使用子字段默认值 | 日志设置 |
| `runtime` | object | 否；使用子字段默认值 | 调度、超时与退避 |
| `state` | object | 否；使用子字段默认值 | 状态缓存路径 |
| `api` | object | 否；使用子字段默认值 | 守护进程 HTTP API |
| `routeros` | object | 是 | 单个连接，至少提供 URL 和用户名 |
| `dns` | object | 否；providers 为空 | 仅维护列表时可省略 |
| `dns.providers` | `map<string, object>` | 否；空 map | 自定义名字供域名绑定引用；每个已定义实例都校验 |
| `items` | `array<object>` | 是；无默认项 | 至少一项，且至少一项启用 |

### 3.2 `log`、`runtime`、`state`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `log.level` | string | 否 | `info` | `debug` / `info` / `warn` / `error`；空字符串无效 |
| `log.format` | string | 否 | `text` | `text` / `json`；空字符串无效；日志写 stderr |
| `runtime.pollInterval` | duration 字符串 | 否 | `10s` | 必须 > 0；显式零或负值无效 |
| `runtime.stableFor` | duration 字符串 | 否 | `0s` | 必须 ≥ 0；零表示首次观察即可接受新值，正值在后续成功轮询时判定 |
| `runtime.reconcileTimeout` | duration 字符串 | 否 | `30s` | 必须 > 0；单项常规 DNS + 列表对账共用总时限，独立列表续期另有同样时限；零无效 |
| `runtime.verifyInterval` | duration 字符串 | 否 | `10m` | 必须 ≥ 0；零关闭周期全目标校验，不关闭动态列表独立续期 |
| `runtime.syncOnStart` | bool | 否 | `true` | `false` 关闭启动强制全目标重查；无缓存 / 失效目标和新值仍会同步，恢复的列表缓存仍须重读 |
| `runtime.maxBackoff` | duration 字符串 | 否 | `5m` | 必须 ≥ `pollInterval`；显式零或负值无效；源及目标退避上限 |
| `state.path` | string | 否 | `./data/state.json` | trim 后空字符串表示仅内存；相对路径按进程工作目录解析 |

所有 duration 的显式空字符串都无法解析。状态不是远端事实来源；缓存恢复不建立 ready。所有恢复的列表缓存均标记待重查；即使同时设置 `syncOnStart: false` 和 `verifyInterval: 0s`，首次获得可用稳定源值的轮询仍须触发列表重读，确认后才能视为 synced，并按实际条目重新安排必要的续期 / 复查。守护进程会读取并写入状态缓存，并写入单实例锁诊断文件。文件损坏或内部版本不兼容时告警并忽略；其他读取错误会使初始化失败。状态保存失败保留内存和远端成功结果，并在条目错误中显示。细节见 [状态持久化](architecture.md#9-状态持久化)。

锁不属于 YAML schema：CLI `-lock-file` 可覆盖默认的 `state.path + ".lock"`；空状态路径仍使用基于规范配置路径（绝对路径、解析符号链接，Windows 统一大小写）的临时锁。配置、`.bak`、非空 `state.path` 与有效锁路径不得指向同一文件；启动和 `-check` 遇到冲突返回 `2`，WebUI 保存返回 `422`。`-check` 不获取锁；热重载 `state.path` 不会移动本次进程已经持有的锁。精确规则及部署权限见 [单实例锁](deployment.md#1-单实例锁)。

### 3.3 `api`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `listen` | string | 否 | `127.0.0.1:8080` | `host:port`，IPv6 写作 `[::1]:8080`；端口仅十进制数字且为 1–65535；空值、端口 0、服务名、符号和端口空白均无效 |
| `token` | string | 条件必填 | 空字符串 | trim 后非空才启用 Bearer 认证；非回环监听必须非空 |

`localhost`（不区分大小写）和字面回环 IP 视为回环；其他主机名不通过 DNS 查询判定是否回环。空 host（`:8080`）、`0.0.0.0:8080`、`[::]:8080` 都要求 token。`listen: ""` 不能关闭 API；主机能否解析、地址能否绑定仍在运行时确定。

设置 token 后保护 `/api/v1` 和 `/api/v1/*`，健康与 ready 探针公开。守护进程先同步绑定端口，再运行服务；绑定失败退出 `1`，不会开始 RouterOS 轮询。`-check` 不绑定 API，但仍校验 API 配置。API 自身提供 HTTP，远程使用可放在 HTTPS 代理之后。

同一监听的 `/` 提供 [WebUI 表单编辑器](webui.md#config仅表单编辑)，没有 YAML 文本编辑模式。保存会保留 `${ENV}` 引用、规范化 YAML（可能丢失注释 / 锚点 / 排版），备份原文件到 `.bak`，并用 `baseHash` 检测外部修改（冲突返回 `409`）。RouterOS、providers、items、runtime、`state.path`、`log.level` 可热重载；`api.listen`、`api.token`、`log.format` 为只读，修改需重启。表单中的 API token 只显示设置状态，配置 GET 对 `api.token` 返回掩码；其他字面量凭据仍会出现在配置响应中，建议使用环境引用；详见 [保存与安全边界](webui.md)。

### 3.4 `routeros`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `url` | string | 是 | 无 | `http` / `https` 且 hostname 非空；可选端口须为十进制 1–65535；只允许 origin 加可选 base path，禁止 userinfo、查询串、fragment、末尾 `/rest` |
| `username` | string | 是 | 无 | trim 后非空，建议专用用户 |
| `password` | string | 否 | 空字符串 | 静态校验允许空；是否能认证由设备决定；保留原值 |
| `timeout` | duration 字符串 | 否 | `10s` | 必须 > 0；显式零、负值或空字符串无效；单次 HTTP 请求 / 源轮询超时 |
| `tls` | object | 否 | 使用子字段默认值 | HTTPS 信任设置 |
| `tls.insecureSkipVerify` | bool | 否 | `false` | `true` 跳过证书验证，与非空 `caFile` 互斥 |
| `tls.caFile` | string | 否 | 空字符串 | PEM CA 文件，加入系统信任池；相对进程工作目录；非空时在客户端初始化读取 |

URL 会去首尾空白和末尾 `/`。例如 `https://router.example.net/base` 的 REST 前缀是 `/base/rest`；不要在配置中填写 `https://user:pass@host`、`https://host/rest`、`https://host?token=...` 或 `https://host#...`，空查询分隔符 `?` / 空 fragment `#` 也被拒绝。端口可以省略，但显式空端口、服务名或非十进制内容无效；`http://:80`、`https://host:0`、`https://host:65536` 和空 IPv6 主机均被拒绝。

代码默认验证 TLS；仓库示例显式跳过验证，复制后应按 [RouterOS 指南](routeros-setup.md#2-启用-rest证书与-www-ssl) 调整。`-check` 不验证 CA 文件存在或证书内容。

### 3.5 `dns.providers.<名字>`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `type` | string | 是 | 无 | 当前只接受 `aliyun` |
| `endpoint` | string | 否 | `alidns.aliyuncs.com` | trim 后为空也使用默认；静态校验不检查 endpoint 可达性 |
| `regionId` | string | 否 | `cn-hangzhou` | trim 后为空也使用默认 |
| `accessKeyId` | string | 否 | 空字符串 | 允许为空；缺失或错误不阻止启动，只在同步时记录错误并重试；保留原值 |
| `accessKeySecret` | string | 否 | 空字符串 | 允许为空；缺失或错误不阻止启动，只在同步时记录错误并重试；保留原值 |
| `securityToken` | string | 否 | 空字符串 | STS 配套 token；保留原值；程序不获取或自动刷新凭据 |
| `timeout` | duration 字符串 | 否 | `10s` | 必须 > 0；显式零、负值或空字符串无效；单次 API 请求时限 |
| `qps` | int | 否 | `5` | 必须 ≥ 1；显式零、负值无效；同实例查询、分页、写入和重试共享限速，突发容量 1 |

未被启用项引用的 provider 仍校验 type、timeout、qps 等，但不创建客户端。AK / Secret 允许为空：守护进程照常启动，DNS 请求在同步阶段失败并记录日志与页内通知，按退避重试；修复凭据（WebUI 保存热重载）后恢复。可以定义多个实例，每个仍须为 Aliyun；不同实例名使用独立限速器，静态检查无法判断它们是否指向同一账号。

### 3.6 `items[]` 与 `source`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `name` | string | 是 | 无 | trim 后全局唯一，匹配 `^[a-z0-9][a-z0-9_-]{0,63}$`；不会自动转小写 |
| `disabled` | bool | 否 | `false` | `true` 不参与运行或启用目标冲突检查；仍须满足所有结构和字段校验 |
| `source` | object | 是 | 无 | 地址来源 |
| `domains` | `array<object>` | 条件必填 | 空数组 | 与非空 `addressList` 对象至少配置一个；空数组本身不算目标 |
| `addressList` | object | 条件必填 | 无 | 一个可选列表目标；空对象会因缺少 list 而失败 |
| `source.interface` | string | 是 | 无 | trim 后非空，精确匹配 RouterOS 的 interface 或 actual-interface |
| `source.family` | string | 是 | 无 | `ipv4` / `ipv6`，派生 A / AAAA |
| `source.eui64-mac` | string | 否 | 空字符串 | 非空仅限 IPv6，必须为 48 位、单播、非全零 MAC |
| `source.select` | object | 否 | 使用子字段默认值 | 候选过滤及选择 |
| `source.select.scope` | string | 否 | `public` | trim / 转小写后为空也用 public；只接受 `public` / `any` |
| `source.select.prefix` | string | 否 | 空字符串 | 非空须为同地址族 CIDR；先掩码规范化，再判断是否包含候选 IP；IPv6 不接受 IPv4-mapped 前缀 |
| `source.select.index` | int | 否 | `0` | 零表示排序后第一个；负值无效；越界在运行时为无合格地址，不自动回退 |

MAC 支持冒号、连字符、点分或连续 12 位十六进制，统一为小写冒号格式；本地管理单播 MAC 可以使用，组播、广播、全零或 64 位 MAC 不可用。非法 MAC 会阻止启动，不回退基础地址。未设置 MAC 时直接使用选中接口 IP；设置后总取选中 IPv6 的前 64 位合成，不要求源掩码恰为 `/64`。

选址规则：

1. 匹配接口并排除 disabled、invalid、非法前缀。
2. 两种 scope 均排除链路本地、回环、组播、未指定地址、IPv4 有限广播和依据源掩码计算的定向广播（`/31`、`/32` 没有定向广播）；IPv6 `link-local` 标志也会排除。
3. `public` 另外排除 IPv4 私网、`100.64.0.0/10`；IPv6 要求 `global=true`、`deprecated=false` 且不是 ULA。`any` 允许私网 / ULA，也允许 deprecated IPv6，但仍遵守前面的排除规则。
4. 按可选 prefix 过滤，按 IP 数值升序排序，再取 index。没有按 dynamic / static 偏好排序，也没有公网连通性探测。

没有候选或 index 越界通常使该项 `waiting` 且 `lastError` 非空：不清空目标，成功源轮询仍计入 ready；监控需检查错误字段而不是只看 phase。

### 3.7 `items[].domains[]`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `provider` | string | 是 | 无 | trim 后必须引用已定义的 provider 名 |
| `domain` | string | 是 | 无 | trim、转小写、去一个末尾 `.`；合法 DNS 标签组成，总长度 ≤ 253 |
| `rr` | string | 是 | 无 | trim / 转小写；允许 `@`、单独的 `*`，或以点分隔的合法标签，如 `home`、`a.b` |
| `ttl` | int | 否 | `600` | 单位秒，显式值范围 1–86400；零与负值无效；云端套餐限制另行检查 |
| `line` | string | 否 | `default` | trim 后空值也用 default；区分大小写，不静态验证线路支持情况 |

普通域名标签匹配 `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`，不接受下划线或直接的非 ASCII 标签；不会自动做 IDNA 转换。除单独 `*` 外，RR 同样遵守标签规则，例如 `*.home` 无效。`rr: "@"` 的 FQDN 是 domain，其余为 `rr.domain`；组合长度也不得超过 253。配置校验不验证域名归属或实际 NS。

本地 TTL 允许 1–86400；阿里云免费版 / 个人版最低 600 秒，见 [套餐说明](aliyun-setup.md#5-ttl-与套餐限制)。line 是目标身份，更改线路会在新线路查找或新增，旧线路不会迁移或删除。

### 3.8 `items[].addressList`

| 字段 | 类型 | 必填 | 缺省值 | 显式值规则与含义 |
| --- | --- | --- | --- | --- |
| `list` | string | 是 | 无 | trim 后非空；按 family 写 IPv4 或 IPv6 防火墙列表 |
| `timeout` | string | 否 | 空字符串 | trim 后为空表示静态，新增时不发送 timeout；非空按下面的 RouterOS 时间语义校验；允许零时长，但非空零值仍会作为 timeout 发送，不等于静态或默认期限 |
| `comment` | string | 否 | `ros-ddns:<name>` | trim 后为空也用默认值；完全相等才视为受管项 |

非空 timeout 在加载时解析并检查数值范围，不连接设备验证接受情况：

| 形式 | 规则与示例 |
| --- | --- |
| 单位组合 | 非负整数加小写 `w/d/h/m/s`，如 `1h30m`、`1w2d3h4m5s`；不接受 `1.5h`、`1ms` |
| 时钟形式 | `MM:SS` 或 `HH:MM:SS`，如 `10:00`、`00:10:00`；分、秒必须小于 60，小时不限制为两位；秒可有最多 9 位小数，如 `00:01.5` |
| 裸秒数 | 非负十进制整数或最多 9 位小数，如 `3600`、`1.5`；不接受正负号或指数表示 |
| 总时长 | 本程序上限为 `35w3d13h13m56s`（21474836 秒，含上限），超限或溢出失败 |

`01:60`、`60:00`、`00:60:00`、`36w`、`never`、负值或内部空白均无效。空字符串表示静态；`0`、`0s`、`00:00` 等非空零时长允许加载，但服务按动态目标处理，无法据此安排有效的到期前续期。动态期限不大于 `1ns`，或设备返回期限为空 / 无法解析时，会安排有界复查：默认 1 分钟，正的 verifyInterval 更短时使用该间隔。静态条目请用空字符串，需要动态续期时请使用设备支持的正常正期限。

最终列表行为：

- 同 family / list 内按精确 comment 管理；只把相同裸 IP 或匹配的主机路由 `/32`、`/128` 视为目标主机。`203.0.113.0/24` 不满足目标 `203.0.113.0`，IPv6 子网同理。
- 受管旧地址、disabled 条目、静态与动态切换或期限不匹配均先删除，必要时按配置重新添加。静态配置要求返回 `Dynamic=false` 且 timeout 为空；动态配置要求 `Dynamic=true` 且 timeout 匹配，可识别 `1h` 与 `01:00:00` 等等价表示。删除失败阻止本轮新增；新增重复错误 `ros.ErrEntryExists` 也会记为失败，等再次读取确认有效目标后才能 synced。
- 其他 comment 的条目永不修改。外部启用的匹配主机满足存在性，无须重复添加；外部禁用的匹配主机是未解决冲突，使条目 degraded，需人工启用或移除。
- 守护进程通常在配置期限约一半时刷新受管动态条目。每项独立的列表专用 worker 读取并按需删除 / 添加，不调用 DNS，也不等待 DNS Ensure 或退避；同项的列表读取、增删和结果发布共用列表锁。重启后待重查的列表也由该 worker 在源可用后立即读取，不等待启动 DNS 对账完成。续期失败使用独立退避重试，不改变 DNS 重试时机。静态受管条目没有续期定时器。外部 dynamic 匹配项按其返回期限或上述回退间隔复查，但不接管或延长其期限。
- 续期独立于源轮询及 `verifyInterval`；`verifyInterval: 0s` 仍可续期。临近到期时 RouterOS 不可达、源无效或重试耗时，条目可能先到期，恢复后再添加；属于尽力而为。守护进程停止后没有后台续期。

完整生命周期和手工检查见 [RouterOS 指南](routeros-setup.md#6-地址列表生命周期)。

## 4. 跨字段校验与规范化

禁用项仍需合法 name、source、目标和 provider 引用，但不会因为它引用 provider 而要求 AK。所有 name 必须唯一，至少保留一个启用项。

启用目标不能重复，包括同一条目内重复 DNS 绑定：

| 目标 | 身份键 |
| --- | --- |
| DNS | provider + domain + rr + 派生类型 + line |
| 地址列表 | family + list + comment |

TTL 不属于 DNS 身份；改 TTL 会保留 RecordId 提示并使该绑定重新确认。不同 provider 名即使实际使用同一账号也不会被视为重复；多个实例之间应自行避免冲突。

枚举字段转小写；domain / rr 转小写；name、interface、provider 引用、list、comment、prefix、API token、状态路径、CA 路径等去首尾空白。line、接口和名称不会统一转小写。密码和 AK / STS 保留原值。routeros URL 去末尾斜杠，domain 去一个末尾点，MAC 统一格式。

## 5. 配置示例

### 5.1 最小 IPv4 DNS 配置

```yaml
routeros:
  url: https://192.168.88.1
  username: ddns-api
  password: ${ROS_PASSWORD}
dns:
  providers:
    ali-main:
      type: aliyun
      accessKeyId: ${ALI_ACCESS_KEY_ID}
      accessKeySecret: ${ALI_ACCESS_KEY_SECRET}
items:
  - name: home-ipv4
    source:
      interface: pppoe-out1
      family: ipv4
    domains:
      - provider: ali-main
        domain: example.com
        rr: home
        ttl: 600
        line: default
```

此例使用系统信任的 HTTPS 证书；私有 CA 需另外配置 `routeros.tls.caFile`。默认 API 仅回环监听，token 为空；按需求加上 `${ROS_DDNS_API_TOKEN:-}`。

### 5.2 仅维护动态地址列表

下面是可独立加载的配置结构，无需 DNS provider 凭据；地址和接口请替换为实际值。

```yaml
routeros:
  url: https://192.168.88.1
  username: ddns-api
  password: ${ROS_PASSWORD}
runtime:
  verifyInterval: 0s
items:
  - name: home-ipv4
    source: {interface: pppoe-out1, family: ipv4}
    addressList:
      list: ddns-home-ipv4
      timeout: 1h
```

关闭全目标周期校验后，动态受管列表仍由独立定时器约每半个期限刷新。改为 `timeout: ""` 则使用静态条目；必须在实际对账时才能替换已有动态条目。

### 5.3 IPv6、多个域名与 EUI-64

以下仅为 `items` 片段，需已有 `routeros` 和 `ali-main` provider；不要重复粘贴多个根 `items` 键。

```yaml
items:
  - name: home-ipv6
    source:
      interface: bridge
      family: ipv6
      eui64-mac: "00:1A:2B:3C:4D:5E"
      select:
        scope: public
        prefix: ""
        index: 0
    domains:
      - {provider: ali-main, domain: example.com, rr: home, ttl: 600}
      - {provider: ali-main, domain: example.net, rr: "@", ttl: 600}
    addressList:
      list: ddns-home-ipv6
```

多个前缀时按实际网段设置 `select.prefix`，注意固定过滤器可能在运营商换前缀后失配。MAC 可以属于实际目标终端，前提是选中地址的前 64 位属于目标网段，且终端确实使用相应 IID；程序不会验证可达性。算法与测试向量见 [EUI-64 派生](architecture.md#7-eui-64-派生)。

## 6. 凭据与状态边界

推荐用环境变量注入凭据；配置也接受普通字符串。服务错误中的已配置密码、API token、AccessKey ID / Secret、STS token 会脱敏，覆盖原值、JSON 字符串转义及 URL query 转义表示；长匹配值按子串、短匹配值按有边界 token 替换；展开后的配置错误也会清洗。此机制不保证任意上游错误及上述变体以外的编码形式都没有秘密。

默认状态文件 `./data/state.json` 和 API Snapshot 没有凭据字段，仍可能暴露域名、IP、RecordId、接口或路径等部署信息。限制配置、日志、状态文件权限；外部共享前检查内容。状态文件内部有格式版本，与无 version 的用户 YAML 无关。
