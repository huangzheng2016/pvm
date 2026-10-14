# pvm Agent 指南

与用户同语言回复。本文件只保留每个任务都必须知道的项目地图、硬约束和交付流程。代码及其实际行为优先于文档；改代码前先读相关代码和最近的 `AGENTS.md`。

## 研发原则

- 先明确可验证结果，再做最小实现。明显的问题直接修复，不反复询问；涉及删除用户数据、生产服务或远程操作时，先确认影响范围并保留回滚路径。
- 遵循现有模块边界和本地惯用写法，不夹带无关重构，不做推测性功能，不为不会发生的场景增加错误处理。
- 本项目只维护当前行为，不重新加入跨架构兼容、旧格式兼容或无用的回退路径。
- 注释只解释非显而易见的原因，不写样板、变更史、个人经历或代码已经表达的内容。Review 说清 bug 和修复即可；原因未确认就明确说未确认，不猜。
- 纯文本使用普通连字符和直引号，命令和代码输出必须可直接复制粘贴。
- 并行工作只处理互不依赖的文件或验证任务；同一文件、共享状态和最终合并由一个人负责，避免并发覆盖。
- 不为某个目录增加 Git 特殊规则或反向排除。`.gitignore` 只描述通用构建产物和临时文件；测试源码是正常源码，不得通过忽略规则隐藏。
- 不提交二进制、QEMU runtime、压缩 guest、临时脚本、工作笔记或测试残留。测试结束后删除生成的二进制和临时产物，并确认 `git status` 干净地只包含预期源码、测试和生成清单变化。
- 提交、推送、打 tag、发版和 force-push 都要用户明确要求；`master` 是主分支。提交信息、PR 和说明中不加入 agent 身份或 co-author 信息。

## Project Map

单个 Go 二进制按子命令承担 host CLI、daemon、guest agent 和 VPS exit-agent 角色；`cmd/gateway` 是独立的 guest 网关二进制。默认数据根目录为 `~/.pvm/`。

- `cmd/pvm`: 主程序入口和 CLI usage；`exit-agent` 子命令运行在 VPS。
- `cmd/gateway`: guest 网关入口，由 daemon 推送的 `/etc/pvm/gateway.json` 驱动。
- `cmd/assetpack`: 构建期压缩 guest 二进制并生成 embed manifest。
- `internal/cli`: CLI 参数解析和 daemon 瘦客户端；所有用户可见行为都要在这里一致处理。
- `internal/daemon`: 常驻控制面，unix socket 上的 HTTP/JSON，负责 VM/cluster 生命周期和 readiness；daemon 单实例 flock。
- `internal/qemu`: QEMU 工具链、进程/QMP、快照和运行状态。
- `internal/vm`: VM 规格、overlay 盘、cloud-init seed、端口和磁盘元数据。
- `internal/image`: 基础镜像下载、缓存和 golden 镜像烘焙；golden 是不可变、按版本保留的资产。
- `internal/cluster`: 集群状态、子网/IP/MAC、端口池和路由状态，持久化必须原子写。
- `internal/gateway`: guest 网关的网桥、DHCP、DNS、NAT、fake-ip 和 fail-closed kill-switch。
- `internal/gw`: host/guest 共享的 DNS、policy 和 rules 逻辑。
- `internal/exit`: VPS exit-agent、peer 注册、WireGuard 和 nft/iptables/userspace NAT。
- `internal/tunnel`: UDP/TCP 两种传输的 WireGuard device 封装。
- `internal/agent`: guest 内 exec agent 和并发会话。
- `internal/assets`: 内嵌资产解包和 SHA256 校验；仅 `-tags embedassets` 编入。
- `internal/native`: 架构选择的唯一事实来源，镜像、guest 和 QEMU target 由此派生。
- `internal/paths`: `~/.pvm` 目录布局。
- `internal/keys`: pvm 专用 SSH keypair，绝不触碰 `~/.ssh` 或 macOS 钥匙串。
- `internal/fsutil`: 原子写和文件锁。
- `internal/buildinfo`: 构建期版本号。
- `script/build.sh`: 原生构建入口；`script/qemu/`: bundled QEMU 组装与签名。
- `SKILL.md`/`SECURITY.md`: 面向 agent 的使用和安全指南，构建期 embed 到二进制中。

## Build & Test

- Go 使用 1.26；只构建宿主原生架构（darwin/arm64、linux/amd64 host）。Linux amd64/arm64 release 二进制用于对应 VPS，不做跨架构 host 模拟。
- `./script/build.sh` 构建 guest、assetpack 和带 `embedassets` 的 host 二进制；macOS 默认追加 bundled QEMU，`PVM_PACK_QEMU=0` 可跳过。根目录 `./pvm` 是构建产物，不是源码，验证后必须删除。
- 提交前必须通过 `go test ./...`、`go vet ./...` 和 `sh -n scripts/install.sh`；修改并发或状态路径时，相关包及全仓库都要用 `go test -race` 复跑。
- 功能改动必须验证真实行为：CLI 帮助和参数、伪终端 SSH、create/start/readiness、失败回滚、挂载、端口和出口链路。只做编译或 import 检查不算完成；不能验证的项目要在交付说明中列明。
- 修改文档示例、CLI 文本或嵌入文档时，同时检查源码、`README.md`、`SKILL.md`、`SECURITY.md` 和构建后 `pvm skill`/`pvm security` 的输出，避免文档与实现漂移。

## Hard Constraints

1. 公开仓库和公开输出中禁止真实 IP、域名、VPS 地址、token、私钥、证书指纹、个人路径、真实出口名和真实主机名。示例和测试统一使用 `dev1`、`my-exit`、`vps.example.com`、`example.com`、`YOUR_TOKEN` 或 `203.0.113.0/24` 等 mock 值；提交前搜索 diff 和嵌入输出。
2. 状态文件必须走 `fsutil.AtomicWrite`（tmp+fsync+rename）。daemon 必须单实例 flock；guest 侧规则、路径和命令输入必须白名单校验。
3. 流量泄漏和身份混同是最高严重度：kill-switch 必须 fail-closed，隧道断开时 VM 断网，绝不能回退宿主直连。不得把宿主 `~/.ssh`、`~/.gitconfig` 或个人文件传入 VM；不得主动删除用户文件。
4. 密钥和证书必须可恢复且不静默轮换：缺少公钥时从已有私钥派生，私钥/公钥不匹配要报错，不能覆盖有效私钥。
5. QEMU 只使用 bundled 或系统安装版本，版本门槛和 HVF 签名 entitlement 不得绕过。VPS 的 nft/iptables 规则按实例命名空间精确创建和清理，绝不 flush 全局规则。
6. 失败必须显式可见：不得静默忽略参数、状态错误或部分成功。create/clone/map/route 等先落状态再做副作用的操作必须回滚，或返回清楚的可恢复状态和下一步命令。
7. `running`、guest provision、agent ready 和 cluster ready 必须分开建模。启动、升级和 daemon 恢复不能让 CLI 在资源尚未就绪时假装成功；长操作要有进度和可恢复的错误提示。

## Delivery Checklist

- 变更前先读文件并确认工作树没有用户未提交修改；变更后检查 `git diff --check`、`git status` 和敏感信息。
- 运行完整构建和测试，复核生成清单只包含预期变化；删除根目录 `./pvm` 等构建产物，不能把二进制带入提交。
- 最终说明实际执行的验证、仍未验证的项目和任何运行时限制，不把代码审查或编译结果冒充端到端成功。
