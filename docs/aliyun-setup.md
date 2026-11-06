# 阿里云 DNS 配置

域名需托管在阿里云解析，NS 指向分配的权威服务器，运行主机可访问 `alidns.aliyuncs.com`。

## 创建 RAM 用户

在 RAM 控制台创建专用用户和 AccessKey，授予以下三个操作（Resource 覆盖账号下全部资源）：

```json
{
  "Version": "1",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "alidns:DescribeSubDomainRecords",
      "alidns:AddDomainRecord",
      "alidns:UpdateDomainRecord"
    ],
    "Resource": "*"
  }]
}
```

## 凭据与配置

向 ros-ddns 进程注入变量（程序不自动加载 `.env`）：

```bash
export ALI_ACCESS_KEY_ID='你的 AccessKey ID'
export ALI_ACCESS_KEY_SECRET='你的 AccessKey Secret'
# 使用 STS 时另设：export ALI_SECURITY_TOKEN='你的 STS token'
```

PowerShell 使用 `$env:ALI_ACCESS_KEY_ID='...'` 等写法；Compose 在 `.env` 添加变量，由 `env_file` 注入。
STS 需要配套 ID、Secret 和 securityToken，程序不自动刷新，过期前更新凭据并重启。

在已有基础配置后添加 provider，也可在 WebUI 的 Config 页填写：

```yaml
dns:
  providers:
    ali:
      type: aliyun
      accessKeyId: ${ALI_ACCESS_KEY_ID}
      accessKeySecret: ${ALI_ACCESS_KEY_SECRET}
      securityToken: ${ALI_SECURITY_TOKEN:-}
```

在 Items 添加域名，`example.com` + `home` 对应 `home.example.com`，根域用 `@`，IPv4 / IPv6 自动对应 A / AAAA。
字段默认值和完整示例见 [配置参考](configuration.md)，AK 可先留空，同步时需有效凭据。

## 记录与查询

记录按需新增或更新（upsert），多义且无有效缓存 RecordId 时需人工清理，TTL / line 由套餐决定，移除配置不自动删除旧记录。
TTL 默认 `600` 秒，line 默认 `default`；修改线路后旧线路记录保留，云端禁用的记录也不会自动启用。

WebUI 查看同步结果，再查询 DNS：

```bash
dig A home.example.com @223.5.5.5 +noall +answer
dig AAAA home.example.com @223.5.5.5 +noall +answer
nslookup -type=A home.example.com 223.5.5.5
nslookup -type=AAAA home.example.com 223.5.5.5
```

递归查询仍为旧值时，用 `dig +short NS example.com` 找到权威服务器，替换上面的 DNS 服务器再查询。

## 排障

| 现象 | 处理 |
| --- | --- |
| AK / 签名 / 权限错误 | 核对 ID、Secret、STS 是否配套及过期，检查主机时间和 RAM 策略 |
| TTL / line 被拒绝 | 使用账号套餐支持的 TTL 和线路 API 标识 |
| 多义记录 | 检查同 RR、类型、线路的重复记录，确认后人工清理，不要随意删除状态缓存 |
| Throttling / 超时 | 降低 qps，检查账号总请求量、网络和 reconcileTimeout |
| 权威已更新，递归仍旧 | 等待缓存到期，检查 NS、线路和记录启用状态 |
