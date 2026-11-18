# PVM Desktop 界面设计提示词（给 Claude Design）

请为 **PVM Desktop** 设计一套完整的高保真界面原型，并输出可直接运行的 HTML/CSS/TypeScript。它是 pvm 的桌面客户端，类似 Docker Desktop：管理本机的 Linux 虚拟机、网络出口和工作区。目前已有一版能用的界面，功能都已实现，但视觉平淡，按钮全是文字，像一张后台表格。这次要重做视觉和交互，**功能范围不变**。产物要尽量能直接替换现有前端，只需少量适配。

## 1. 产品与用户

- **pvm 是什么**：在 Windows 上用 QEMU 运行隔离的 Linux 虚拟机，给开发和 AI 编程代理用。每台虚拟机的网络从指定的"出口"服务器出去，不走宿主机网络。虚拟机里的程序可以作为宿主机上的独立窗口打开。
- **用户**：开发者。他们熟悉命令行，但希望常用操作点几下就完成。界面要专业、紧凑、信息密度适中，不要营销页风格。
- **名称**：一律写作 **PVM Desktop**（大写 PVM）。命令行里的命令仍是小写的 `pvm`，例如 `pvm ssh dev1`。
- **界面语言**：英文。下文引号里的英文就是现有文案，可以润色，但不要改变意思。

## 2. 技术约束（决定能否直接用）

- **运行环境**：Wails v2，窗口内是 Microsoft Edge WebView2（Chromium 内核）。只考虑 Windows 10/11。
  - 窗口默认 1100×720，最小 800×540。要能适应这个范围内的各种尺寸，不考虑手机尺寸。
- **技术栈**：原生 TypeScript + CSS，用 Vite 构建，**不用 React、Vue 等框架**，也不用 UI 组件库。
  - 现有代码用一个很小的 `h(tag, props, ...children)` 函数构建 DOM。请沿用这种写法，或者给出同样轻量的组件函数。
- **完全离线**：不能加载任何外部资源，不用 Google Fonts、CDN 或远程图片。
  - 字体使用系统字体栈（`Segoe UI Variable`、`Segoe UI` 等），等宽字体用 `Cascadia Mono` 或 `Consolas`。
  - **图标用内联 SVG**，统一一套风格（线性、1.5–2px 描边、24 网格，类似 Lucide）。可以把需要的图标直接画进一个 `icons.ts`。
- **安全**：所有来自后端的文字一律作为文本节点插入，不要使用 `innerHTML`。
- **主题**：浅色和深色两套，默认跟随系统（`prefers-color-scheme`）。颜色全部定义成 CSS 变量。
- **动效**：要尊重 `prefers-reduced-motion`，开启时去掉所有位移和缩放动画。
- **可访问性**：
  - 全键盘可操作，焦点样式清晰。
  - 纯图标按钮必须有 `aria-label` 和悬停提示（tooltip）。
  - 对话框打开后，焦点留在对话框内。
- **数据层**：数据接口见第 6 节。请把"界面"和"取数据"分开，原型先用一份 mock 数据（`mock.ts`）驱动，函数签名和第 6 节保持一致，这样接入时只需把 mock 换成真实后端。

## 3. 视觉与交互方向

- **整体**：现代、干净、有一点灵动感。可以参考 Docker Desktop、Linear、Raycast、Windows 11 Fluent 的质感。克制用色：一个主强调色，状态用语义色（绿=就绪，蓝或琥珀=启动中，红=失败，灰=已停止）。
- **左侧导航栏**：
  - 每一项都配图标：VMs、Create、Exits、Workspaces、Settings。
  - 可以折叠成只显示图标的窄栏，折叠后悬停显示名称。
  - 当前项有明显的选中态。切换时可以做一个平滑移动的选中指示条。
  - 底部显示 daemon 状态（一个圆点加 "Daemon running" 或 "Daemon not running"），以及版本号。
- **操作按钮优先用图标**：
  - VM 和工作区的行内操作（Start/Stop、Terminal、App、Files、更多）改成图标按钮，带 tooltip。
  - 最主要的一个操作（如 Start）可以保留"图标+文字"。
  - 危险操作（Delete、Remove）放进"更多"菜单，并用红色标出。
- **VM 列表可以改成卡片或更有层次的列表**，不一定是表格。每台 VM 要能一眼看清：名称、状态、配置（镜像、CPU、内存、磁盘）、出口、是否默认 VM。
- **动效（要有，但要克制，时长 150–250ms，ease-out）**：
  - 状态徽章变化时淡入或变色过渡。"Starting" 状态有呼吸或脉冲效果。
  - 列表项出现时依次轻微上移淡入。
  - 对话框和下拉菜单弹出时轻微缩放并淡入。
  - Activity 面板展开和收起要平滑。正在运行的任务步骤有进度动效。
  - toast 从右下角滑入。
  - 按钮有悬停和按下反馈。图标按钮按下时可以有轻微缩放。
  - 正在执行操作的行，用细进度条或骨架闪烁表示忙碌，而不是只放一个转圈。

## 4. 全局结构

```
+------------+---------------------------------------------+
| PVM Desktop|  页面标题                      [主操作按钮]  |
|            |                                             |
|  VMs       |  页面内容                                   |
|  Create    |                                             |
|  Exits     |                                             |
|  Workspaces|                                             |
|  Settings  |                                             |
|            +---------------------------------------------+
| ● Daemon   |  ▲ Activity   最近一个任务的标题和状态       |
+------------+---------------------------------------------+
```

- **Activity 面板**：固定在底部，可以展开。它列出 PVM Desktop 执行过的所有任务（见第 5.6 节）。
- **toast**：右下角，用于提示失败的操作，带 "Dismiss" 按钮。
- **数据刷新**：每 2 秒自动刷新一次，窗口最小化时改为 10 秒。刷新时不要闪烁，也不要打断用户正在输入的表单。

### 4.1 全屏状态页（代替所有页面显示）

1. **加载中**："Loading..."，用一个优雅的加载动画。
2. **无法启动**：标题 "PVM Desktop cannot start"，下面是错误原因。
3. **WSL2 后端**：标题 "pvm is using the WSL2 backend"。
   - 说明文字："With the WSL2 backend the pvm daemon runs inside WSL, and Windows has no address or token to reach it. PVM Desktop works with the native Windows backend only."
   - 再附 3 条可复制的命令：`pvm ls`、`pvm ssh`、`pvm --help`。
   - 这种状态下隐藏导航栏和 Activity。
4. **daemon 未运行**，分两种情况：
   - 被用户用 `pvm daemon stop` 停掉的：标题 "The pvm daemon is stopped"，说明 PVM Desktop 不会自己启动它。
   - 其他原因没在运行：标题 "The pvm daemon is not running"。
   - 两种都有一个主按钮 "Start the daemon"。启动过程中显示 "Starting the pvm daemon..." 和动画。

### 4.2 通用组件

- **可复制的命令块**：等宽字体，右侧有复制图标按钮，复制后显示 "Copied" 反馈。
- **徽章**：状态、`default`、`read-only`、模式名等。
- **对话框**：
  - 标题、内容、取消按钮和提交按钮。
  - 提交前做校验，错误显示在对话框内。
  - 危险操作的提交按钮是红色，文字写明对象，例如 "Delete dev1"。
- **下拉菜单**：用于"更多"和 "Add exit"。菜单项可以禁用，危险项为红色。
- **空状态**：图标或插画，加标题、说明和一个主按钮。
- **错误横幅**：用于读取失败，例如 "Cannot read the VM list: ..."。
- **警告提示块**：琥珀色，用于"重启会停止程序"之类的提醒。

## 5. 页面与功能（全部要覆盖）

### 5.1 VMs（虚拟机）

- **页面头部**：标题 "Virtual machines"，主按钮 "New VM"，点击跳到 Create 页。
- **每台 VM 显示**：
  - 名称。默认 VM 带 `default` 徽章。
  - 配置摘要，例如 "ubuntu:26.04, 4 CPUs, 4 GB memory, 20 GB disk"。
  - 状态，共四种：
    - `Stopped`
    - `Starting (provision)`，括号里是当前阶段，可能是 `boot`、`provision`、`ssh`、`agent`。可以把阶段画成小的步骤进度。
    - `Ready`
    - `Failed`：在 VM 下方显示失败提示文字，即 hint。
  - 出口名称。如果用的是全局默认出口，下面注明 "default exit"。
- **行内操作**，图标加 tooltip：
  - **Start / Stop**：已停止时显示 Start（主按钮），否则显示 Stop。
  - **Terminal**：在 Windows Terminal 里打开到这台 VM 的终端，相当于 `pvm ssh <vm>`。
  - **App**：打开对话框，在 VM 里运行一个程序，窗口出现在宿主机上。
    - 输入框标签 "Command line or URL"，占位文字 `firefox  or  https://example.com`。
    - 说明："A command runs in the VM and shows its window here. A URL opens in the VM's browser."
    - 下方 "Installed programs" 列出已安装的目录程序，点一下直接打开。终端类程序标注 "(terminal)"。没有时显示 "None yet. Install some with More, Install programs."
    - 提交按钮 "Open"。
  - **Files**：在宿主机上打开 VM 的文件管理器窗口（Thunar）。
  - **更多菜单**：
    - **App settings**：对话框，三项都可以选 "Leave as it is"。至少改一项才能提交 "Apply"。
      - Clipboard：Off / Host to VM / VM to host / Both ways。
      - Drop files：On / Off。说明："Files dropped on the VM's windows upload into ~/Downloads in the VM."
      - Audio：Off / VM sound on the host speakers / Speakers and host microphone。
    - **Install programs**：对话框，多选目录里的程序，每项显示名称和描述。说明："Downloads run in the VM and leave through its exit."。只有 Ready 时可用。
    - **Change exit**：见 5.3 的"更换出口"对话框。
    - **Set as default**：设为默认 VM。
    - **Delete**（红色）：确认对话框 "Delete dev1?"，正文："This deletes the VM dev1 and its disk. It cannot be undone."
- **按钮可用规则**：
  - App 和 Files 需要 VM 处于 Ready 或 Failed。不可用时 tooltip 显示 "Start the VM first"。
  - 某台 VM 有操作正在执行时，它的其他按钮禁用，并显示忙碌效果。
  - 如果只是在等它启动完成，Stop、Delete 和更多菜单仍然可用。
- **空状态**：标题 "No VMs yet"，说明 "Create one to get started."，按钮 "Create your first VM"。

### 5.2 Create（创建虚拟机）

**表单**分为四组：

- **Machine**：
  - Name：字母、数字、`-`、`_`，最多 32 个字符，以字母开头。
  - Image：下拉选择，例如 `ubuntu:26.04`、`ubuntu:24.04 (not prepared yet)`、`debian:13`、`debian:12`。选中未准备的镜像时提示："No prepared image for X yet: the first create downloads and bakes one, which takes minutes."
  - User：说明 "The account pvm creates in the VM."
  - CPUs、Memory (MB)、Disk (GB)，带默认值。
- **Network**：
  - Exit：下拉选择 `direct (no exit)` 或某个出口。说明："Where the VM's traffic leaves, from its first boot on. direct uses the host's network."
  - 如果全局默认出口已被删除，显示警告。
- **App windows**：Clipboard、Drop files、Audio 三个下拉框，第一项都是 "Default"。
- **Programs to install**：程序卡片多选。每张卡片显示：
  - 名称。
  - 类型标签：`terminal` 或 `window`。
  - 描述。
  - 来源：官方或社区。

现有目录有三项：

| 名称 | 类型 | 描述 |
| --- | --- | --- |
| Claude Code | terminal | Anthropic's coding agent for the terminal |
| Claude Desktop | window | Anthropic's desktop app (Linux beta) |
| Codex CLI | terminal | OpenAI's coding agent for the terminal |

**提交**按钮是 "Create VM"。

**提交后，表单换成进度视图**：
- 展示这个任务的步骤列表，例如：
  1. `pvm create ...`
  2. `pvm route dev1 --via my-exit`
  3. `pvm app config dev1 ...`
  4. `pvm start dev1`
  5. `pvm app install dev1 claude-code`
- 每一步有四种状态：pending、running、done、failed。
- 正在运行的步骤自动展开，显示实时滚动的输出日志，最多保留最近几百行。
- **成功时**：显示 "dev1 is ready."，以及 "Go to the VMs"（主按钮）和 "Create another" 两个按钮。
- **失败时**：
  - 显示错误原因。
  - 列出没有执行的剩余步骤，作为可复制的命令："These steps were not run. Fix the problem, then run them by hand:"。
  - 说明 "Nothing was undone: if dev1 was created, it is in the VM list."。
  - 按钮 "Go to the VMs" 和 "Back to the form"。
- 这是全应用里最长的操作，首次可能要几十分钟，**请把这个进度视图设计成亮点**。

### 5.3 Exits（出口）

- **页面头部**：标题 "Exits"，下拉按钮 "Add exit"，两个选项：
  - **Agent exit (your own server)**：对话框，三个字段：
    - Name。
    - Server：格式 `host:port`，示例 `vps.example.com:18443`。说明："The address of a server running pvm exit-agent, as host:port."
    - Token：密码框。说明："It is handed to pvm in a private file and is not shown or logged."
    - 提示："Adding probes the connection, which can take up to two minutes."
  - **Cloudflare WARP**：对话框，只填 Name。说明："This registers a new Cloudflare WARP device and uses it as the exit."
- **出口列表**，每项显示：
  - 名称。默认出口带 `default` 徽章。
  - 地址，用等宽字体。
  - 时区和语言区域，例如 `Asia/Tokyo`。
  - 类型：Agent 或 WARP。
  - 被谁使用：VM 名称列表，以及路由规则的数量。
  - 操作：
    - **Test**：Agent 类型会弹出对话框，可勾选 "Also run a speed test"，结果显示在 Activity 中；WARP 类型直接测试。
    - **Set default**。
    - **Remove**（红色）：确认对话框。如果仍有默认出口、VM 或路由规则在用，要列出来并警告。
- **固定的 `direct` 项**：表示 "the host's network"，类型为 "No exit"，只能设为默认。
- **默认出口已不存在时**：页面顶部显示警告，说明跟随默认出口的 VM 现在没有网络。
- **出口为空时**：说明 "An exit is a server the traffic of a VM leaves through; without one, VMs use the host's network."
- **"Exit of each VM" 分区**：每台 VM 显示当前出口（跟随默认时带 `default exit` 徽章），有一个 "Change exit" 操作。
- **更换出口对话框**：
  - 下拉选择新出口。
  - 警告："A running VM restarts to apply the new exit. Programs running in dev1 are stopped."
  - 如果当前出口已被删除，额外警告这台 VM 现在没有网络。

### 5.4 Workspaces（工作区）

- **工作区是什么**：把宿主机上的一个文件夹绑定到 VM 里的一个文件夹，VM 里的路径是 `/workspace/<id>`。终端和程序都从这个文件夹启动。
- **页面头部**：标题 "Workspaces"，主按钮 "New workspace"。
- **每个工作区显示**：
  - id。
  - 模式徽章：`mount`、`sync` 或 `copy`。只读时加 `read-only` 徽章。
  - 所属 VM。
  - 宿主机路径和 VM 内路径。
  - 上次传输时间。mount 模式没有传输时间，显示 "live"。
- **行内操作**，用图标：
  - **Folder**：在资源管理器里打开宿主机文件夹。
  - **Terminal**：打开终端，直接进入 VM 里对应的文件夹。
  - **App**：同 VM 的 App 对话框，但程序在这个工作区的文件夹里启动。
  - **Files**：在 VM 的文件管理器里打开这个文件夹。
  - **更多菜单**：
    - 非 mount 模式额外有 "Push to the VM" 和 "Pull from the VM"。
    - Remove（红色）："This removes the record of X only. The files in the host folder and in the VM are not touched."
- **新建工作区对话框**：
  - Id：小写字母、数字和 `-`。说明："The folder shows in the VM as /workspace/<id>."
  - VM。
  - Mode，三个选项：
    - mount - one shared copy: the VM works on the host folder
    - sync - a copy on each side, pushed and pulled
    - copy - uploaded once into the VM
  - Host folder：输入框加 "Choose..." 按钮，按钮会打开系统的文件夹选择器。
  - 复选框 "Read-only: the VM cannot change the host folder"，只有 mount 模式可选。
  - 复选框 "Let pvm start the VM if it is stopped, and upload more than 1 GiB"。
- **空状态**：说明 "A workspace binds a host folder to a folder in a VM, so terminals and apps start there."。没有 VM 时，主按钮改为 "Create a VM first"。

### 5.5 Settings（设置）

- **Default VM**：
  - 下拉选择 None 或某台 VM。说明："The VM that pvm commands use when none is named and the folder is not in a workspace."
  - 默认 VM 已被删除时显示警告。
  - 修改后立即生效，没有保存按钮。
- **At sign-in**：两个开关，改动立即生效：
  - "Start the pvm daemon when I sign in"
  - "Open PVM Desktop when I sign in"
- **About**：显示以下四项：
  - PVM Desktop 版本。
  - pvm daemon 版本。
  - Accelerator：`whpx` 或 `tcg`。
  - Data folder：数据目录。

  另有两条警告：
  - 两个版本不一致时，提示 `pvm upgrade` 只会替换 pvm.exe，要重新运行安装包才能让 PVM Desktop 也升级。
  - 有 VM 退回到软件模拟（TCG）时，提示 "N running VMs fell back to software emulation (TCG) ... They run much slower."

### 5.6 Activity（任务面板）

- **收起时**：一行，显示最近一个任务的标题和状态，例如 "Create dev1 - running"。没有任务时显示 "Nothing has run yet"。
- **展开后**：任务列表，最新的在上面。每个任务包括：
  - 标题和状态徽章：Running、Done 或 Failed。
  - 步骤列表。每步显示对应的命令（等宽字体）和状态，可以展开查看输出日志。正在运行和失败的步骤默认展开。
  - 失败时显示错误原因，以及剩余步骤的可复制命令。
- **"Clear finished" 按钮**：清掉已结束的任务。
- **关闭窗口时**：如果还有任务在运行，弹出确认框 "PVM Desktop is still working"，列出正在运行的任务，说明现在关闭的后果，并询问是否仍要关闭。

## 6. 数据接口（mock 请严格按此结构）

```ts
interface State { backend: "native" | "wsl"; daemon_healthy: boolean; daemon_stopped: boolean;
  app_version: string; daemon_version: string; accel: string; tcg_fallback: number; data_dir: string; }
interface VM { name: string; image: string; state: "stopped" | "starting" | "ready" | "failed";
  stage: string; hint: string; cpus: number; mem_mb: number; disk_gb: number;
  exit: string; exit_own: boolean; default: boolean; }
interface CatalogApp { id: string; name: string; description: string; homepage: string;
  source: "official" | "community"; window: boolean; start: string; }
interface Exit { name: string; kind: "agent" | "warp"; endpoint: string; tz: string; locale: string;
  default: boolean; vms: string[]; rules: string[]; }
interface Workspace { id: string; vm: string; mode: "mount" | "sync" | "copy"; host_path: string;
  guest_path: string; read_only: boolean; last_transfer: string /* RFC 3339 或空 */; }
interface Settings { default_vm: string; daemon_at_login: boolean; app_at_login: boolean; }
interface Image { spec: string; golden: boolean; }
interface CreateDefaults { user: string; cpus: number; mem_mb: number; disk_gb: number; exit: string; }
interface AppConfig { clipboard: string; drop: string; audio: string; } // 空字符串表示不改
interface Step { command: string; state: "pending" | "running" | "done" | "failed"; }
interface Task { id: number; title: string; steps: Step[]; done: boolean; ok: boolean;
  error: string; remaining: string[]; }
// 事件：task（Task 有更新）、task-output（{ id, step, line }，一行输出）
```

- **读取**：`State()`、`VMs()`、`InstalledApps(vm)`、`Catalog()`、`Exits()`、`DefaultExit()`、`Workspaces()`、`Settings()`、`Images()`、`CreateDefaults()`、`PickFolder()`。
- **操作**：都返回 `Promise<Task>`。
  - 后端拒绝执行时，Promise 以错误字符串 reject。
  - 执行了但失败时，resolve 一个 `ok: false` 的 Task。
  - 操作列表：
    - `StartDaemon`、`StartVM`、`StopVM`、`DeleteVM(name, confirmed)`。
    - `RunApp(name, target)`、`RunCatalogApp(name, id)`、`OpenFiles`。
    - `ConfigureApps(name, AppConfig)`、`InstallApps(name, ids)`。
    - `SetDefaultVM`、`ClearDefaultVM`。
    - `SetVMExit(name, via, confirmed)`、`SetDefaultExit(via)`。
    - `CreateVM(req)`。
    - `AddExit({name, server, token})`、`AddWarpExit(name)`、`TestExit(name, speed)`、`RemoveExit(name, confirmed)`。
    - `RunWorkspaceApp(id, target)`、`OpenWorkspaceFiles(id)`、`SyncWorkspace(id, "push"|"pull")`、`CreateWorkspace(req)`、`RemoveWorkspace(id, confirmed)`。
    - `SetDaemonAtLogin(on)`。
- **不产生任务的操作**：返回 `Promise<void>`，有 `OpenTerminal(name)`、`OpenWorkspaceTerminal(id)`、`OpenWorkspaceFolder(id)`、`SetAppAtLogin(on)`。

**mock 数据**只能使用以下示例值，不要出现真实域名、IP 或个人路径：
- VM：`dev1`（ready，默认 VM）、`dev2`（stopped）、`apptest`（starting，阶段 ssh）、`broken`（failed，带 hint）。
- 出口：`my-exit`，agent 类型，地址 `vps.example.com:18443`；`my-warp`，WARP 类型。
- 工作区：`my-project`（mount 模式），宿主机路径 `C:\Users\me\projects\my-project`；`notes`（sync 模式）。
- 任务：一个运行中的 Create 任务（含滚动日志），一个已完成的任务，一个失败的任务。

## 7. 希望得到的产出

1. 一份简短的设计说明：色板和语义色、字号层级、间距、圆角、阴影、动效时长，全部写成 CSS 变量。
2. 一个可交互的原型，包含：
   - 五个页面。
   - 第 4.1 节的四种全屏状态。
   - 所有对话框和菜单。
   - Activity 面板的展开和收起。
   - toast。
   - 浅色和深色两套主题。
3. 代码结构建议对应现有模块：`api.ts`（接口类型）、`mock.ts`、`dom.ts`（组件函数）、`icons.ts`、`vms.ts`、`create.ts`、`exits.ts`、`workspaces.ts`、`settings.ts`、`tasks.ts`、`main.ts`、`style.css`。
4. 标出哪些地方需要接真实后端，例如把 mock 换成 `window.go.desktop.App`，把事件换成 `window.runtime.EventsOn`。
