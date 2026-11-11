# pvm

[English](README.md) | 简体中文

单个二进制管理的个人 QEMU 开发虚拟机：一条命令创建隔离的 Linux 开发机，并让它的全部流量经由你自己的 VPS 出口。

## 功能

- **快速、开箱即用的虚拟机：** Golden 镜像预装 Docker、常用开发工具和桌面。`pvm create` 会自动下载基础镜像并烘焙缺失的 golden；首次烘焙只需一次，约十分钟，之后新建虚拟机通常约 30 秒就绪。
- **失败即阻断（fail-closed）的出口路由：** TCP、UDP 和 ICMP 经 WireGuard 发往你的 VPS（UDP 或 TCP 承载），或发往 Cloudflare WARP（UDP）。只有显式的 direct/internal 规则可以绕过所选出口；隧道断开时流量直接丢弃，不会回落到宿主机出口。
- **真实 IP 的全隧道 DNS 与分流：** 走默认出口的域名经所选出口解析为真实 IPv4 地址，并通过三层隧道直接转发。只有显式的域名代理规则才使用 fake-ip 中继。direct/internal 域名保留真实地址和直连路径；域名与 CIDR 规则无需重启即可生效。
- **原生浏览器桌面：** `pvm desktop` 通过 TigerVNC 和 noVNC 提供虚拟机自身的 XFCE 桌面。默认只给出回环地址的 URL；`--public` 可选择监听 `0.0.0.0`，但仍需 VNC 认证。它和 SSH 登录的是同一台机器，不是容器。
- **按宿主机配置取默认值：** 新虚拟机默认使用宿主机逻辑 CPU 数的四分之一（向上取整）和四分之一内存，并通过 balloon 驱动把空闲内存还给宿主机。稀疏 qcow2 磁盘默认 20 GB，可用 `pvm resize` 扩容，用 `pvm compact` 释放未使用的宿主机空间。
- **完整的虚拟机工作流：** RAM 快照数秒内恢复；克隆会重新生成机器标识；只读的 `pvm audit` 检查路由及 TCP/UDP/ICMP/DNS 健康状况；回环端口映射、双向 `/share` 与自定义宿主机挂载、virtio-serial exec 覆盖日常开发与故障恢复。

## 安装

pvm 支持 arm64 的 macOS、amd64 的 Linux，以及 amd64 的 Windows（原生 QEMU 或 WSL2）。

**Windows：** 从 [Releases](https://github.com/huangzheng2016/pvm/releases) 下载 `pvm_windows_amd64_setup.msi` 或 `pvm_windows_amd64_setup.exe` 并运行。两者都是按用户安装的安装包，不需要管理员权限，并自带 QEMU 运行时。详见 [Windows 安装包](#windows-安装包)。下面的 `curl` 脚本在 Git Bash 中也能用，但 Windows 上推荐使用安装包。

**macOS 和 Linux：** 先安装 GitHub CLI（`gh`）和 Python 3，然后用 `curl` 安装最新版本：

```bash
curl -fsSL https://raw.githubusercontent.com/huangzheng2016/pvm/master/scripts/install.sh | sh
```

安装脚本默认把 `pvm` 放到 `~/.local/bin`，可通过 `INSTALL_DIR` 修改。在 macOS、Linux 和 Windows 上，它还会安装自带的 QEMU 运行时；设置 `PVM_SKIP_QEMU=1` 则跳过，pvm 改用系统 QEMU。安装脚本会校验发布包的 checksum 和 GitHub 构建证明（attestation），包括仓库、工作流和 tag；没有这些证明的发布会被拒绝。所有下载和可执行文件检查都在替换前完成，旧的二进制和 QEMU 运行时会被保留。你也可以从 [Releases](https://github.com/huangzheng2016/pvm/releases) 下载压缩包，使用前先用 `gh attestation verify` 校验。

### Windows 安装包

每个发布都附带两个按用户安装、不需要管理员权限的 Windows 安装包：`pvm_windows_amd64_setup.msi` 和 `pvm_windows_amd64_setup.exe`，都自带 QEMU 运行时，任选其一即可。两者都会把 `pvm.exe` 安装到 `%LOCALAPPDATA%\pvm`，把该目录加入用户 `PATH`，并把运行时安装到 `%USERPROFILE%\.pvm\runtime\qemu\x86_64`（设置了 `PVM_HOME` 时为 `PVM_HOME\runtime\qemu\x86_64`）。安装后请打开新的终端，让 `PATH` 变更生效。下载后可先用 `gh attestation verify pvm_windows_amd64_setup.exe --repo huangzheng2016/pvm` 校验。

- 正在运行的 pvm 或虚拟机占用的文件无法替换。交互运行时，两种安装包及其卸载程序都会检测正在运行的 daemon 或 QEMU，并提示停止 daemon（同时会停止正在运行的虚拟机）。静默运行（`/S`、`msiexec /qn`）则直接拒绝，请先执行 `pvm daemon stop`。
- MSI 静默安装：`msiexec /i pvm_windows_amd64_setup.msi /qn`。MSI 只能读到持久化的用户级 `PVM_HOME`；可传入 `PVMHOME=D:\pvm-home` 显式指定位置。升级和卸载会沿用记录下来的位置。
- 从“设置 > 应用”卸载会删除 `pvm.exe`、运行时和 `PATH` 条目。`~/.pvm` 下的虚拟机磁盘、密钥和配置会保留。
- 每台机器只使用一种安装包。两种方式安装的副本都可以用 `pvm upgrade` 原地更新。

Windows 安装包的已知限制：

- 安装、以及从“设置 > 应用”卸载，都是在 pvm 停止的状态下测试的。提示停止正在运行的 daemon 的流程，在两种安装包中都还没有端到端测试过。如果提示没有出现，或者安装程序仍然提示 pvm 正在运行，请取消，执行 `pvm daemon stop`，然后重新运行安装程序。
- 安装程序停止 daemon 时会等待正在运行的虚拟机关闭，最长约三分钟，期间不显示进度。
- 没有 daemon 而遗留运行的虚拟机，安装程序无法停止。它会列出这些虚拟机并拒绝继续；请先关闭它们，再重新运行安装程序。
- 杀毒软件会扫描刚解压的文件并短暂锁定它们。安装程序对每次文件移动最多重试 10 秒；如果锁定持续更久，安装程序会报错停止并保留旧版本。请重新运行安装程序。
- 执行 `pvm upgrade` 后，“设置 > 应用”中显示的仍是安装包当初写入的版本号。

### Windows 后端选择

Windows 支持两种后端，通过 `PVM_BACKEND` 或 `~/.pvm/config.yaml` 中的 `backend` 选择：

- `wsl`：把 pvm 命令转发给 WSL2 中的 `pvm`。需要完整的 `/share`、额外挂载以及（可用时的）KVM 时推荐使用该后端。`C:\work\proj` 这类 Windows 挂载路径会自动转换为 `/mnt/c/work/proj`。
- `native`：直接运行 Windows 版 pvm daemon 和 Windows QEMU。daemon 监听回环 TCP，除 `/v1/health` 外的所有 API 都需要 `~/.pvm/run/admin.token` 中的 bearer token。官方 Windows QEMU 构建不包含 virtio-9p，因此 `create` 会使用 `--share none` 并给出明确警告；显式要求 `private` 或 `global` 会失败，并提示改用 WSL2。`pvm daemon install` 会注册一个计划任务（Scheduled Task）。
- `auto`（默认）：如果已有正在运行的原生 daemon 或原生 QEMU 就使用原生后端，否则在 WSL2 中装有 `pvm` 时使用 WSL2。

只有在 Windows Hypervisor Platform 已启用且 QEMU 探测成功时才使用 WHPX；否则原生模式使用 TCG 并给出提示。设置 `PVM_QEMU_ACCEL=whpx` 可强制尝试 WHPX，设置 `PVM_QEMU_ACCEL=tcg` 可强制使用软件模拟。`create`、`start`、`clone` 或 `image build` 检测到 TCG 时，pvm 会输出针对当前后端的加速建议。要启用 WSL2 嵌套虚拟化，在 `%UserProfile%\.wslconfig` 的 `[wsl2]` 下设置 `nestedVirtualization=true`，执行 `wsl --shutdown`，重启 WSL，并确认 `/dev/kvm` 存在。显式设置 `PVM_QEMU_ACCEL=tcg` 会关闭该警告。原生 Windows 使用 DNS-over-TCP，因为部分 Windows QEMU/网络组合无法传递 UDP DNS；可用 `PVM_DNS_UPSTREAM` 覆盖解析器（例如 `tcp://192.0.2.53:53`，或默认 slirp 地址 `10.0.2.3`）。`pvm doctor` 会报告后端、OpenSSH、QEMU、WSL2 和加速的就绪情况。

## 快速开始

daemon 会在需要时自动启动。首次 `create` 可能需要下载并烘焙镜像，之后会快得多。

```bash
pvm create --image ubuntu:26.04 --name dev1 --user dev
pvm mount dev1 ~/work/proj:/home/dev/proj
pvm ssh dev1
pvm desktop dev1
pvm map dev1 8080:80
```

新虚拟机的 `/share` 使用宿主机上的私有目录 `~/.pvm/vms/<name>/share`。用 `pvm create --share none ...` 可禁用，或显式选择 `--share global` 使用公共的 `~/.pvm/share`。已有虚拟机保持其记录的共享方式。烘焙虚拟机和 gateway 虚拟机不导出宿主机共享目录。

### 9p 挂载配置

`/share` 和额外挂载默认使用 `coherent` 配置（`cache=none,msize=262144`），保证宿主机与虚拟机双向可见。这是共享源码的基线配置，不提供多写者的事务一致性。`pvm mount ls dev1` 会显示每个额外挂载的配置和访问模式。要在 QEMU 层强制只读，先停止虚拟机，执行 `pvm mount dev1 --read-only /host/data:/mnt/data`，再启动。添加或删除只读挂载都需要虚拟机处于停止状态。

以大文件顺序读取为主的挂载，可显式选择 `readahead`：

```bash
pvm mount dev1 --profile readahead ~/work/data:/home/dev/data
```

它使用 `cache=readahead` 并请求 `msize=1048576`；实际 msize 由虚拟机与 QEMU 协商，可能更小。它不能消除小文件 create/stat 等元数据瓶颈，也不保证 pnpm、Vite 或 TypeScript 构建会更快。Node/TypeScript 依赖和构建缓存请放在虚拟机本地磁盘上。

如果宿主机在虚拟机持有旧文件描述符期间改写或替换了文件，`readahead` 可能一直返回旧内容，即使虚拟机新打开文件描述符也一样，直到旧描述符关闭为止。两端都在频繁读写同一批文件时请使用 `coherent`。只接受这两种命名配置，不接受任意 cache 选项。要修改额外挂载的配置，先关闭虚拟机中使用该挂载的程序，执行 `pvm mount rm`，再用新配置执行 `pvm mount`。升级 pvm 不会改写已有虚拟机的 cloud-init，也不会强制重新挂载。

注册一个 VPS 出口，并让整台虚拟机经由它上网：

```bash
pvm exit add my-exit --server vps.example.com:18443 --token YOUR_TOKEN
pvm route dev1 --via my-exit
pvm ssh dev1 'curl -s ipinfo.io'
pvm route dev1 --via direct
```

也可以使用 Cloudflare WARP，无需部署 VPS，也不用在虚拟机里安装任何东西：

```bash
pvm exit add-warp my-warp
pvm route dev1 --via my-warp
```

要让所有未显式设置路由的虚拟机都走某个出口，设置集群默认路由：

```bash
pvm route default --via my-exit
pvm route ls
```

## 架构

宿主机上的 `pvm` 二进制同时提供宿主机 CLI 和 daemon，也可以在 VPS 上作为出口 agent 运行。另有一个从 `cmd/agent` 构建的独立 guest 二进制，只包含 guest agent 服务、当前虚拟机的端口映射和 guest skill。daemon 负责虚拟机和集群的生命周期；受管 gateway 提供 DNS、路由、WireGuard 设备和 fail-closed 断网保护（kill switch）。WARP 的注册和凭据都在宿主机上管理，不需要 `warp-cli`、`wgcf` 或额外的配置文件。

```text
控制面: pvm CLI -> pvm daemon -> gateway 虚拟机和开发虚拟机
数据面: 开发虚拟机 -> gateway 虚拟机 -> WireGuard 隧道 -> VPS 上的出口 agent 或 Cloudflare WARP -> 互联网
```

## 支持的镜像和宿主机

- 镜像：`ubuntu:26.04`、`ubuntu:24.04`、`debian:13` 和 `debian:12`。
- 宿主机：arm64 的 macOS（`darwin/arm64`）、amd64 的 Linux（`linux/amd64`），以及 amd64 的 Windows（`windows/amd64`，通过 WSL2 桥接或原生 QEMU/WHPX；可用 `PVM_BACKEND=auto|wsl|native` 或 `config.yaml` 中的 `backend` 选择）。
- 虚拟机始终使用宿主机的原生架构，不支持跨架构模拟。Linux arm64 的发布二进制用于 VPS 出口 agent，不用于运行 pvm 宿主机。

## 在 VPS 上部署出口

先校验对应 VPS 架构的发布包，并创建仅所有者可读的 token 文件，然后把出口 agent 作为常驻服务运行：

```bash
pvm exit-agent --listen :18443 --token-file /etc/pvm-exit/token
```

然后在宿主机上用 `pvm exit add` 注册它。完整的 SSH 部署流程（包括 systemd unit、指纹校验、测速和回滚）见 [SKILL.md](SKILL.md#standard-flow-deploy-an-exit-over-ssh)。

要让手机或笔记本用标准 WireGuard 客户端连接，可从 UDP 出口导出一份独立的客户端配置：

```bash
pvm exit client-config my-exit --output phone.conf
```

该命令会生成新的客户端密钥、在出口 agent 上登记，以 `0600` 权限写出配置文件，并显示供手机 App 扫描的二维码。每台设备执行一次。标准客户端只支持 UDP，得到的是全隧道 IPv4 VPN，不包含 pvm gateway 的 DNS 或分流规则。IPv6 不走隧道。

## 文档

- [SKILL.md](SKILL.md)：面向 agent 和人的完整使用手册；`pvm skill` 会输出内嵌的副本。
- [SECURITY.md](SECURITY.md)：宿主机/虚拟机信任边界、凭据、镜像来源和网络验证；`pvm security` 会输出内嵌的副本。
- [AGENTS.md](AGENTS.md)：仓库结构、构建与测试流程以及开发规则。
- `pvm upgrade` 会校验发布包、保存恢复点、升级已安装的二进制和自带的 QEMU 运行时、重启正在运行的 daemon，并尝试恢复之前所有正在运行的用户虚拟机。用 `--no-restart` 只替换二进制和运行时，用 `pvm upgrade --rollback` 恢复保存的二进制、QEMU 运行时和虚拟机恢复计划。在 Windows 上，包含 QEMU 时 `--no-restart` 要求没有正在运行的虚拟机；省略该参数，pvm 会在替换运行时前停止 daemon，并在之后恢复虚拟机计划。设置 `PVM_SKIP_QEMU=1` 则只升级 pvm。
- `pvm audit dev1 --privacy`、`pvm doctor` 和 `pvm route explain dev1 example.com` 只检查本地配置，不启动 daemon，也不发起网络探测；输出中的可识别信息会被脱敏。
- 自动更新检查默认关闭。在 `~/.pvm/config.yaml` 中设置 `update_check: true` 或设置 `PVM_UPDATE_CHECK=1` 可开启；`PVM_NO_UPDATE_CHECK=1` 可关闭。
- 如需使用独立信任的镜像签名密钥，执行 `pvm image fetch ubuntu:26.04 --keyring /trusted/ubuntu-cloudimage.gpg`。这需要 `gpgv`；pvm 不会自动下载或信任任何密钥。

## 开发

完整构建使用 Go 1.27.4 和 garble v0.18.0，CI 中固定了这两个版本。构建使用专用的 `cmd/agent` 入口和可复现的混淆设置。

```bash
go install mvdan.cc/garble@v0.18.0
./script/build.sh
go test ./...
```

在 Windows 上，先安装 Git for Windows、Go 1.27.4 和 garble v0.18.0，然后在 PowerShell 中运行包装脚本。它通过 Git Bash 运行 `script/build.sh` 并生成 `pvm.exe`。Windows 上不打包自带的 QEMU（该运行时由 `script/qemu/pack-windows-runtime.sh` 单独构建），因此包装脚本默认设置 `PVM_PACK_QEMU=0`，继续使用 `~/.pvm/runtime` 下已安装的运行时。

```powershell
go install mvdan.cc/garble@v0.18.0
.\script\build.ps1
.\pvm.exe version
```

不要在 Windows 宿主机上运行完整的 `go test ./...`。测试套件按 Unix 编写（文件权限位、`/tmp` 等路径、符号链接，以及 `ip`、`gh`、`bash` 等工具），在 Windows 上会有大量与改动无关的失败，而且 `internal/cli` 的测试会不断重新拉起自身，留下大量 `cli.test.exe` 进程。在 Windows 上只运行与改动相关的定向测试，例如 `go test ./internal/qemu -run Accel`；完整测试以 Linux 或 macOS（CI 或 WSL2）为准。

CI 对每个提交在各原生平台上各测试、构建一次，缓存 Go 和构建工具，QEMU 打包和签名只在发布时进行。发布可以复用同一提交在 master 上成功运行时已验证的产物。Linux namespace 测试必须实际运行，不能跳过。

安全边界：用户虚拟机共享同一个二层网络；全局共享目录和额外的宿主机挂载是显式的信任边界。它们不是相互隔离的不可信租户。显式的 direct/internal 规则可以绕过默认出口。保证、限制和部署检查见 [SECURITY.md](SECURITY.md)。
