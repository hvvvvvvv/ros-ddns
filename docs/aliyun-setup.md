# 阿里云解析（Alidns）配置指南

本页说明 RAM 凭据、记录匹配、TTL、线路、限流和最终 CLI / 状态检查。真实账号和 DNS 检查都是需要在自己的环境执行的手工步骤，不代表已经完成云端验收。

## 1. 前提条件

域名须在对应账号的阿里云解析中托管，NS 指向实际分配的权威服务器。运行 ros-ddns 的机器需要访问 `alidns.aliyuncs.com` 的 HTTPS API，并已具备 [RouterOS 读取条件](routeros-setup.md)。

配置的 `domain` 是托管域，`rr` 是相对记录名，例如 `example.com` + `home` 得到 `home.example.com`，`rr: "@"` 表示域本身。记录类型由 `source.family` 自动派生，IPv4 是 A，IPv6 是 AAAA。

## 2. RAM 用户与权限

为程序创建专用 RAM 用户和 OpenAPI 凭据，避免使用主账号 AK；程序不需要控制台登录。代码仅使用以下三个 action：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "alidns:DescribeSubDomainRecords",
        "alidns:AddDomainRecord",
        "alidns:UpdateDomainRecord"
      ],
      "Resource": "*"
    }
  ]
}
```

这是按操作收敛的示例，`Resource: "*"` 仍覆盖可操作的全部资源。需要限制到域名时，按每个 action 的资源定义设置并手工验证，不应假定查询与更新使用完全相同的资源标识。[Alidns RAM 授权信息](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-ram)

策略中的 `Version` 属于 RAM JSON，ros-ddns YAML 没有 version。程序不调用 `DeleteDomainRecord`、`SetDomainRecordStatus` 或 `DescribeDomainRecords`，无需为它授予这些操作。

STS 场景须配套提供 AccessKey ID、Secret 和 securityToken。程序使用加载时的凭据，不负责获取或自动刷新 STS；过期前更新部署凭据并重启进程。没有 STS 时保持 securityToken 为空。

## 3. 环境变量、配置与 CLI

首次启动生成的默认配置不含 DNS 服务商或同步项目，`dns.providers` 与 `items` 为空时守护进程仍可正常启动。打开 WebUI，在 Config 页添加 Aliyun 服务商并填写凭据，再到「同步项目」页添加接口与域名等信息，并保存应用。

手工配置仍支持 `${VAR}` / `${VAR:-默认值}` 环境变量引用。以下凭据变量仅在配置中显式引用时读取；可配合后面的手写配置片段使用。程序不加载 `.env`，请由 shell 或进程管理器注入：

```powershell
$env:ALI_ACCESS_KEY_ID = '替换为 AccessKey ID'
$env:ALI_ACCESS_KEY_SECRET = '替换为 AccessKey Secret'
# STS 时另设 $env:ALI_SECURITY_TOKEN
# 完整示例还需 ROS_PASSWORD，可设置 ROS_DDNS_API_TOKEN
```

```bash
export ALI_ACCESS_KEY_ID='替换为 AccessKey ID'
export ALI_ACCESS_KEY_SECRET='替换为 AccessKey Secret'
# STS 时另设 ALI_SECURITY_TOKEN
```

以下是与上述环境变量配套的手写配置片段，需与已有 `routeros` 合并；`ali-main` 是此示例自定义的 provider 名：

```yaml
dns:
  providers:
    ali-main:
      type: aliyun
      endpoint: alidns.aliyuncs.com
      regionId: cn-hangzhou
      accessKeyId: ${ALI_ACCESS_KEY_ID}
      accessKeySecret: ${ALI_ACCESS_KEY_SECRET}
      securityToken: ${ALI_SECURITY_TOKEN:-}
      timeout: 10s
      qps: 5
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

缺省 endpoint、regionId、timeout、qps 分别是 `alidns.aliyuncs.com`、`cn-hangzhou`、`10s`、`5`。endpoint / regionId 去首尾空白后为空也会取默认值；显式 `timeout: 0s`、负 timeout、`qps: 0` 或负 qps 都无效。缺省 TTL 为 600，显式 `ttl: 0` 无效。

provider 名是自定义实例名，多项引用同名实例共享客户端和限速。AK / Secret 允许为空：缺失或错误不会阻止守护进程启动，只在同步时记录错误并退避重试。未被引用的实例仍检查 type / timeout / qps 等，但不创建客户端。当前 type 只支持 aliyun，`domains[]` 不能填写记录 type。

环境变量在 YAML 解析后对标量展开，注释不展开；空格、`#`、冒号、引号和换行保留为字符串内容。已设置的空变量覆盖默认值，未设置的必需引用报错；数字字段必须用 YAML 字面量，布尔字段若用环境引用则展开值必须是 `true` / `false`。见 [完整配置规则](configuration.md#2-环境变量展开)。

在 README 所述构建完成后执行：

```powershell
.\scripts\go.cmd run ./cmd/ros-ddns -config build/configs/config.local.yaml -check
```

Linux 对已构建程序使用：

```bash
./build/bin/ros-ddns -config build/configs/config.local.yaml -check
```

`-check` 输出人类可读的文本摘要，不连接云端，也不创建或修改配置文件；它不能验证 AK、RAM 权限或阿里云套餐是否接受 TTL / line。`-once` 与 `-dry-run` 已移除：需要验证真实读取/写入时启动守护进程，并使用测试记录观察 WebUI 状态、日志与通知。

准备好测试目标后启动守护进程；运行时会实际对账，必要时新增 / 更新 DNS。正 stableFor 时需等待后续成功观察。条目错误与重试留在服务内，通过 WebUI、日志、通知和 status 的 phase / lastError 判断结果。

```powershell
.\build\bin\ros-ddns.exe -config build/configs/config.local.yaml
```

守护进程运行后，在另一终端查询或触发：

```bash
curl -sS http://127.0.0.1:8080/api/v1/status \
  -H "Authorization: Bearer $ROS_DDNS_API_TOKEN"
curl -sS -X POST http://127.0.0.1:8080/api/v1/items/home-ipv4/sync \
  -H "Authorization: Bearer $ROS_DDNS_API_TOKEN"
```

PowerShell 使用 `curl.exe` 和 `$env:ROS_DDNS_API_TOKEN`。未配置 token 时可省略认证头。检查 `items[].recordValue` 及 `domains[]` 中的 provider、fqdn、type、value、recordId、ttl、line、updatedAt、lastError。手动 sync 的 `202` 仅表示排队；ready 只证明源轮询曾成功，不能替代 DNS 成功。完整字段见 [状态结构](architecture.md#10-http-api-与状态结构)。

## 4. Upsert 与多义保护

Provider 按托管域、FQDN 和类型分页调用 `DescribeSubDomainRecords`，每页 100 条，读完后在本地按 RR、type、line 匹配。RR / type 不区分大小写，line 精确比较。

| 请求线路内的匹配结果 | 行为 |
| --- | --- |
| 无匹配 | `AddDomainRecord`，使用期望值、TTL、line |
| 唯一匹配且规范化 IP、TTL、line 一致 | 返回现有 RecordId，不写 |
| 唯一匹配但值 / TTL 不同 | `UpdateDomainRecord` 更新该条 |
| 多条匹配，缓存 RecordId 命中其中一条 | 仅比较并按需更新命中项，其他条目保留 |
| 多条匹配，无有效 RecordId 命中 | 返回多义错误，拒绝新增或更新 |
| 分页不完整、匹配记录缺少 ID 等 | 拒绝写入 |

RecordId 是查询之后消解多义的提示，不能跳过 RR / 类型 / 线路筛选。唯一记录可以在缓存丢失后重新发现；多条匹配不能保证无缓存恢复，需要人工处理。

DNS 状态为 `DISABLE` 时会告警，但程序不调用启用 / 禁用 API；值或 TTL 不一致仍可更新。因此 `phase: synced` 不等于云端记录已启用，验证解析时要另查启用状态。

常规对账中，同项各域名依序执行，后续还有可选列表，共用 reconcileTimeout；独立列表续期另有自己的同等时限。某目标失败不会回滚之前的成功项，普通重试跳过已成功目标。删除配置、禁用项、修改 RR / domain / provider / line 都没有自动删除或孤儿清理，旧 DNS 记录可能保留。

## 5. TTL 与套餐限制

缺省 `ttl: 600`，本地校验允许显式整数 1–86400 秒；显式零或负值会失败。阿里云免费版 / 个人版最低 TTL 为 600 秒，其他套餐能力需在账号中核对；本地通过的较小 TTL 不代表云端接受。[阿里云 TTL 说明](https://help.aliyun.com/zh/dns/pubz-how-to-modify-ttl-time)

pollInterval、stableFor、请求耗时和退避决定何时更新权威记录；递归 DNS 和客户端缓存另外受旧 TTL 及自身策略影响。降低轮询间隔不能清除已有 DNS 缓存，也不能保证固定的全网生效上限。

递归查询显示的是剩余 TTL，不必等于配置值。验证配置 TTL 应对照控制台或权威查询；权威更新后，递归仍可能返回旧值。[TTL 缓存机制](https://help.aliyun.com/zh/dns/pubz-how-to-modify-ttl-time)

## 6. line 是记录身份

缺省及 trim 后空 line 使用 `default`；其他线路要填写账号支持的 API 标识，配置校验不会探测套餐。line 区分大小写。

例如将测试绑定从 default 改到账号支持的 telecom：程序查找相同 RR / 类型的 telecom 记录。已有唯一匹配则按需更新，没有匹配则新增；default 旧记录保留，即使缓存里有它的 RecordId，也不能拿该 ID 把另一线路迁移过来。云端手工把旧记录改到别的线路后，下次对账也遵守这条规则。

静态重复键是 provider + domain + rr + 派生类型 + line，启用目标重复会拒绝加载。不同 provider 名即使使用同一账号也不会自动识别为相同远端，需避免多个实例争用记录。

## 7. 限流、重试与请求预算

| 层次 | 行为 |
| --- | --- |
| 实例限速 | 默认 qps=5，突发容量 1；查询、分页、新增、更新、重试全部计入，同名 provider 的各项共享 |
| 请求内重试 | 最多 3 次尝试含首次；Throttling、5xx、网络错误 / EOF 可重试，等待约 1s、2s，加最多 25% 抖动 |
| SDK | 自动重试关闭，HTTP 请求绑定 context；单次请求受 provider.timeout 约束 |
| 确定错误 | 参数、权限、多义等不在 Provider 请求内即时重试；服务仍按目标退避重新对账 |
| 服务目标退避 | 初始为 `min(5s, maxBackoff)`，翻倍并加最多 10% 正向抖动，总延迟不超过 maxBackoff；新值或强制请求重置 |
| 对账总时限 | 默认 reconcileTimeout=30s，常规对账的所有域名、列表及限流等待共用，后续目标不会另获完整预算；独立列表续期另计 |

本地限速不替代账号级配额；同一账号拆成多个 provider 会产生多个限速器，需控制总量。持续的权限、多义或套餐错误需要处理根因，重试不能自行消除它们。

源值不变的普通轮询不调用已确认 DNS 的 Ensure；默认每 10m 周期强制验证会查询。列表动态续期由独立的列表专用 worker 执行，从不调用 DNS provider，也不等待 DNS Ensure 或退避；续期失败使用独立退避重试，不改变 DNS 重试时机。`verifyInterval: 0s` 会关闭 DNS 的周期漂移检查，但仍保留新值同步、手动强制、失败重试及列表独立续期。

## 8. 验证 DNS 解析

在控制台核对托管域、RR、A / AAAA、值、TTL、line 和启用状态。EUI-64 绑定用派生值核对，并另外确认实际可达性。

```bash
# 递归查询
dig A home.example.com @223.5.5.5 +noall +answer
dig AAAA home.example.com @223.5.5.5 +noall +answer
# 找到实际权威 NS
dig +short NS example.com @223.5.5.5
# 用上一步结果替换 ns1.example.net
dig A home.example.com @ns1.example.net +noall +answer
dig AAAA home.example.com @ns1.example.net +noall +answer
```

Windows 可执行：

```powershell
nslookup -type=A home.example.com 223.5.5.5
nslookup -type=AAAA home.example.com 223.5.5.5
```

递归旧值先对照权威，再观察缓存到期；空结果检查 NS、记录状态和 RR 拼接。多条记录或线路可能返回多个地址，不能仅凭一次查询判断唯一绑定是否正确。

## 9. 手工端到端验证清单

使用独立测试记录和对应状态路径，以下为预期行为而非测试通过记录。

1. 核对实际 NS、托管账号、专用 RAM 用户及三个 action 的资源范围。
2. 执行 `-check` 确认摘要与结构；启动守护进程后从 WebUI / status 查看基础地址、派生值和错误（该步骤会读取 RouterOS 并可能写 DNS，请使用测试记录）。
3. 持续运行时等源稳定后检查 status 的每个 domain 结果，再核对控制台中的值、TTL、line、启用状态。
4. 先问权威再查递归；递归 TTL 是剩余时间，不要求每次都等于 600。
5. 在可接受的维护窗口改变源地址；新稳定值更新同线路的已有唯一记录，无需额外新增，随后观察递归缓存变化。
6. 保留缓存并重启，或用 POST sync 强制检查；唯一且一致的记录不重复添加，`202` 后还要查看实际结果。
7. 在测试绑定切换受支持线路；新线路无匹配时新增，旧线路保留，不能期待自动迁移。
8. 为没有缓存的测试绑定预建两条同 RR / 类型 / line 的记录，应报告多义且拒绝覆盖。使用独立测试状态路径，不要删除生产缓存来制造条件。
9. 另一测试绑定先成功同步并保留 RecordId，再添加第二条同身份记录；强制验证可以选择缓存 ID 指向的原记录，另一条保留。
10. 模拟一个绑定失败，核对其他成功目标保留；修正权限或参数后观察重试清除错误。需要时检查 reconcileTimeout 是否被限流 / 分页耗尽。
11. 禁用或移除测试绑定（若仍保留同步项，至少启用一项；也可移除所有同步项），旧云端记录仍在；手工清理测试数据。

## 10. 常见问题

| 现象 | 检查方向 |
| --- | --- |
| AccessKey / Signature / 权限错误 | ID、Secret、STS 配套和过期时间、系统时间、RAM action / 资源范围 |
| TTL / line 拒绝 | 本地校验不检查套餐；免费 / 个人版 TTL 至少 600，使用账号支持线路 |
| 多义记录 | 检查当前线路 RR / 类型的全部记录及缓存 ID，确认归属后人工整理 |
| 换 line 后新增记录 | 新线路无匹配即新增，旧线路不会迁移或删除 |
| Throttling | 核对账号内各实例总请求量、qps、分页和 verifyInterval |
| 权威已更新、递归仍旧 | 检查缓存、线路和启用状态 |
| recordValue 与 domain.value 不同 | 可能正在 stableFor、目标失败或缓存仍是旧确认值，结合 phase / lastError / 时间判断 |
| DISABLE 仍存在 | 程序不修改启用状态，人工确认后处理 |
| 后续目标超时 | 同项共用 reconcileTimeout，限流、分页和重试都占预算 |

## 11. 凭据与状态

凭据可在 WebUI 中填写并保存到配置，也可在手写配置中引用由进程环境或部署密钥注入的变量，避免提交真实 AK、RouterOS 密码或 API token。配置接受普通字符串，但环境展开能保留特殊字符，并对展开后的错误清洗。STS 需要自行管理更新和进程重启。

Summary 不显示密钥值；服务对错误中的已配置密码、API token、AK / STS 做脱敏，覆盖原值、JSON 字符串转义及 URL query 转义表示，长匹配值按子串、短匹配值按有边界 token 处理。这覆盖配置凭据的上述表示，不保证任意上游内容或这些变体以外的编码形式都无秘密。正常日志与状态仍可能含域名、IP、RecordId。

默认状态路径为 `./data/state.json`，相对进程工作目录，空路径只存内存。缓存不设置 provider 凭据字段，但保存 RecordId、确认值及已处理错误；限制文件、目录和日志权限。多实例使用独立路径。状态缺失会失去多义提示，不能把删除缓存当作通用修复。

API 默认 `127.0.0.1:8080`；非回环监听必须有非空 token，`/api/v1/*` 受 Bearer 保护，探针公开。通过 API 输出分享排障信息前，仍应检查域名、地址、RecordId 和路径的可见范围。
