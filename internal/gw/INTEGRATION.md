# gw 包集成说明（fake-ip DNS / rules / policy → pvm gateway）

三个包供 `pvm gateway`（网关 VM 内进程）使用，集成点如下：

## 启动接线

```go
eng, _ := rules.NewEngine(paths.GatewayRulesJSON, rules.DefaultInternal()) // 热增删自动持久化
pool, _ := dns.NewPool("198.18.0.0/16", 65534)
dnsSrv := dns.NewServer(dns.UDPUpstream{Addr: "10.0.2.3:53"}, eng, pool) // 上行 = slirp DNS
dnsSrv.SetStatic("gw.pvm.internal", net.ParseIP(cluster.GatewayIP))
go dnsSrv.ListenAndServe("100.99.x.1:53") // 绑定集群网桥地址
pol := policy.New(eng, pool)
```

## DHCP

DHCP 应答把 DNS 和 default route 都指到 100.99.x.1（网关自己），
这样 VM 的全部 DNS 查询天然到达 dns.Server，无需额外劫持。

## DNS 强制接管（防硬编码 DNS 泄露）

nftables 在网桥上拦截 VM 出方向 53/853，非到网关的统统 DNAT 回网关：

```
nft add rule inet pvm_gw prerouting iifname "br0" ip saddr != 100.99.x.1 \
    udp dport {53, 853} ip daddr != 100.99.x.1 dnat to 100.99.x.1
# tcp 53/853 同理
```

## 数据面分流（fake-ip 反查 → 进隧道）

VM 向 fake-ip 发起连接时，网关 conntrack 目的地址必然在 198.18.0.0/16：
1. `pol.IsFakeIP(dst)` 命中 → `pol.DecideFlow("", dst, vmID)`：
   内部自动 `LookupByFakeIP` 取回域名 → 跑一遍 rules → direct 或 proxy(exit)。
2. direct：正常 SNAT 到上行（slirp）转发。
3. proxy：经对应 exit 的隧道接口策略路由（`ip rule from <vmIP> lookup <exit表>`），
   远端解析在 VPS 端完成——应用层（SOCKS/透明转发）需要域名时用
   `LookupByFakeIP` 的结果作为目标名，而不是假 IP。
4. 非 fake-ip 的直连目标（用户手输 IP）：`pol.DecideFlow("", dst, vmID)` 走
   CIDR 规则 + VM 默认出口。

## 注意

- fake-ip 应答 TTL=5s，AAAA 一律 NODATA 强制回退 A；别缓存映射超过 LRU 生命周期。
- 内网后缀绝不出网关（不查上行、不进隧道），由 `rules.Engine.IsInternal` 保证。
- 规则增删即时生效（RWMutex），但已建立的连接不 retroactive 切换，新连接按新规则。
