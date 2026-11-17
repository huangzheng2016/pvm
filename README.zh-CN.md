# pvm

[English](README.md) | 简体中文

单个二进制管理的个人 QEMU 开发虚拟机：一条命令创建隔离的 Linux 开发机，并让它的全部流量经由你自己的 VPS 出口。

## 亮点

- **一条命令，一台开箱即用的开发机。** 预装 Docker、常用工具、Firefox 和桌面。镜像只需烘焙一次，之后新建虚拟机约 30 秒就绪。见[快速开始](#快速开始)。
- **流量全走你自己的出口，出口断了就断网。** 经 WireGuard 走你的 VPS，或用无需部署的 Cloudflare WARP。隧道一断，虚拟机直接断网，绝不漏出宿主机 IP。见[失败即断网的出口路由](#失败即断网的出口路由)。
- **按域名或网段分流。** 让部分域名直连或走另一个出口，规则即时生效，无需重启。
- **虚拟机里的程序变成宿主机原生窗口。** `pvm app dev1 -- gedit` 或一个网页在你的桌面上开成独立窗口，Cookie、指纹和出口都留在虚拟机里，还能直接用宿主机输入法打字。见[虚拟机应用作为宿主机窗口](#虚拟机应用作为宿主机窗口)。
- **浏览器标签页里的完整桌面。** `pvm desktop` 打开虚拟机自己的桌面，和 SSH 登录的是同一台机器。见[浏览器桌面](#浏览器桌面)。
- **项目文件夹直接在虚拟机里用。** 挂载一个目录、保持两份同步副本，或上传一次再把结果下载回来。见[工作区](#工作区)。
- **秒级快照和克隆**、回环端口映射，以及只读的 `pvm audit` 路由和 DNS 体检。
- **Windows 免管理员安装。** 一个安装包装好 pvm、QEMU 和 Xpra 客户端。见[Windows 安装包](#windows-安装包)。

## 路线图

已完成：

- [x] 按用户安装的 Windows 安装包（v1.1.6）
- [x] 工作区：`mount`、`sync` 和 `copy`（v1.1.7）
- [x] golden 镜像自带 Firefox 和 sshfs（v1.1.7）
- [x] `pvm app`：虚拟机里的程序和网页作为宿主机窗口（v1.2.0）
- [x] `pvm.stream` 通道，以及更快的 `pvm desktop`（v1.2.0）
- [x] 原生 Windows 上的 UDP 和 UDP 出口（v1.2.0）
- [x] Windows 安装包自带 Xpra 客户端（v1.2.0）

计划中：

- [ ] `pvm exit-agent install|uninstall`，注册为 systemd 服务
- [ ] `pvm app` 在 macOS 和 Linux 上支持宿主机到虚拟机的剪贴板，支持 WSL2，并让输入法输入稳定可靠
- [ ] 输入法候选框跟随应用的文本光标

候选：

- [ ] `pvm ssh-config`，供 VS Code Remote-SSH、rsync 等工具使用
- [ ] `pvm exit test` 增加出网探测
- [ ] 默认镜像和默认虚拟机
- [ ] 端口映射改走 `pvm.stream`，不再经过 gateway 虚拟机
- [ ] 提示启动已停止的虚拟机
- [ ] 修复 gateway 规则漂移误报

详见 [docs/roadmap.md](docs/roadmap.md)。

## 安装

pvm 支持 arm64 的 macOS、amd64 的 Linux，以及 amd64 的 Windows。

### Windows

1. 从 [Releases](../../releases) 下载 `pvm_windows_amd64_setup.exe` 或 `pvm_windows_amd64_setup.msi`。可先校验：`gh attestation verify pvm_windows_amd64_setup.exe --repo huangzheng2016/pvm`。
2. 运行它，不需要管理员权限。
3. 打开新的终端让 `PATH` 变更生效，运行 `pvm doctor` 检查加速和后端。

两种安装包都会把 `pvm.exe` 装到 `%LOCALAPPDATA%\pvm` 并加入用户 `PATH`，把 Xpra 客户端装到 `%LOCALAPPDATA%\pvm\xpra`，把 QEMU 运行时装到 `%USERPROFILE%\.pvm\runtime\qemu\x86_64`（设置了 `PVM_HOME` 时装在它下面）。每台机器只用一种安装包；两种都可以用 `pvm upgrade` 更新。

- 正在运行的 pvm 或虚拟机占用的文件无法替换。交互运行时会提示停止 daemon（同时停止正在运行的虚拟机）；静默运行（`/S`、`msiexec /qn`）则直接拒绝，请先执行 `pvm daemon stop`。
- MSI 静默安装：`msiexec /i pvm_windows_amd64_setup.msi /qn`。传入 `PVMHOME=D:\pvm-home` 可指定数据位置，升级和卸载会沿用它。
- 从"设置 > 应用"卸载会删除 pvm、运行时、Xpra 客户端和 `PATH` 条目。`~/.pvm` 下的虚拟机磁盘、密钥和配置会保留。

<details>
<summary>Windows 安装包的已知限制</summary>

- 安装、以及从"设置 > 应用"卸载，都是在 pvm 停止的状态下测试的。提示停止正在运行的 daemon 的流程还没有端到端测试过。如果提示没有出现，或者安装程序仍提示 pvm 正在运行，请取消，执行 `pvm daemon stop`，再重新运行安装程序。
- 安装程序停止 daemon 时会等待正在运行的虚拟机关闭，最长约三分钟，期间不显示进度。
- 没有 daemon 而遗留运行的虚拟机，安装程序无法停止。它会列出这些虚拟机并拒绝继续；请先关闭它们，再重新运行安装程序。
- 杀毒软件会短暂锁定刚解压的文件。安装程序对每次文件移动最多重试 10 秒；锁定持续更久时会报错停止并保留旧版本。请重新运行安装程序。
- 执行 `pvm upgrade` 后，"设置 > 应用"中显示的仍是安装包当初写入的版本号。

</details>

Windows 有两种后端，通过 `PVM_BACKEND` 或 `~/.pvm/config.yaml` 中的 `backend` 选择：

- `native`：Windows 版 daemon 直接运行 Windows QEMU，启用 Windows Hypervisor Platform 时使用 WHPX。Windows QEMU 没有 virtio-9p，因此 `/share` 关闭；宿主机目录请用[工作区](#工作区)。
- `wsl`：命令转发给 WSL2 中的 `pvm`，支持完整的 `/share`、额外挂载，以及（可用时的）KVM。
- `auto`（默认）：优先使用正在运行的原生 daemon 或原生 QEMU，否则在 WSL2 装有 pvm 时使用 WSL2。

加速、DNS 和 WSL2 嵌套虚拟化的设置见 [SKILL.md](SKILL.md)。

### macOS 和 Linux

先安装 GitHub CLI（`gh`）和 Python 3，然后执行：

```bash
curl -fsSL https://raw.githubusercontent.com/huangzheng2016/pvm/master/scripts/install.sh | sh
```

脚本把 `pvm` 装到 `~/.local/bin`（可用 `INSTALL_DIR` 修改），并安装自带的 QEMU 运行时（设置 `PVM_SKIP_QEMU=1` 则改用系统 QEMU）。替换任何文件之前，它会校验发布包的 checksum 和 GitHub 构建证明（attestation），并保留旧的二进制和运行时。该脚本在 Windows 的 Git Bash 中也能用，但 Windows 上推荐使用安装包。

### 快速开始

daemon 会按需自动启动。首次 `create` 会下载并烘焙镜像，只需一次，约十分钟。

```bash
pvm create --image ubuntu:26.04 --name dev1 --user dev
pvm ssh dev1
pvm desktop dev1
pvm map dev1 8080:80
pvm exit add-warp my-warp
pvm route dev1 --via my-warp
```

支持 9p 的宿主机上，每台虚拟机都有一个私有的宿主机目录挂在 `/share`（`~/.pvm/vms/<name>/share`）；`pvm mount` 可以添加更多挂载，挂载配置见 [SKILL.md](SKILL.md)。

## 功能

### 失败即断网的出口路由

注册一个 VPS 出口，让整台虚拟机经由它上网。TCP、UDP 和 ICMP 经 WireGuard（UDP 或 TCP 承载）转发；隧道断开时流量直接丢弃，不会回落到宿主机的网络。

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

让所有未显式设置路由的虚拟机都走某个出口，并按域名或网段添加分流规则：

```bash
pvm route default --via my-exit
pvm route add direct --domain-suffix example.com
pvm route add proxy --domain-suffix example.com --via my-exit
pvm route ls
```

只有显式的 direct 和 internal 规则可以绕过所选出口。自己部署出口见[在 VPS 上部署出口](#在-vps-上部署出口)。

### 虚拟机应用作为宿主机窗口

`pvm app` 通过 [Xpra](https://xpra.org) 把虚拟机里的单个程序或网页显示为宿主机窗口。宿主机需要 Xpra 6.5 客户端，Windows 安装包已自带；虚拟机里第一次运行 app 时，会从 xpra.org 的仓库安装 Xpra 服务端。

```bash
pvm app dev1 -- gedit
pvm app dev1 https://mail.example.com --kiosk
pvm app ls
pvm app config dev1 --clipboard both --drop on
pvm app attach dev1
pvm app stop dev1 --all
```

剪贴板和拖放默认关闭，需按虚拟机单独开启。使用 `--clipboard to-guest` 或 `both` 时，宿主机内容只在该虚拟机的窗口切到前台、或窗口在前台时复制，才会进入虚拟机；被密码管理器标记为排除的内容永远不会发送。用宿主机输入法（比如微软拼音）输入的中文等文字，也能正常输入到应用里。详见 [docs/design/app.md](docs/design/app.md)。

### 浏览器桌面

`pvm desktop dev1` 通过 TigerVNC 和 noVNC 在回环地址上提供虚拟机自己的 XFCE 桌面，分辨率随浏览器窗口调整。新虚拟机通过 `pvm.stream` 传输；没有该端口的虚拟机继续使用原有 gateway 映射和中继，并在重启后自动迁移。`--public` 会监听 `0.0.0.0`，但仍需 VNC 认证。

### 工作区

工作区把一个宿主机目录绑定到一台虚拟机的 `/workspace/<id>`。创建时选择模式：

- `mount`：只有一份共享副本。Linux、macOS 和 WSL2 使用 9p；原生 Windows 后端使用反向 sshfs，走已有的 SSH 连接。宿主机上的修改不会在虚拟机中触发 inotify 事件，监听文件的工具需要改用轮询（例如 `CHOKIDAR_USEPOLLING=1`）。
- `sync`：两端各有一份完整副本。用 `push` 和 `pull` 搬运改动，发现冲突时会停止，直到你传入 `--prefer host`、`--prefer vm` 或 `--keep-both`。
- `copy`：上传一次，在虚拟机中工作，结束后用 `clone` 下载结果。

```bash
cd ~/src/web
pvm workspace create web dev1 --mode sync
pvm workspace ssh web 'npm install && npm test'
pvm workspace pull web
pvm workspace clone web ~/src/web-result
pvm workspace rm web
```

上传会跳过 `.gitignore` 匹配的路径和 `node_modules`；`.pvmignore` 文件可以追加规则或重新包含路径。pvm 拒绝上传或挂载主目录、文件系统根目录、它自己的数据目录，以及包含 `.ssh`、`.aws` 等凭据的目录树；`pvm workspace rm` 从不删除宿主机文件。详见 [docs/design/workspace.md](docs/design/workspace.md)。

### Windows 安装包

每个发布都附带 `pvm_windows_amd64_setup.exe` 和 `pvm_windows_amd64_setup.msi`：按用户安装、不需要管理员权限，一步装好 pvm、自带的 QEMU 运行时和 `pvm app` 所需的 Xpra 客户端。替换文件前会检测正在运行的 pvm，卸载时保留你的虚拟机。见[在 Windows 上安装](#windows)。

### 架构

宿主机上的 `pvm` 二进制同时提供 CLI 和 daemon，也可以在 VPS 上作为出口 agent 运行。另有一个从 `cmd/agent` 构建的独立 guest 二进制，只包含 guest agent、当前虚拟机的端口映射和 guest skill。daemon 负责虚拟机和集群的生命周期；受管的 gateway 虚拟机提供 DNS、路由、WireGuard 设备和 fail-closed 断网保护（kill switch）。WARP 的注册和凭据都在宿主机上管理，不需要 `warp-cli`、`wgcf` 或额外的配置文件。

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

VPS 防火墙必须放行 TCP 18443（控制面，`--listen`）、UDP 18443（WireGuard over UDP，`--udp-port`）和 TCP 18444（WireGuard over TCP，`--tcp-port`）。使用 `nft` 或 `iptables` NAT 后端时，还必须允许转发出口 agent 的 TUN 设备流量：UDP 传输为 `pvmexit0`，TCP 传输为 `pvmexit1`（前缀随 `--tun-prefix` 变化）。出口 agent 自己的 nftables 表无法覆盖转发路径上其他地方的丢弃规则，例如 ufw 默认的 `deny (routed)`。这种情况下 `pvm exit test` 仍然通过，因为它只探测出口 agent 本身，但经该出口路由的虚拟机无法向互联网建立连接，ping 却可能仍然正常。使用 ufw 时放行转发，并把 `eth0` 换成 VPS 的公网网卡：

```bash
ufw route allow in on pvmexit0 out on eth0
ufw route allow in on pvmexit1 out on eth0
```

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
