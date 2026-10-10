---
name: pvm
description: 使用 pvm 创建和管理隔离的 Linux 开发虚拟机，并把指定 VM 的全部流量经 VPS exit-node 出口。需要一台干净/可丢弃的开发机、隔离设备或网络指纹、让某个环境只经指定 VPS 出口、开网页桌面、做端口映射/快照/克隆时使用本 skill。
---

# pvm：隔离开发机与出口管理

pvm 是本机的 QEMU 虚拟机系统（Go 单二进制）。daemon 默认常驻，CLI 在 daemon 未运行时会自动拉起（例外：显式 `pvm daemon stop` 后不会自动拉起，需 `pvm daemon start`）。所有操作通过 `pvm` 完成，不需要直接操作 QEMU。

## 架构边界

pvm 仅支持宿主原生架构：darwin/arm64 只运行 arm64 VM，linux/amd64 只运行 amd64 VM。构建产物也只包含当前宿主架构的 guest 与 QEMU 运行时，不提供其他架构的模拟或分发包。

## 使用前提

- 仓库内使用 `cd ~/pvm && ./pvm ...`；若 pvm 已加入 PATH，直接用 `pvm ...`。
- 先跑 `pvm daemon status` 确认 daemon 正常；`pvm list` 查看现有 VM。
- 首次使用先 `pvm image list` 确认基础镜像已下载，缺失则 `pvm image fetch <distro:version>`；base 镜像不存在时 create 会直接报错提示 fetch。
- 本机已注册出口 `us`（WG over UDP）和 `us-tcp`（WG over TCP），指向 203.0.113.10。用 `pvm exit ls` 确认，不要重复 add 同名出口。
- VPS 上的 `pvm-exit-agent.service` 必须保持运行。不要停它，除非你明确要测试 kill-switch 或进行升级。

## 常用选择

- **镜像**：`ubuntu:24.04`、`ubuntu:22.04`、`debian:13`、`debian:12`、`alpine:3.20`、`alpine:3.21`
- **出口传输**：`udp`（默认、性能优先）/ `tcp`（受限网络后备）
- **exit-agent NAT 后端**：`auto`（自动探测）/ `nft` / `iptables` / `userspace`（免 root、无 ICMP）
- **出口路由**：`direct`（本机出口）或 `<exit-name>`（本机现有 `us`、`us-tcp`）
- **create 常用参数**：`--image`、`--distro`、`--version`、`--name`、`--user`、`--cpus`、`--mem`、`--disk`、`--mount`、`--tz`、`--locale`、`--git-email`、`--git-name`、`--smbios-spoof`、`--no-start`、`--no-wait`
  - `--image alpine:3.20` 与 `--distro alpine --version 3.20` 等价；冲突参数会报错。
- **Alpine 差异**：精简定位——没有 golden 镜像、没有 docker/kasm、首次启动要跑 apk 装包（稍慢）、登录 shell 默认 `/bin/sh`。跑 Claude Code 等场景建议用 Ubuntu/Debian（Alpine/musl 不是典型桌面用户环境）。
- **`pvm image build` 代理透传**：构建 golden 时自动使用宿主 `HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY`（回环代理地址自动改写为 VM 可见地址），构建结束彻底清理，不会把代理配置烘进镜像。

## 核心工作流

### 1. 创建并进入开发机

```bash
pvm create --image ubuntu:24.04 --name dev1 --user dev
# 别名：pvm create --distro alpine --version 3.20 --name a1 --user dev
pvm list
pvm ssh dev1 'whoami && ls /share'
```

- `--user` 必须显式给出。缺省时 create 会交互询问用户名；**非交互环境下 stdin 遇 EOF 会静默回退默认用户 `pvm`**（不报错，可能非预期），脚本里务必显式传 `--user`。
- create 默认进入跟随模式，显示镜像、磁盘、启动、cloud-init 进度；`--no-wait` 可后台创建。
- golden 镜像已预装 Docker、常用开发工具和 Kasm 桌面镜像。`/share` 默认挂载宿主机 `~/.pvm/share`，用于和宿主机交换文件。
- SSH 使用 pvm 专用密钥，无需密码。`pvm ssh dev1 bash` 进入交互 shell；`pvm ssh dev1 '<cmd>'` 执行单条命令。SSH 不可达时会自动经 virtio-serial agent 通道兜底（也可显式用 `pvm exec dev1 <cmd...>`，不依赖网络）。
- create/start 会先把 VM 的 IP/MAC 映射**同步**推送到网关 DHCP 后才开机（新 VM 立即可达）；网关不可达时 start 会失败，而不是开出一台无网 VM。

### 2. 把 VM 全部流量切到 VPS 出口

```bash
pvm exit ls
pvm route dev1 --via us          # UDP，优先
pvm route dev1 --via us-tcp      # TCP 后备
pvm ssh dev1 'curl -s ipinfo.io'
```

- `ipinfo.io` 的 IP、城市、时区应和 VPS 所在地一致；地区画像会自动把 VM 时区/locale 切到出口地区（`en_US.UTF-8`，经 SSH 应用；VM SSH 不可达时本次跳过，重新执行 `pvm route` 可重试）。
- 恢复本机出口：`pvm route dev1 --via direct`。
- **fail-closed**：出口隧道断开时，绑定出口的 VM 会完全断网，这是防止流量泄漏的预期行为，不要用 direct 绕过排查；恢复 VPS 服务后应自动回连。
- 切换 `--via` 的瞬间有秒级无网窗口（隧道 enroll/握手收敛）；隧道死亡后约 60s 内自动重建，期间绑定 VM 断网属预期。
- 代理域名的 DNS 在 VPS 远端解析，网关本地不解析；不要把它当成故障。出口状态可用 `pvm exit test us` 验证握手与出口 IP。

### 3. 网页桌面（Kasm）

```bash
pvm kasm dev1
# 输出 http://127.0.0.1:<port>/，浏览器直接打开即可，无登录页、无密码
```

- kasm 要求 VM 来自 **golden 镜像**（docker 已预装、镜像已预拉 `kasmweb/core-ubuntu-jammy:1.16.0`，秒开不用等下载）。基础镜像和 Alpine VM 没有 docker，`pvm kasm` 会直接报错。core 是 KasmVNC + 最小桌面环境，按需安装应用。
- 容器启动时会注入 VM 当前时区（`-e TZ`），浏览器读到的时区与地区画像/出口地理一致；每次 `pvm kasm` 重建容器都会刷新。
- 需要浏览器自动化时，用本机浏览器打开该 URL；页面走 daemon 回环反代注入凭据，不要尝试绕过反代直连底层端口。
- Claude Code 等需要避免环境特征的工具，**跑在 VM 主用户态，不要跑在 Kasm/Docker 容器里**；容器内的 `/.dockerenv` 是可探测信号。

### 4. 端口映射

```bash
pvm map dev1 8080:80
curl http://127.0.0.1:8080/
pvm map ls
```

- 宿主机只监听回环地址，链路为宿主机 relay → 网关 → VM。
- 删除 VM 会连带清理 relay/kasm/agent 连接。

### 5. DNS 与分流规则

```bash
# 内网域名：默认用宿主机 DNS 解析且流量直连
pvm route add internal --domain-suffix corp.internal
# 可选指定专用内网 DNS
pvm route add internal --domain-suffix corp.internal --dns 10.0.0.53
# 普通直连规则
pvm route add direct --domain-suffix example.com
pvm route add direct --cidr 10.0.0.0/8
# 代理规则：指定域名/网段强制走某个出口
pvm route add proxy --domain-suffix example.com --via us
# 匹配类型：--domain-suffix / --domain-prefix / --domain-keyword / --cidr
pvm route ls
pvm route rm <index>
pvm dns ls
```

- 未匹配到 internal/direct/proxy 的域名一律给 fake-ip（198.18.x.x）。**绑定出口的 VM**：域名经隧道由 VPS 远端解析，网关不本地解析；**direct 的 VM**：网关在连接时经宿主上行 DNS 解析后代拨。
- 域名规则只对经网关 DNS 的解析生效；硬编码 DoH 的应用不走网关 DNS，规则对其无效。
- 规则热生效，但已建立的连接不会 retroactive 切换。

### 6. 停止、删除与克隆

```bash
pvm stop dev1
pvm start dev1
pvm rm dev1 -y                  # 或 --force
pvm clone dev1 dev2             # 源 VM 必须 stopped；--no-start 只克隆不启动
```

- `pvm rm` 有二次确认：交互输入 y 才删；**非交互/脚本环境必须加 `-y`（或 `--force`）**，否则命令会拒绝执行。
- clone 默认继承挂载、CPU/内存、用户、时区、SMBIOS 等配置；出口路由不继承；端口映射默认不继承，可交互选择是否继承及改配端口（`--inherit-ports` / `--no-inherit-ports`）。
- clone 首启会自动重建 hostname、machine-id、SSH host key，避免两台 VM 身份混同（**仅 Ubuntu/Debian 克隆**；Alpine 克隆的身份重建脚本依赖 systemd 会静默失败，Alpine 请直接新建 VM）。
- 注意：clone 的清洗不含 Claude 的 deviceId（`~/.claude.json` 的 userID）——clone 一台用过 Claude 的 VM，两台在服务端仍是同一台设备。要全新设备身份就新建 VM。

### 7. 快照与排障

```bash
pvm snapshot create dev1        # 保存 RAM+磁盘状态并停机（需 VM 在运行）
pvm start dev1                  # 检测到快照时快速恢复，进程原样存活
pvm snapshot rm dev1            # 需 VM 先停止
pvm logs dev1 --follow
pvm daemon status
pvm daemon stop && pvm daemon start
```

- 快照适合挂起多日不用的机器或高风险操作前 checkpoint。
- daemon 重启会停掉用户 VM（gateway 会自动恢复）；需要时手动 `pvm start <name>`。
- 网关 pvm-gw1 若出现高 CPU 且 SSH 长时间不就绪，可 `pvm stop pvm-gw1 && pvm start pvm-gw1` 恢复。不要删除 pvm-gw1（删了 daemon 会自动重建，但重建期间 cluster 网络中断、自定义状态丢失）。
- daemon 是单实例锁；看到 `another pvm daemon is already running` 时先确认是否已有 daemon，不要再起第二个。

## 安全与身份姿态（防风控）

- 不要设置 `DISABLE_TELEMETRY`、`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` 等关闭遥测的环境变量；关闭动作本身和付费功能降级都是风险。
- 不要在 VM 内设置 `ANTHROPIC_BASE_URL` 或经任何 AI gateway/中转（LiteLLM、Helicone、Portkey 等）访问——中转 host 和响应头特征会明文上行；pvm 的出口已在 VPS，叠加中转只会多出指纹。
- 保持同一服务/账号的出口一致，不要在 direct 和 VPS 出口之间频繁切换；活跃时段与出口地理相符，避免 7×24 无间断自动化调用（服务端可从活跃时段反推真实时区）。
- 不要把宿主机 `~/.gitconfig`、`~/.ssh` 或个人文件复制进 VM；pvm 专用密钥与宿主 SSH 是隔离的。
- VM 时区、locale 应与出口地理一致，正常使用 Ubuntu/Debian 和官方镜像源，避免国内镜像与中文环境特征。
- gitStatus 随每次请求上行：除 `user.name/email` 外，分支名、`git status` 文件名、近 5 条提交标题也在其中——VM 内仓库避免中文分支/提交/文件名。
- 要全新的设备身份就新建 VM（每台新 VM 有全新的 `~/.claude.json` deviceId）。clone 是完整磁盘复制：机器码（hostname/machine-id/SSH host key）会重建，但用户文件原样继承——源机跑过 Claude 再 clone，两台机器在服务端仍是同一台设备；正确姿势是先 clone 再装 Claude，或克隆后自行删除 `~/.claude.json`。快照恢复代表同一台设备。

## 新增出口（仅在用户明确要求时）

```bash
# VPS 上手动启动（生产已有 us/us-tcp，不要重复部署）
pvm exit-agent --listen :18443 --token <随机强 token>

# 本机注册（指纹建议带外核对）
pvm exit add <name> --server <vps>:18443 --token <token> --transport udp
pvm exit add <name> --server <vps>:18443 --token <token> --transport tcp --fingerprint <sha256>
```

- 控制面 HTTPS + Bearer token + TOFU 指纹 pin；数据面 WireGuard，UDP 优先、TCP 后备。
- exit-agent 支持 `nft` / `iptables` / `userspace` NAT 后端；userspace 不需要 root，但不转发 ICMP ping。
