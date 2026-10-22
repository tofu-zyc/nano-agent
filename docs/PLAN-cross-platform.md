# 计划：Windows 内核、macOS 内核、发布安装包

日期：2026-10-22。用户要求：先支持 Windows 内核，macOS 顺手做；能发布 release 安装包，别的机器不用装编译器、不用从源码编。
分阶段做，每个阶段 `make test` 全过后单独提交，并更新 docs/HANDOFF.md。上一份计划（docs/PLAN-next.md）里的用户原则仍然有效：安全靠人，不加额外限制；所有文件放在 ~/.nano-agent。

## 现状（2026-10-22 核对过）

- 内核和执行器大约 4500 行 C，只能在 Linux 上跑。和 Linux 绑死的只有这几处：
  - `sandbox.c`：Landlock 和 seccomp；
  - `nexec_main.c`、`conf.c`：用 `/proc/self/exe` 找自己的路径；
  - `nexec_proc.c`：`PR_SET_PDEATHSIG`。
  
  其余都是 POSIX 的：poll、socket、fork/exec、管道。`core.c` 是 sans-I/O 的状态机，可以原样复用。
- Grep 用 rg，找不到 rg 时退回 grep。Windows 上两个都没有。
- 没有沙箱的情况已经能处理：landlock=0 时 Write/Edit 由执行器检查路径，Shell 在 auto 模式下也要审批。新平台第一版就走这条路，**不用做沙箱**（符合“安全靠人”）。
- 网关能用 `GOOS=windows` 和 `GOOS=darwin` 编译通过，但在 Windows 上实际用不了：
  - 本机执行靠 Linux 的 nano-exec；
  - SSH 用 OpenSSH 的 ControlMaster，Windows 版 OpenSSH 不支持；
  - 安装脚本是 bash。
  
  这次不做 Windows 网关，放在最后的“以后”里。
- 开发机是 WSL（Fedora 43）。**在 WSL 里可以直接运行 Windows 的 .exe**：`/mnt/c/Windows/System32/cmd.exe` 能用。运行时把当前目录放在 /mnt/c 下面，否则 cmd 会抱怨 UNC 路径。Windows 版本可以在 WSL 里交叉编译，再在真实的 Windows 上测。
- 本机没有 mingw，也不能静态链接 glibc（没装 glibc-static），不能用 sudo 装东西。交叉编译用 **zig**（`zig cc`）：
  - 下载固定版本的官方压缩包，放到 `build/tools/`（加进 .gitignore），写一个脚本自动下载；
  - 目标：x86_64-linux-musl、aarch64-linux-musl（静态）、x86_64-windows-gnu、aarch64-macos、x86_64-macos。
- 仓库**没有 git remote**。gh 已经登录（账号 Paste-Fried-tofu）。**创建 GitHub 仓库、推送、发布 release 都要等用户决定**（公开还是私有：私有仓库的 release 不能匿名下载，一行命令安装就用不了）。决定之前，只做本地的 `make release` 和 workflow 文件，**不创建仓库，不推送**。

## 阶段 1：平台层 + Windows 执行器（nano-exec.exe）

- 先把系统相关的代码收到一个小的平台层后面：进程、管道、路径、找自己的路径、子进程跟随退出、沙箱。POSIX 一份（Linux 和 macOS 共用，个别地方用 `#ifdef __APPLE__`），Windows 一份。`core.c` 和工具的定义不动。
- Windows 执行器：
  - **全程用宽字符 API**（`CreateFileW`、`CreateProcessW` 等）。协议和 JSON 都是 UTF-8，在边界上做转换。
  - **路径**：
    - 认得 `C:\x`、`C:/x`、`\\server\share`，`~` 指 `%USERPROFILE%`；
    - 判断在不在工作目录里时不区分大小写；
    - 用 `GetFinalPathNameByHandleW` 解析符号链接和 junction 之后再比较。
  - **Read / Write / Edit / ls**：行为和 Linux 一样。Edit 遇到 CRLF 文件时，把 old_string 和 new_string 里的 `\n` 换成 `\r\n` 再匹配和替换。Write 新建文件用 `\n`；覆盖已有文件时沿用原来的换行风格。
  - **Shell**：用 PowerShell，有 `pwsh` 优先用，没有就用 `powershell.exe`。参数 `-NoProfile -NonInteractive -Command`，输出强制 UTF-8（设置 `[Console]::OutputEncoding` 和 `$OutputEncoding`）。返回退出码。超时和取消用 **Job Object**（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`），连带子孙进程一起杀，代替 Linux 上的进程组加 PDEATHSIG。
  - **Grep**：用发布包里自带的 rg（放在执行器旁边，优先用它）。Linux 和 macOS 也一样优先用自带的。
  - **环境变量清理**：Windows 至少要留 `SystemRoot`、`ComSpec`、`PATHEXT`、`PATH`、`USERPROFILE`、`TEMP`、`TMP`、`LOCALAPPDATA`、`APPDATA`，否则很多程序跑不起来。
  - **沙箱**：报 landlock=0，也就是无沙箱，走现有的无沙箱规则。提权（elevated）的逻辑照旧。
  - 不能当工作目录的检查：`%USERPROFILE%\.nano-agent` 和执行器所在的目录。
- **测试**：把 test_exec.py 改成也能跑 Windows 版。在 WSL 里通过 interop 运行 `nano-exec.exe`，测试目录放在 Windows 的临时目录下（用 `wslpath -w` 转路径）。要覆盖：读写、越界、CRLF 的 Edit、中文路径和中文内容、PowerShell 输出编码、超时后杀掉子进程、取消、rg。

## 阶段 2：Windows 内核（nano-kernel.exe）+ 网关适配

- **内核**：
  - Windows 上只用 TCP 连网关，用 Winsock。
  - 主循环保持 WSAPoll。执行器子进程的输出用读线程读，读完通过一个回环 socket 唤醒主循环，这样主循环的结构和 Linux 一样。
  - run、skill_put、ls、配对、日志、退避重连都和 Linux 一致。kernel.conf 的解析和路径用平台层。
- **协议 v8**：hello 里带上 `os`（linux / windows / darwin）和 `arch`；SSH 服务器在探测时也记下 os。网关在拓扑、机器列表、`/node` 里显示系统。
- **修一个现有的问题（用户正是这个场景）**：网关现在把从 127.0.0.1 用 TCP 连上来的内核当成“本机”（`internal/kernel/hub.go`，IsLoopback）。WSL 里跑网关、Windows 上跑内核时，Windows 内核会经过 WSL 的 localhost 转发，从 127.0.0.1 连进来，于是被当成本机，路径和执行就全错了。改成：只有 unix socket 连接，或者网关自己启动的本机内核（AllowToken），才算本机；或者再加上“os 和机器标识都和网关一致”。别的一律当作别的电脑。
- **路径：路径属于它所在的机器。** 网关多半跑在 Linux 上，内核和工作目录可能在任何系统上，所以网关不能用自己的规则去理解别的机器的路径。原则：**网关只保存、显示、转发路径，不解析；规范化、比较、包含判断都交给那台机器的执行器。**
  - **规范形式由那台机器给。** 添加工作目录时，网关本来就会先 ListDir（`AddWorkspace`）。改成用 ListDir 返回的 `Path` 当作保存的路径：那台机器解析过 `~`、相对路径和符号链接，Windows 上还是磁盘上的真实大小写。之后判断是不是同一个工作目录，直接比较字符串就行。机器不在线时先按原样保存，等它上线、第一次 ListDir 时再换成规范形式。
  - **网关要删掉或改掉的**（只有这几处）：
    - `app/workspace.go` 的 `cleanPath` 和 `expandHome`：别的机器上的路径不要再用 `filepath.Clean`，它会把 `C:\a` 当成一个奇怪的相对路径；
    - `workspace.go:175` 和 `workspaces.go` 的 `checkWorkspace`：现在要求路径以 `/` 或 `~` 开头，这会挡住 `C:\`。改成交给那台机器的 ListDir 判断，机器报错就是不合法；
    - 网关自己这台机器上的路径（本机执行、guard）照旧用 filepath，这是对的。
  - **“机器:路径”的拆法**：`parseWorkspace` 现在按第一个 `:` 拆，`C:\proj` 会被拆成机器 `C`、路径 `\proj`。改成拿已知的机器名去匹配前缀：以某个机器名加 `:` 开头才算指定了机器，比如 `laptop:C:\proj`。没有匹配上的，整串都是默认机器上的路径。网页的选择器本来就是分开传机器和路径的，不受影响。
  - **内核选工作目录**（`kernel/src/main.c` 的 pick_workspace，按全名、序号、只写机器、只写路径匹配）：WorkspaceSpec 加一个 `os` 字段。os 是 windows 时，比较路径不区分大小写，`\` 和 `/` 当成一样。这件事在内核里做，因为可能是 Linux 内核在给 Windows 上的工作目录选目标。
  - **工具参数里的路径**（Read、Write 等的 `path`）：一直都由执行工具的那台机器解析，Windows 执行器认 `C:\x`、`C:/x`、`~`、相对路径。网关和别的内核不碰。
  - **`~` 开头的约定路径**（比如技能文件放在 `~/.nano-agent/skills/<名字>`）：协议里保持 `~/…`，由每台机器自己展开。Windows 上展开成 `%USERPROFILE%\.nano-agent\…`。
  - **目录选择器**：请求 Windows 机器的 `/` 或空路径时，执行器返回盘符列表（C:\、D:\）。这也是那台机器自己决定的，网关不用特殊处理。
  - **混合场景都要通**：Linux 内核用 Windows 上的工作目录（网关通过 run 让 Windows 内核执行）；Windows 内核用 Linux 上的工作目录（Windows 内核把 exec 交给网关，网关在本机或 SSH 上执行）。
- **给模型的说明**：
  - Shell 跑在工作目录所在的机器上，可能是 sh，也可能是 PowerShell。工具描述要写成中性的：“Linux 和 macOS 上用 /bin/sh，Windows 上用 PowerShell，见环境说明”。描述必须固定不变（前缀缓存），不要按机器换描述。
  - envNote 里给每个挂载的工作目录写上系统和 shell，比如“laptop:C:\proj（Windows，PowerShell）”。变了才追加，和现在一样。
- **测试**：
  - Go：解析带盘符的工作目录、Windows 路径的包含判断、经过 127.0.0.1 TCP 连上来的远程内核不算本机。
  - 实测：在 WSL 里起演示网关（mock 模型），通过 interop 启动 `nano-kernel.exe` 连上来，配对，挂 `C:\…` 下的工作目录，跑一遍 Read、Write、Edit、Shell、Grep，以及 Skill 文件下发。

## 阶段 3：安装（先做本地安装，发布以后再说）

用户说先不发布，先把适配做好。这个阶段只做：
- 能在一台机器上交叉编译出各个平台的程序；
- 安装脚本能**从本地的包**安装（离线安装）；
- `install-kernel.ps1`（Windows 的安装、配对、开机自启、升级、卸载）。

下面列的 release 下载、GitHub Actions workflow、`SHA256SUMS`、一行命令从网上安装，**都挪到以后**。安装脚本里留一个下载地址的变量，先不用。

原来的完整设计留在下面，做到时参考：

### 发布安装包（以后）

- `scripts/fetch-zig.sh` 下载固定版本的 zig。`make release VERSION=x.y.z` 输出到 `dist/`：
  - 内核包，每个目标一个：`nano-kernel-<版本>-linux-x86_64.tar.gz`、`linux-arm64`、`macos-arm64`、`macos-x86_64`、`windows-x86_64.zip`。包里有 nano-kernel、nano-exec、rg、安装脚本和 VERSION。Linux 版是 musl 静态链接，不挑发行版。
  - 网关包：Go 交叉编译，`CGO_ENABLED=0`。目前只做 linux-x86_64 和 linux-arm64。包里带上**各个架构的 Linux 执行器**，网关往 SSH 服务器部署时直接上传对应架构的，**服务器上不用再现场编译**（现编作为没有对应架构时的兜底，保留）。
  - rg 用官方 release，从 ripgrep 的 GitHub 下载，固定版本，校验 sha256。
  - `SHA256SUMS`。
- **安装脚本**：
  - `install-kernel.sh`（Linux 和 macOS）：认出系统和架构，从 release 地址下载对应的包，校验 sha256，安装到 `~/.nano-agent/kernel`。包里自带脚本时，就直接用包里的文件（离线安装）。从源码编译只在没有对应的包时兜底。macOS 开机自启用 launchd 的用户 agent（`~/Library/LaunchAgents/`，卸载时删除）。
  - `install-kernel.ps1`（Windows，新增）：
    - 支持一行命令安装：`irm <地址>/install-kernel.ps1 | iex`，也支持离线：解压包后运行里面的 ps1。
    - 不需要管理员权限。装到 `%USERPROFILE%\.nano-agent\kernel`。
    - 问网关地址和名字，默认名字是计算机名。
    - 开机自启用“任务计划程序”（登录时启动，失败自动重启），不用服务。
    - 和 Linux 脚本一样读 kernel.log，显示确认码，网关报错时说清楚原因。
    - 支持 `-Uninstall`。再运行一次就是升级，保留配置和配对口令。
    - 提示：WSL 里的网关要让 Windows 能连上（device_listen 监听 0.0.0.0，或者靠 WSL 的 localhost 转发）。
  - 安装脚本默认的下载地址先写成一个变量：仓库定了之后，改一处就行。
- `make installer` 生成的单文件安装包（base64 嵌进源码），在 release 能用之后可以去掉。去之前先问用户。
- **GitHub Actions workflow**：推送 tag 时，在 ubuntu、macos、windows 三种 runner 上编译并跑测试，然后上传 release。文件先写好、提交，**不创建仓库、不推送**。
- **测试**：从 `dist/` 的包做离线安装，Linux 在临时 HOME 里装一遍（加一个总是失败的假 systemctl），Windows 通过 interop 运行 ps1 装到一个临时的 USERPROFILE。要覆盖：装、配对、升级、卸载。

## 阶段 4：macOS（顺手）

- 平台层的 POSIX 版补上 macOS 的部分：
  - 找自己的路径用 `_NSGetExecutablePath`；
  - 子进程跟随退出：执行器已经在监视 stdin，stdin 关了就杀进程组，确认 macOS 上够用；不够就在子进程里用 kqueue 的 `NOTE_EXIT` 盯着父进程；
  - 沙箱：第一版没有（landlock=0）。以后可以接 Seatbelt（`sandbox-exec`），它能表达“全盘可读、只写工作目录”。
- 这里没有 Mac：用 zig 交叉编译确认能编过，实际运行的测试交给 GitHub Actions 的 macos runner。HANDOFF 里要写清楚“还没在真机上跑过”。

## 以后（这次不做，做之前先问用户）

- Windows 上跑网关：PowerShell 安装脚本；SSH 没有 ControlMaster 时，每条命令单独连接（或者换成 Go 的 x/crypto/ssh）；终端聊天的 socket。
- Windows 沙箱（受限令牌或 AppContainer）、macOS 的 Seatbelt。
- 手机（Android 上的 Termux）：Shell 路径要改成 `$PREFIX/bin/sh`，没有 systemd。

## 约定（照旧）

- 用中文。实现保持直接，不堆防御代码。每个阶段都要 `make test` 全过；改了前端要 `make web`。
- 提交：在 main 分支，用 `git -c user.name=tofu-zyc -c user.email=286995380+tofu-zyc@users.noreply.github.com`，Co-Authored-By 写实际参与的人。提交前用 `git grep --cached -F -q` 确认本地的密钥没进暂存区，查的时候不要打印出来。
- 不要动用户的 `~/.nano-agent`、源码目录里的 gateway.toml 和 data/。不要用真实配置启动网关。演示用临时目录加 mock 模型，Windows 那边用一个临时的 USERPROFILE。杀进程前确认是你自己启动的（Linux 上看 /proc/PID/cwd，Windows 上看程序路径）。
- 下载的工具（zig、rg）固定版本、校验 sha256，放在 `build/tools/`，不提交。
