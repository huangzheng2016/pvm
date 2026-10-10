# 风控姿态（Security Posture）

pvm 的 VM 用于本地开发。以下是与风控/环境指纹相关的设计决策与使用建议。

## 架构边界

pvm 仅支持宿主原生架构：darwin/arm64 只运行 arm64 VM，linux/amd64 只运行 amd64 VM。构建与打包也只包含当前宿主架构的 guest 和 QEMU 运行时，不提供其他架构的模拟或分发包。

## VM 形态本身就是最优解

AI-service Code 等工具的运行环境检测针对的是**容器**与云主机：`/.dockerenv`（docker 信号）与 `/sys/hypervisor/uuid`（Xen/EC2 信号）会被映射为 `deployment_environment` 并上行。pvm 的 QEMU VM 两者都没有，在用户态看来就是一台普通 Ubuntu 机器。因此：

- **ai-service 类工具跑在 VM 主用户态**，不要在 VM 内的 docker/kasm 容器里跑——容器里会出现 `/.dockerenv`。
- kasm 只作为桌面/浏览器用途。

## 网络一致性

- `pvm route <vm> --via <exit>` 后，该 VM 全部流量（含 DNS）经隧道从 VPS 出口，fail-closed：隧道断 = VM 断网，不会回退到本地出口造成 IP 跳变。
- 被代理域名采用**域名直达（domain-egress）**：网关对代理域名不执行任何 DNS 解析（本机/本地网络对访问的域名零可见），域名经隧道内 domain-connect 通道送到 exit-agent，由它在 VPS 上解析并拨号。

## 内网 DNS 分流

- `pvm route add internal --domain-suffix corp.example`：内网域名默认经**宿主机原本的 DNS** 解析（slirp 上行，宿主在办公网/VPN 里自然能解析），无需单独配置。
- `--dns 10.0.0.53` 为可选覆盖：匹配后缀**只**往指定服务器转发，非匹配查询物理隔离，绝不发往该服务器。
- 内网域名的解析流量与流量本体都只走直连、不进隧道；`--via <exit>` 的 VM 上内网查询同样不进隧道。
- 直连规则（`route add direct`）域名同样走默认上行解析——直连意味着流量本就经本地出口。

## 环境信号对齐（地区画像）

- `pvm exit add/set <name> --tz America/Los_Angeles --locale en_US.UTF-8` 给出口节点登记地区。
- `pvm route <vm> --via <exit>` 时自动把 VM 的 `timedatectl` 时区与 `update-locale` 设为出口地区；`--via direct` 时恢复原值。时区会进入模型提示词（"Local timezone: ±HH:MM"），必须与出口 IP 地理一致。
- `pvm create --tz/--locale` 可创建时直接指定；默认 UTC + `en_US.UTF-8`（中立）。

## 身份卫生

- **git 身份**：AI-service Code 的会话上下文（gitStatus）携带 `git config user.name/user.email` 上行。pvm **绝不读取或传播宿主 `~/.gitconfig`**；需要时用 `pvm create --git-email/--git-name` 显式设置。
- **设备身份**：`~/.ai-service.json` 的随机 userID 是唯一真正上行的持久设备指纹（遥测事件与 `/v1/messages` 的 `metadata.user_id` 都带它，且任何遥测开关都关不掉）。相比之下 machine-id 与 ssh host key 的读取结果不上行，属纵深防御。每台新 VM 天然是全新设备；**clone 的首启清洗会同时清除 `~/.ai-service.json` 与 `~/.ai-service/`**——克隆体与源机在服务端不是同一台设备。RAM 快照会把设备身份一起冻结：同一 VM 快照恢复后仍是"同一台设备"，这是预期行为。要换设备身份就新建或 clone，不要在 VM 间复制 `~/.ai-service.json` / `~/.ai-service/`。
- **主机名**：trusted_devices 的 display_name 会带 hostname，VM 名保持中性即可。
- **SMBIOS**：`pvm create --smbios-spoof` 让 DMI 呈现为常见 PC 硬件（Dell Inspiron）而非 QEMU 默认值。当前证据显示该类字段不上行，默认关闭。

## 不要做的事

- **不要关闭遥测**（`DISABLE_TELEMETRY` 等）：该动作本身集中在特定地区用户群体中，是强风控特征；且会连带关闭付费功能。VM 里保持默认。
- 不要在 golden 镜像里配置地区性软件镜像源；provision 只用官方源。
