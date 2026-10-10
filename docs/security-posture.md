# 风控姿态（Security Posture）

pvm 的 VM 用于本地开发。以下是与风控/环境指纹相关的设计决策与使用建议。

## 架构边界

pvm 仅支持宿主原生架构：darwin/arm64 只运行 arm64 VM，linux/amd64 只运行 amd64 VM。构建与打包也只包含当前宿主架构的 guest 和 QEMU 运行时，不提供其他架构的模拟或分发包。

## VM 形态本身就是最优解

Claude Code 等工具的运行环境检测针对的是**容器**与云主机：`/.dockerenv`（docker 信号）与 `/sys/hypervisor/uuid`（Xen/EC2 信号）会被映射为 `deployment_environment` 并上行。pvm 的 QEMU VM 两者都没有，在用户态看来就是一台普通 Ubuntu 机器。因此：

- **claude 类工具跑在 VM 主用户态**，不要在 VM 内的 docker/kasm 容器里跑——容器里会出现 `/.dockerenv`。
- kasm 只作为桌面/浏览器用途。
- Alpine/musl 不是典型桌面用户环境，跑 Claude Code 等场景建议用 Ubuntu/Debian；Alpine 定位是精简工具机。

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

- **git 身份**：Claude Code 的会话上下文（gitStatus）携带 `git config user.name/user.email` 上行。pvm **绝不读取或传播宿主 `~/.gitconfig`**；需要时用 `pvm create --git-email/--git-name` 显式设置。gitStatus 的内容面不止用户名/邮箱：分支名、`git status` 文件名、近 5 条提交标题与账号邮箱同样随每次请求上行——VM 内仓库避免中文分支/提交/文件名。
- **设备身份**：真正上行的设备指纹是 `~/.claude.json` 的随机 userID（deviceId）——它随每个遥测事件和每次 API 请求上行，任何遥测开关都关不掉。每台新建 VM 都有全新 deviceId。machine-id / ssh host key 不上行（读取后被客户端丢弃），golden 对它们的清洗属纵深防御。**clone 不会重置 deviceId**：clone 一台用过 Claude 的 VM，两台在服务端仍是同一台设备；要全新身份请新建 VM。RAM 快照会把设备身份一起冻结——同一 VM 快照恢复后仍是"同一台设备"，这是预期行为。
- **主机名**：trusted_devices 的 display_name 会带 hostname，VM 名保持中性即可。
- **SMBIOS**：`pvm create --smbios-spoof` 让 DMI 呈现为常见 PC 硬件（Dell Inspiron）而非 QEMU 默认值。当前证据显示该类字段不上行，默认关闭。
- **字体取舍**：golden 预装 `fonts-noto-cjk` 是为正确渲染中文内容。它是纯本地信号、遥测不可见，但若走 Kasm 浏览器路线，CJK 字体集可被网站字体指纹侧面探测，与"融入美国桌面用户"的画像有张力——按实际用途取舍。
- **ssh locale**：OpenSSH 客户端默认 `SendEnv LANG LC_*`，宿主中文 locale 会进入交互会话环境。CLI 遥测不上行 locale（实际风险为零），但 shell 内 `LANG=zh_CN` 与地区画像不一致时，可用 `ssh -o SetEnv=LANG=en_US.UTF-8` 覆盖。

## 不要做的事

- **不要关闭遥测**（`DISABLE_TELEMETRY` 等）：该动作本身集中在特定地区用户群体中，是强风控特征；且会连带关闭付费功能。VM 里保持默认。
- **不要用 AI gateway/中转**：不要在 VM 内设 `ANTHROPIC_BASE_URL` 或经 LiteLLM、Helicone、Portkey 等中转访问——中转 host 会明文随 GrowthBook 上行，网关类响应头特征（`via`/`cf-ray`/`x-envoy-*` 等）也会被检测上报。pvm 的出口已在 VPS，叠加中转只会多出明文指纹。
- **不要 7×24 无间断调用**：活跃时段应与出口地理相符。事件信封时间戳恒为 UTC，服务端可从活跃时段分布反推真实时区，与客户端时区伪装无关。
- 不要在 golden 镜像里配置地区性软件镜像源；provision 只用官方源。
