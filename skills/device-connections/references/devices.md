# 机器和工作目录：接入与排查

## 概念

- **网关**保存会话、上下文、模型配置和审批，是唯一有状态的地方。**内核**只跑 agent 循环，可以有好几个。
- **工作目录 = 机器 + 目录**，比如 `本机:~/proj`、`gpu-server:~/train`、`laptop:/mnt/d/work`。列表存在网关的 `data/workspaces.json`，★ 的是新会话的默认。
- **机器**：
  - 本机：网关所在的机器。
  - SSH 服务器：网关通过 SSH 连，上面只放执行器 `nano-exec`（在 `~/.nano-agent/`），不跑内核。
  - 电脑：装了内核、自己连到网关的 Linux 电脑或 Windows 的 WSL2。
- **怎么执行**：每一轮网关挑一个内核来跑（第一个挂载的工作目录那台机器上的优先）。每个工具调用：
  - 工作目录在这个内核的机器上：内核在本地执行。
  - 在别的机器上：交给网关，网关在那台机器上执行（本机、SSH 服务器网关自己执行，别的电脑请它的内核代为执行）。
  - 那台机器用不了（电脑关机、服务器断开）：这一轮不挂它；本机内核在时在 HOME 下工作，否则这一轮没有工具。
- 网关所在的机器上的内核由网关自己启动（叫 gateway），不用另外装。

## 挂工作目录

- 一个会话可以同时挂好几个工作目录，可以在不同的机器上；一个都不挂就在 HOME 下工作。标了 ★ 的新会话默认挂上。
- 你（管理员）可以直接用 `GatewayAdmin` 的 `workspace_add` / `workspace_mount` / `workspace_default` / `workspace_delete` 做，每次审批；先 `Gateway {"what":"workspaces"}` 看真实列表。
- 用户也可以 `/dir` 看挂了哪些和整个列表，`/dir 2` 挂上，`/dir -2` 取下，`/dir add gpu-server:~/proj` 添加并挂上（只写路径就是本机），`/dir none` 全部取下，`/dir default 2` 设为 / 取消新会话默认挂载，`/dir rm 2` 从列表删掉（不动机器上的文件）。
- 挂了好几个时，调用工具要用 `workspace` 参数写明在哪个上执行；同一台机器上挂的几个目录互相都能读写。
- 不能用的目录：包含 nano-agent 自己的（`~/.nano-agent` 里的网关数据、配置和内核/执行器）。`~` 本身包含 `~/.nano-agent`，不能挂；用 `~/proj` 这样的子目录。

## SSH 服务器

管理员在聊天里用 `GatewayAdmin`：

```json
{"action":"ssh_setup","name":"gpu","host":"192.168.1.10","user":"root","password":"…","path":"~/proj"}
```

- 主机地址，或网关这台机器 `~/.ssh/config` 里的别名（Include、跳板机、代理照常生效）。
- 名字：工作目录里用（`名字:路径`），不能叫“本机”，不能有空格、冒号、斜杠。
- 用户、端口留空就沿用 SSH 配置；密码或私钥路径（网关这台机器上的路径），都不填就用 ssh-agent / 默认密钥。
- 先 `Gateway {"what":"ssh"}` 看真实状态；`ssh_setup` 会返回在线 / 认证失败 / 超时和原因。失败不要反复重试。
- `ssh_save`、`ssh_delete`、`ssh_connect`、`ssh_disconnect` 分步操作。

连上后网关检查服务器架构：一样就把执行器传上去，不一样（比如 ARM）就传源码用服务器的 `cc` 现编。网关保持一条 SSH 主连接，每次执行工具复用它。密码在 `data/connections.json` 里加密，不进命令行参数和日志；首次连接接受新主机密钥，密钥变了就拒绝。

## 别的电脑：装内核

在网关那台机器的源码目录运行 `make installer`，把 `dist/nano-kernel-install.sh` 拷到那台电脑上：

```sh
sh nano-kernel-install.sh --gateway 192.168.1.2:7700
```

- 脚本会：检查并安装 cc / make（用 sudo 前先问）→ 编译 → 装到 `~/.nano-agent/kernel/` → 写 `kernel.conf` → 注册 systemd 用户服务（没有 systemd 时后台运行）→ 连网关，显示 6 位确认码。
- 用 `Gateway {"what":"pairing"}` 看确认码，再用 `GatewayAdmin {"action":"pair_allow","id":"…"}` 允许（或 `pair_deny`）；不要让用户去网页点。口令存在那台电脑的 `paired.token`，之后自动重连。
- 再运行一次是升级；`--name 新名字` 改名；`--uninstall` 卸载；`nano-kernel status|logs|restart|connect 网关地址` 管理（`nano-kernel help` 看全部）。
- 网关那边要先启用电脑接入（默认端口 7700）。Windows 在 WSL2 里装，工作目录用 `/mnt/c/...`、`/mnt/d/...`。

## 常见问题

- `Permission denied (publickey,password)`：SSH 认证失败，检查用户、密码、密钥路径和 SSH 配置。
- `Connection refused`、超时：地址、端口、网络、服务器上 sshd 有没有起来。
- 提示服务器上没有 `cc`：架构和网关不一样，要在服务器上装编译器（Debian/Ubuntu `build-essential`，Fedora `gcc make`）后重连。
- 安装脚本说“暂时没连上网关”：网关地址对不对（别的电脑访问不了 `127.0.0.1`，用网关的局域网 IP）、网页拓扑图「＋ 接入机器」节点里电脑接入有没有启用、防火墙放没放行端口；网关在 WSL 里时，Windows 还要做端口转发或开镜像网络。
- 提示“名字已经有电脑配对过了”：换个名字（重新运行安装脚本加 `--name`），或者用 `GatewayAdmin {"action":"pair_remove","node":"旧名字"}` 移除旧的。
- 被拒绝后内核退出；`nano-kernel restart` 重新申请。移除过的电脑会自动重新申请。
- 内核没在运行（`nano-kernel status` 显示“没有运行”）：`nano-kernel start`。Windows 上 status 提示“任务计划里没有 …”时，是旧版安装脚本用普通用户建任务失败了：重新运行一次安装命令就好（新版只给当前用户建任务，建不了自动改用「启动」文件夹）。
- 机器在线但工具被拒：看会话选的工作目录对不对、目录存不存在、权限模式是不是只读；写工作目录外要 `elevated: true` 并审批。
- Windows 盘目录读写报没有权限：旧版执行器 DrvFS 的问题，更新后重试，不需要 chmod 777 或关沙箱。
- 内核和协议本身不加密，跨网络请用 WireGuard / Tailscale / SSH 隧道。
