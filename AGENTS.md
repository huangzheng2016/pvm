# pvm Agent 指南

与用户同语言回复。本文件只放热路径规则：项目地图、硬约束、工作流。代码是唯一事实来源，注释与文档以代码为准。

## Project Map

单个 Go 二进制按子命令承担多角色：host CLI / daemon（本机控制面）、agent（guest 内）、exit-agent（VPS 上）；`cmd/gateway` 是独立的 guest 网关二进制。数据根目录 `~/.pvm/`。

- `cmd/pvm`: 主二进制入口与 usage；`exit-agent` 子命令运行在 VPS 上。
- `cmd/gateway`: guest 网关二进制入口，由 daemon 推送的 `/etc/pvm/gateway.json` 驱动。
- `cmd/assetpack`: 构建期把 guest 二进制 zstd 压缩并生成 embed manifest。
- `internal/cli`: 全部子命令实现；需要控制面的命令是 daemon 的瘦客户端。
- `internal/daemon`: 常驻控制面，unix socket 上的 HTTP/JSON，拥有全部 VM/cluster 生命周期；单实例 flock。
- `internal/qemu`: QEMU 工具链定位（bundled 优先）、进程/QMP/内存快照。
- `internal/vm`: VM 规格本体：overlay 盘、cloud-init seed、端口分配、磁盘元数据。
- `internal/image`: 基础镜像下载与 golden 镜像烘焙（不可变资产，按版本保留）。
- `internal/cluster`: 集群状态：子网/IP/MAC 分配、端口池，原子写持久化。
- `internal/gateway`: guest 网关代理：网桥、固定映射 DHCP、DNS 转发、nftables NAT、fake-ip 接管与 kill-switch。
- `internal/gw`: host/guest 共享的网关域逻辑（`dns` fake-ip 池、`policy`、`rules`）。
- `internal/exit`: VPS exit-agent：WireGuard 终结、domain-connect 拨号、nft/iptables NAT。
- `internal/tunnel`: UDP/TCP 两种传输上的 WireGuard device 封装。
- `internal/agent`: guest 内 exec agent（单连接、多路并发 exec 会话）。
- `internal/assets`: 内嵌资产解包到 `~/.pvm/runtime`，SHA256 校验；仅 `-tags embedassets` 编入。
- `internal/native`: 架构选择的唯一事实来源。
- `internal/paths`: `~/.pvm` 目录布局。
- `internal/keys`: pvm 专用 SSH keypair，绝不触碰 `~/.ssh`。
- `internal/config`: 加载 `~/.pvm/config.yaml`。
- `internal/fsutil`: `AtomicWrite`（tmp+fsync+rename）与文件锁。
- `internal/buildinfo`: 版本号，构建期 ldflags 注入。
- `script/build.sh`: 原生构建入口；`script/qemu/`: bundled QEMU 运行时组装与补丁。
- `pvm-skill/`: 面向 agent 的 pvm 使用 skill；`docs/`: 设计文档（安全姿态等）。

## Build & Test

- `./script/build.sh`：全流程原生构建——linux/$HOSTARCH guest 二进制（gateway+agent）→ assetpack → 带 `embedassets` 的 host `pvm`；macOS 上默认追加 bundled qemu（`PVM_PACK_QEMU=0` 跳过）。产物 `./pvm` 约 19MB。
- 提交前 `go test ./...` 与 `go vet ./...` 必须全绿；改并发/状态路径时对相关包加 `-race` 复跑。
- 只构建宿主原生架构，不做交叉构建发布。

## Hard Constraints

1. 仅支持宿主原生架构（darwin/arm64、linux/amd64）。`internal/native` 是架构事实的唯一来源，镜像、guest 二进制、QEMU target 全部由它派生；禁止重新引入跨架构选择、模拟或回退。
2. 仓库公开：永不提交真实 IP/域名/token/证书指纹/个人路径。公开文本与测试数据一律用占位符——TEST-NET（`203.0.113.0/24`）、`example.com`、`YOUR_TOKEN`；提交前 grep diff 自查。
3. 状态文件必须原子写（tmp+fsync+rename，走 `fsutil.AtomicWrite`）；daemon 单实例 flock；guest 侧规则/命令输入一律白名单校验。
4. 流量泄漏与身份混同是最高严重度：kill-switch 任何故障路径必须 fail-closed（隧道断 = VM 断网，不得回退本地出口）；不要把宿主 `~/.ssh`、`~/.gitconfig` 传入 VM；不要主动删除用户文件。
5. 最小注释：只写非显而易见的 why，代码自解释优先。
6. QEMU 仅用 bundled（`~/.pvm/runtime/qemu`）或系统安装；版本门槛逻辑（darwin/arm64 HVF 拒绝 QEMU 11.0.0 与未知版本）不得绕过。
7. VPS/exit 侧 nft/iptables 规则按实例命名空间精确创建与清理（如 iptables comment `pvm-exit`），绝不 flush 全局规则。

## 验证惯例

- 功能改动必须真实跑通：VM 级端到端优先（create/ssh/route/exit 全链路），仅编译通过不算完成。
- 提交前 `git status` 与 `git diff --staged` 检查：无临时文件、无敏感信息。
