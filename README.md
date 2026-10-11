# pvm

个人 QEMU 虚拟机系统：一条命令拿到隔离的 Linux 开发机，可把整个 VM 的流量经你自己的 VPS 出口。

## 特性

- **秒级开发机**：golden 镜像预装开发工具链与桌面组件，Ubuntu / Debian 多版本，`pvm create` 后约 30s 可用
- **出口节点（exit-node）**：整机流量经 WireGuard 隧道从你的 VPS 出海；UDP / TCP 双传输，`pvm exit add` 自动测速选最快；kill-switch fail-closed，隧道断 = VM 断网
- **fake-ip DNS 分流**：代理域名只在 VPS 远端解析，本机与本地网络零可见；内网域名走宿主 DNS，域名/CIDR 规则热生效
- **网页桌面**：VM 原生 XFCE + noVNC，浏览器打开即是 VM 本体（非容器），免密回环访问
- **配套能力**：端口映射、RAM 快照（秒级恢复）、clone（机器码自动重建）、宿主机目录挂载（`/share` 默认 + 自定义）、virtio-serial 兜底 exec（断网/无 sshd 仍可执行命令）

## 安装

要求：macOS (arm64) 或 Linux (amd64)。一键安装（需 `curl`）：

```bash
curl -fsSL https://raw.githubusercontent.com/huangzheng2016/pvm/master/scripts/install.sh | sh
```

默认装到 `~/.local/bin/pvm`（`INSTALL_DIR` 可改），macOS 会附带安装 bundled QEMU 运行时（`PVM_SKIP_QEMU=1` 跳过，回退系统 QEMU）。也可以直接从 [Releases](https://github.com/huangzheng2016/pvm/releases) 下载对应压缩包。

## 快速开始

```bash
pvm daemon status                                  # daemon 未运行会自动拉起
pvm image fetch ubuntu:24.04                       # 下载基础镜像
pvm image build ubuntu:24.04                       # 烘焙 golden（docker/桌面预装，可选但推荐）
pvm create --image ubuntu:24.04 --name dev1 --user dev
pvm ssh dev1                                       # 免密进入
pvm desktop dev1                                   # 浏览器打开 VM 的 XFCE 桌面
pvm map dev1 8080:80                               # 端口映射到宿主回环
```

把整台 VM 切到 VPS 出口（部署 exit 见下文）：

```bash
pvm exit add us --server <vps>:18443 --token <token>   # 默认并行测速 udp/tcp 选最快
pvm route dev1 --via us                                # 时区/locale 自动对齐出口地理
pvm ssh dev1 'curl -s ipinfo.io'                       # 应显示 VPS 的 IP 与时区
pvm route dev1 --via direct                            # 切回本机出口
```

## 部署 exit（VPS 出口端）

在 VPS 上从 Releases 下载对应架构的 tar.gz，以 `pvm exit-agent --listen :18443 --token <token>` 常驻（systemd），本机 `pvm exit add` 注册即可。完整流程（含 systemd unit、指纹核验、回滚）：[pvm-skill/SKILL.md](pvm-skill/SKILL.md) 的「给定 SSH 部署 exit」一节。

## 文档

- [pvm-skill/SKILL.md](pvm-skill/SKILL.md)：完整使用手册（面向 agent，同样适合人读）
- [docs/security-posture.md](docs/security-posture.md)：网络/设备身份的安全与风控姿态
- [AGENTS.md](AGENTS.md)：仓库结构、构建测试与研发规范

## 从源码构建

```bash
./script/build.sh   # 原生构建：guest 二进制 + assetpack + 带嵌入资产的 ./pvm
go test ./...
```
