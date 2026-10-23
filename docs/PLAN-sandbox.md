# 沙箱：Windows 和 macOS

日期：2026-10-23。分支：`dev/sandbox`（从 main 开出，和 `dev/v0.3` 互不依赖）。做完、用户同意后合并回 main，作为 v0.2.x 发布，再把 main 合并进开发中的大版本分支。

## 现状

- **Linux**（`kernel/src/sandbox.c`）：执行器 nano-exec 在执行工具前，给自己和之后的所有子进程套上限制：
  - Landlock：读写都按清单放行。管理员额外拿到 `ro: ["/"]`，能读整台机器；访客只能读工作目录和系统目录。
  - seccomp：不能建 unix socket、不能用 io_uring。
  - no_new_privs。
- **Windows**（`sandbox_win.c`）和 **macOS**（`sandbox_stub.c`）：空实现。`sb_landlock_abi()` 返回 0，网关就按“没有沙箱”处理：自动模式下的 Shell 也要审批；Write / Edit 的路径由执行器自己检查。
- 权限规则（用户定的，不变）：工作目录读写，外面只读，写外面要提权（`elevated`）并审批。

## 目标

| | 写：只能写工作目录、临时目录、缓存目录 | 读：只读清单里的路径（访客） | 网络 |
|---|---|---|---|
| Linux（已有） | ✅ Landlock | ✅ Landlock | 不限制 |
| Windows（本计划） | ✅ 写受限令牌 + 目录 ACL | ❌ 做不到（见下），访客 Shell 继续审批 | 不限制 |
| macOS（本计划） | ✅ Seatbelt | ✅ Seatbelt | 不限制 |

做完后，Windows 和 macOS 上管理员会话在自动模式下的 Shell 不用再审批，和 Linux 一样。

## Windows：写受限令牌 + 合成 SID + 工作目录 ACL

参考 OpenAI Codex 的“不提权”Windows 沙箱（`openai/codex` 仓库 `codex-rs/windows-sandbox-rs`，Apache-2.0；设计说明见文末链接）。它不需要管理员，正好对上“读全盘、只写工作目录”。

### 原理

1. **合成 SID**：第一次用时随机生成一个 `S-1-5-21-a-b-c-d` 形式的 SID，存在 `~/.nano-agent/sandbox.json`（Windows 上是 `%USERPROFILE%\.nano-agent`）。它不对应任何真实账户，只用来标记“沙箱可以写的地方”。
2. **给可写目录加 ACE**：在每个可写目录（工作目录、这个会话的临时目录、共用缓存目录）的 DACL 里加一条“允许这个 SID 读、写、执行、删除”。用对象继承、容器继承（OI|CI），并且传播到已有的子文件。
3. **写受限令牌**：从当前用户的令牌派生，`CreateRestrictedToken(DISABLE_MAX_PRIVILEGE | LUA_TOKEN | WRITE_RESTRICTED)`，限制 SID 列表是：合成 SID、当前登录会话 SID、Everyone。
   - 写受限令牌只在**写**的时候额外核对限制 SID，读照常按用户身份。所以读全盘不受影响；写的时候，目标的 DACL 必须同时允许用户本人和某个限制 SID，基本上只有加了 ACE 的目录能写。
   - 再用 `SetTokenInformation(TokenDefaultDacl)` 设默认 DACL，让沙箱里新建的对象只对这个登录会话开放（Codex 也这么做）。
4. **用这个令牌启动执行器的内层**（见下面的“两段式执行器”）。Shell 命令都是内层的子进程，自动继承受限令牌。

`CreateProcessAsUserW` 用的是从自己令牌派生的受限令牌，不需要额外特权。

### 两段式执行器

Linux 上是执行器给**自己**套 Landlock。Windows 上进程不能换掉自己的主令牌，所以 nano-exec.exe 分两段：

1. **外层**（普通令牌）：
   1. 读请求 JSON，算出可写目录；
   2. 确保这些目录有 ACE（已有就跳过）；
   3. 建写受限令牌；
   4. 用它启动自己的内层（`nano-exec.exe --inner`）。内层放进一个 Job Object（`KILL_ON_JOB_CLOSE`），把请求写进内层的 stdin，内层的结果原样转给内核。
2. **内层**（受限令牌）：照常执行工具，Write / Edit / Shell 都在受限令牌下。

- 取消：内核关掉 stdin（`watch_stdin`）→ 外层结束 Job，内层和它的子进程一起被杀。
- 临时目录：内层环境里的 `TEMP` / `TMP` 指向会话临时目录（已经加了 ACE），不用用户自己的 `%TEMP%`。
- 内核和网关不用改执行方式。执行器本来就是一次一个进程。经 SSH 在远程 Windows 上运行执行器时，行为也一样。

### ACE 怎么加、什么时候去掉

- **只加不改**：用 `GetNamedSecurityInfoW` 读 DACL，已经有这个 SID 的 ACE 就跳过；没有就 `SetEntriesInAclW` 加一条，再 `SetNamedSecurityInfoW` 写回（它会把可继承的 ACE 传播给已有的子文件）。
- **很慢，只在第一次**：第一次给一个大目录加 ACE 要遍历整棵树，Codex 也提到这一点。所以：
  - 已经加过 ACE 的根目录记在 `sandbox.json`，以后直接跳过；
  - 第一次超过 2 秒时，工具结果里提示“第一次给工作目录设置沙箱权限，花了 N 秒”；
  - 网关在用户添加工作目录时，就可以提前让那台机器加好 ACE（可选，放在第二步做）。
- **从别处挪进来的文件**：移动文件时保留原来的 ACL，所以移进工作目录的文件没有这个 ACE，写的时候会被拒。
  - 碰到“拒绝访问”时，给模型的错误里提示这种可能；
  - `nano-kernel sandbox repair <目录>` 重新传播一次。
- **去掉**：工作目录从列表里删除时，网关通知那台机器去掉 ACE；另外提供 `nano-kernel sandbox clean`，去掉所有加过的 ACE（卸载时也调用）。
- **不支持 ACL 的地方**（FAT32、U 盘、部分网络盘和 OneDrive 占位文件）：加 ACE 失败时，这个工作目录退回“没有沙箱”（这一次 Shell 要审批），工具结果里说明原因。不要让命令因此直接失败。

### 已知限制（写进文档，按“安全靠人”不去硬堵）

- **读**不受限制。写受限令牌只管写。访客会话在 Windows 上照旧：Shell 要审批，Read / Grep 由执行器检查路径。
- **任何人都能写的目录**仍然能写，因为限制 SID 里有 Everyone，这是 Codex 的取舍，为了兼容大量工具。可以在 doctor 里列出工作目录上级里有没有这种目录，提示一下。
- **网络**不受限制，和 Linux 现在一样。
- **兼容性**：有的环境下 `CreateRestrictedToken` 会失败（企业策略），有的杀毒软件会拦 `CreateProcessAsUserW`。失败时退回“没有沙箱”，探测结果里带上原因，`nano-kernel status` 和网页设备页显示出来。
- 更强的方案（专用的本地沙箱账户 + 防火墙规则，读和网络都能管）要管理员权限，放到以后，不在这一版做。

### 探测和协议

- `nano-exec --probe` 加 `sandbox`：`"landlock"` / `"win-restricted"` / `"seatbelt"` / `""`，以及 `confine: {"write": true, "read": false}`。保留 `landlock` 字段，兼容旧网关。
- Windows 的探测要真的试一次：建受限令牌、启动内层跑一个空请求，成功才报 `win-restricted`。
- **判断**改成看 `confine`，不再只看 `landlock`：
  - `write` 为真：管理员会话在自动模式下 Shell 不审批；
  - 访客会话的 Shell 免审批，还要求 `read` 为真。
- **现在按 landlock 判断的地方**：
  - 内核 `main.c`：探测结果存进 `k->landlock`；hello 的 `sandbox`；按工作目录决定 `calls[i].sandbox`。
  - 网关 `app/nodes.go`：`info.Landlock`、`hostFor`、`mount.landlock`、`WorkspaceSpec.Landlock`。
  - `proto.Sandbox` 和 `proto.WorkspaceSpec`。

  每个工具调用是不是“在沙箱里”，由执行的那台机器决定，要按“这个调用的身份（管理员 / 访客）需要哪种限制”来算。
- 内核 hello 的 `landlock` 改成同样的 `sandbox` 和 `confine`。协议版本加一，内核和网关会自动同步版本。
- `nano-kernel status`、网页设备页、拓扑：显示“沙箱：Windows 受限令牌（只限制写）”这类文字。

## macOS：Seatbelt

macOS 自带 Seatbelt 沙箱：`sandbox-exec` 命令和 libsystem_sandbox 里的 `sandbox_init_with_parameters`。苹果在手册里标成“已废弃”，但一直能用，Codex、Chromium 都在用，目前没有给命令行程序的替代品。

### 做法

- **执行器给自己套**，和 Linux 的 Landlock 一样：`sb_apply()` 生成 SBPL 配置，调用 `sandbox_init_with_parameters`。套上以后对自己和所有子进程生效、不能撤销。
  - 这个函数不在公开头文件里，用 `dlsym(RTLD_DEFAULT, "sandbox_init_with_parameters")` 取。zig 交叉编译时不用链接它。
  - 取不到或者调用失败：退回“没有沙箱”，探测里带上原因。
  - 备选：外层用 `/usr/bin/sandbox-exec -p <配置> -D… nano-exec --inner` 启动内层，和 Windows 一样两段式。只在 dlsym 方式不可行时才用。
- **配置**参考 Codex 的 `seatbelt_base_policy.sbpl`（Apache-2.0，文件开头注明来源；它本身参考了 Chromium 的配置）：
  - `(deny default)`；
  - 允许 `process-exec`、`process-fork`、只对同一沙箱内的进程发信号；
  - 允许读几个常用的 sysctl、`/dev/null`、pty；
  - 放行 `com.apple.system.opendirectoryd.libinfo` 等用户信息查询；
  - 网络和 TLS 需要的那几个 mach 服务（Codex 的 `seatbelt_network_policy.sbpl`）照样放行，因为我们不限制网络。
- **读和写**：
  - 读：管理员 `(allow file-read*)` 全盘读；访客只给工作目录、系统目录和 PATH 里的目录（和 Linux 的 SYSTEM_RO 对应，换成 macOS 的路径：`/usr`、`/bin`、`/System`、`/Library`、`/opt/homebrew`、`/private/etc` 等）。
  - 写：`(allow file-write* (subpath (param "WRITABLE_ROOT_0")) …)`，包括工作目录、会话临时目录、缓存目录。
  - 路径要先 `realpath`：`/tmp` 实际是 `/private/tmp`，`/var` 实际是 `/private/var`。
- **已知坑**：
  - Seatbelt 默认挡掉大部分 mach IPC，有的程序（比如带 GUI 的、Chromium / Playwright 这类会自己再套一层沙箱的）在里面跑不起来。碰到时，在工具结果里提示可以用 `elevated` 在沙箱外执行（要审批）。
  - 沙箱拒绝时，内核日志里能看到违规记录，可以帮助调整配置。
- **测试**：本机没有 Mac，在 GitHub Actions 的 macOS runner（CI 已有）上跑。测试先在 `dev/sandbox` 分支上跑通。

## Linux

不变。顺手把探测改成新的 `sandbox` / `confine` 字段：Landlock 可用时是 `"landlock"`，读写都限制；Landlock 不可用（老内核、WSL1）时为空。

## 测试

在现有的 `kernel/tests/test_exec.py`（Linux、macOS）和 `test_exec_win.py`（Windows：本机 WSL interop + CI 的 Windows runner）里加功能测试。只在临时目录里做：

- 工作目录里 Write、Edit、Shell 新建、修改、删除文件都成功；修改在加 ACE 之前就存在的文件也成功（检验传播）。
- Shell 写工作目录外面的临时“外部目录”被拒，并且错误信息看得懂；读外部目录成功（管理员）。
- macOS 上，访客读外部目录被拒。
- `TEMP` / `TMPDIR` 能写；Shell 里跑 git（init、add、commit）、python（有的话）正常。
- 取消（关闭 stdin）后，内层和它的子进程都被结束（Windows 用 Job Object）。
- 第二次执行时不再重复加 ACE（`sandbox.json` 里有记录）。
- `nano-kernel sandbox clean` 后，ACE 全部去掉。
- 探测结果：Windows 的 `win-restricted` / 写限制、macOS 的 `seatbelt` / 读写限制。网关据此决定自动模式下 Shell 要不要审批（Go 单元测试）。
- 加 ACE 失败（用一个不支持 ACL 的路径模拟，或者测试开关强制失败）时，退回“没有沙箱”，而不是直接报错。

## 分步

1. **探测和网关判断**：`sandbox` / `confine` 字段，网关按 `confine` 决定审批；Linux 先接上新字段。只改协议和判断，不改行为。
2. **Windows 两段式执行器**：合成 SID、ACE、受限令牌、Job、`sandbox.json`、退路；`test_exec_win.py` 本机通过。
3. **Windows 收尾**：`nano-kernel sandbox repair|clean`、卸载时清理、删除工作目录时去掉 ACE、doctor 和状态显示；CI 的 Windows runner 通过。
4. **macOS Seatbelt**：dlsym 方式、SBPL 配置、读写测试；CI 的 macOS runner 通过。
5. **文档**：README（沙箱一节，三个平台各自能限制什么、不能限制什么）、HANDOFF、ARCHITECTURE。

每一步 `make test` 全过后，单独提交到 `dev/sandbox` 并更新 HANDOFF。不在开发分支上打标签。

## 资料

- OpenAI，Building a safe, effective sandbox to enable Codex on Windows：https://openai.com/index/building-codex-windows-sandbox/
- Codex 源码：https://github.com/openai/codex ，`codex-rs/windows-sandbox-rs`（`token.rs`、`acl.rs`、`cap.rs`）、`codex-rs/sandboxing`（`seatbelt.rs`、`seatbelt_base_policy.sbpl`、`seatbelt_network_policy.sbpl`）
- Codex Windows 沙箱架构解读：https://codex.danielvaughan.com/2026/05/14/codex-cli-windows-sandbox-engineering-restricted-tokens-acls-elevated-architecture/
- Windows 沙箱机制对比：https://agentpatterns.ai/security/windows-sandbox-primitives-coding-agents/
- macOS Seatbelt 现状：https://github.com/openai/codex/issues/215 、https://pierce.dev/notes/a-deep-dive-on-agent-sandboxes
