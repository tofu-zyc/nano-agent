# 计划：GitHub 发布、一行安装、更新、迁移到别的机器

仓库：https://github.com/tofu-zyc/nano-agent （公开，Apache-2.0）。
提交身份已经改成 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`：仓库本地的 git config 已设置，历史已改写。以后不要再用 foxmail 邮箱。

目标：任何一台机器一行命令装上网关或内核；网关和内核都能一条命令（或在聊天里、网页上）更新，坏了自动退回；网关能整个搬到另一台机器。

## 阶段 1：发布前准备（第一次推送之前）

- 加 `LICENSE`（Apache-2.0 全文，Copyright 2026 tofu-zyc）。README 开头写清楚这是什么、一行安装、文档入口。
- 用 gitleaks（固定版本下载到 build/tools，不要 sudo）扫**全部历史**和工作区。发现真密钥就停下来问用户（要改写历史）；测试里的假 key 加进 gitleaks 的允许列表。
- 确认 .gitignore 挡住了 gateway.toml、data/、dist/、build/、node_modules；工作区里不能有用户真实的配置被跟踪。
- 文档里写死的个人路径（/home/tofu 之类）、内网地址改成示例。

## 阶段 2：CI 和自动发布（GitHub Actions）

- `ci.yml`（push / PR）：
  - Linux：`make test`（WSL interop 相关的自动跳过）。
  - **Windows runner**：在真实 Windows 上原生跑 Windows 执行器测试和 Windows 内核端到端（现在的测试走 WSL interop，要改成在 Windows 上也能直接跑）；顺便实测 `install-kernel.ps1` 注册计划任务（WSL 里测不了的那一步）。
  - **macOS runner**：原生编译，跑内核测试、e2e、`install-kernel.sh` 的 launchd 安装。这是 macOS 第一次真的跑起来，修好跑出来的问题。
- `release.yml`（推 `v*` 标签）：从标签取版本号跑 `make release`，把所有包、`SHA256SUMS`、`install.sh`、`install.ps1` 上传到 Release。只有测试全过才发布。
- action 固定版本；权限最小（只有发布任务要 `contents: write`）。

## 阶段 3：一行安装

- `scripts/install.sh`（同时作为 Release 附件）：
  ```sh
  curl -fsSL https://github.com/tofu-zyc/nano-agent/releases/latest/download/install.sh | sh                   # 装网关（问一下）
  curl -fsSL .../install.sh | sh -s -- --kernel --gateway tcp:1.2.3.4:7700                                       # 只装内核
  curl -fsSL .../install.sh | sh -s -- --import nano-agent-export.tar.gz                                          # 装网关并导入旧数据（阶段 5）
  ```
  认系统和架构，下载对应的包，用 `SHA256SUMS` 校验，解压到临时目录，再调用包里现有的 `install-gateway.sh` / `install-kernel.sh`。`curl | sh` 时从 /dev/tty 提问；没有终端就要求 `--yes`。`--version x.y.z` 装指定版本。
- Windows（只装内核）：
  ```powershell
  & ([scriptblock]::Create((irm https://github.com/tofu-zyc/nano-agent/releases/latest/download/install.ps1))) -Gateway tcp:1.2.3.4:7700
  ```
- 下载地址能用 `NANO_DOWNLOAD_URL` 覆盖（国内访问 GitHub 慢时换镜像）。
- 网关包除了 Linux x86_64/arm64，顺手出 macOS 的（Go 交叉编译 + 已有的 macOS 内核），成本低就做。
- 网页「设备」里添加电脑时，直接给出 Linux/macOS 和 Windows 的一行安装命令，网关地址已经填好；网关内核监听还是 unix 时提示要先改成 tcp。

## 阶段 4：更新

**网关**

- `nano-gateway update [--check] [--version x.y.z]`；聊天 `/update`（管理员）；网页上“有新版本 x.y.z”和更新按钮；GatewayAdmin `update`（要审批）。
- 每天查一次最新版本（配置可关），只提示，不自动装。status、网页、`/status` 里显示。
- 过程：下载自己系统和架构的包 → 校验 SHA256 → 备份 gateway.toml 和 data/ 下的 JSON 到 `~/.nano-agent/backup/<旧版本>-<时间>/` → 换 bin/、lib/，保留上一版 → 用**新程序**检查配置 → 自重启到新程序。
- **自动退回**：新版本在限定时间内没有正常起来（自己报告健康），就换回上一版并启动，在发起的会话里说明原因。systemd 和后台进程两种模式都要实测；e2e 用一个故意起不来的假新版本测退回。
- 包里自带的 personas/ 和 skills/：用户没改过的才更新（记下上次安装时的哈希），改过的保留，新版本写成 `.new` 并提示。
- 本机内核和执行器随网关一起更新；SSH 服务器上的执行器本来就按版本号上传，确认更新后会自动换新。

**内核（装在别的电脑上的）**

- hello 里带上内核版本（协议没有就加，按协议升级规则处理）；网关显示每个内核的版本，落后时提示。
- `nano-kernel update`（Linux/macOS/Windows）：下载、校验、走现有安装脚本的升级路径（保留 kernel.conf 和 paired.token），重启服务。
- 网关能让内核自己更新：网页每台电脑一个更新按钮、GatewayAdmin 动作（要审批），通过协议发给内核执行。更新失败内核要能退回上一版并重新连上。

## 阶段 5：把网关搬到另一台机器

- `nano-gateway export [-o 文件]`：打包 gateway.toml、data/（不含日志、run/ 和缓存）、`secret.key`、personas/、skills/，权限 0600。输出时说明：**这个文件能解开所有密钥，当密码一样保管**。在源码目录里跑的网关也要能导出：data 目录读配置，secret.key 从 NANO_HOME 拿。
- 导入：`install.sh --import 文件` 或新装好后 `nano-gateway import 文件`。目标 `~/.nano-agent` 已有数据时拒绝，`--force` 时先备份。
- `--old-local-as <名字>`：旧网关上“本机”的工作目录改记到这个机器名下（旧机器以后装内核用这个名字连过来，工作目录和会话就都还能用）。
- 导入后的检查（doctor 也查）：路径不存在的“本机”工作目录；配对过的内核要改连新地址（列出来）；内核监听是 unix 时提示其他电脑连不上；网页只监听 127.0.0.1 时说明怎么从别的设备访问。

## 阶段 6：第一次发布

- 用户在仓库 Settings → Collaborators 邀请 Paste-Fried-tofu；用这台机器上的 gh 接受邀请（`gh api user/repository_invitations`）。邀请还没到就停下来告诉用户。
- 推 main，等 CI 全绿；打 `v0.1.0` 标签，等 Release 生成。
- 用真实 Release 实测：WSL 里临时 HOME 一行装网关、导出导入、装内核并配对、更新到一个测试版本再退回；Windows 上临时 USERPROFILE 一行装内核。测试用的预发布版本测完删掉。
- README 里的安装说明以真实 Release 为准。

## 约定

- 只推到 tofu-zyc/nano-agent 的 main。第一次推送之后**不再改写历史、不强推**，除非用户要求。
- 每次推送前：gitleaks 扫要推的提交；`git grep --cached -F -q` 确认用户本地的密钥没进暂存区（不要打印密钥）。
- CI 没过不发 Release。
- 不启动用户真实的配置；测试用临时 NANO_HOME / HOME / USERPROFILE 和 mock 模型；不动用户的 ~/.nano-agent、仓库里的 gateway.toml 和 data/、真实的 nano-gateway.service。
- 每个阶段 `make test` 全过后单独提交，更新 HANDOFF.md。计划没写清、又会影响用户使用的取舍，先问用户。
