# 电脑与服务器接入

## 接入后如何工作

网页左侧打开“设备”。机器上线后会出现在拓扑和会话设置中；在会话里选工作机器，再添加它上面的工作目录。聊天也可以用 `/node <名字>` 和 `/dir add <路径>`。机器换了，原来机器的目录会清掉，避免把路径解释在错误的机器上。

会话、模型配置、聊天记录和审批仍由网关统一保存。电脑或服务器断开时，该机器正在执行的任务会停止；其他在线内核可以接替聊天，文件操作等原机器恢复后再继续。

## Windows 电脑

1. 在网关的“设备 → 电脑启动包”中启用电脑接入，默认端口 7700，可展开设置改地址。现有本机 Unix socket 继续使用，无需重启网关。
2. 填写网关可达的 IP 或域名和端口，下载 Windows 包，解压到电脑上。
3. 双击 `start.cmd`，填写网关地址、电脑名字，选择一个已安装的 WSL2 环境。包内已带上连接口令，可在界面修改。
4. 连接后保持窗口运行；Ctrl-C 或关闭窗口可停止。再次打开会沿用上次填写的配置。

这一版 Windows 使用 WSL2，尚未提供原生 Win32 内核。未安装 WSL 时先运行 `wsl --install`。Windows 工作目录在会话中使用 `/mnt/c/项目`、`/mnt/d/项目` 等路径。启动器会把程序放到 WSL 的 `~/.local/share/nano-agent/client/`，不会把安装目录当作工作目录。

如果网关也在 WSL 中，其他电脑必须能访问它的接入地址；WSL 的网络模式、Windows 防火墙或端口转发也要允许这个端口。可以使用私有网络地址或 SSH 隧道。TCP 协议自身不加密；SSH 服务器接入不需要这个 TCP 端口。

可选命令行参数：

```powershell
.\start.ps1 -NoUI -Gateway tcp:192.168.1.2:7700 -Node windows-pc -Distro Ubuntu
```

`-InstallDir` 可指定 WSL 内的安装目录；不指定时使用上述默认目录。

## Linux 电脑

下载 Linux 包，解压后运行：

```sh
sh start.sh
```

按提示填写网关地址、口令和电脑名字。之后可用 `sh start.sh --saved` 直接使用已保存配置。

安装包包含当前构建的二进制和完整 C 源码。二进制不能在目标机器运行时，自动用该机器的编译器构建；需要 `cc`、`make` 和 `tar`。Ubuntu/Debian 可安装 `build-essential`，Fedora 可安装 `gcc make`。工作目录与安装目录分开。

## SSH 服务器

在“设备 → 添加服务器”填写：

- 主机地址，或网关 `~/.ssh/config` 中的别名；别名下拉列表也会读取 `Include`。
- 在会话里显示的名字，不能与在线内核重名。
- 用户和端口；留空沿用 OpenSSH 配置，未配置的端口通常为 22。
- 密码，或私钥路径；两者留空时使用 SSH 配置、默认密钥或 ssh-agent。密钥路径是网关所在机器上的路径。
- 可选“网关启动时连接，掉线后重连”。

点击“保存并连接”。网关先通过 SSH 上传程序和配置，目标机不能运行包内程序时自动从源码构建，再通过另一条 SSH 会话启动 `gateway = stdio` 的内核。内核通信全程走 SSH，无需服务器反向连接网关或开放额外端口。SSH 配置中的跳板机、代理和密钥设置由 OpenSSH 正常处理。

管理的程序安装在服务器 `~/.local/share/nano-agent/managed/<连接ID>/`。连接密码只存在网关 `data/connections.json` 和私有认证目录中，文件权限 0600；不会写进 SSH 参数、接口列表或日志。首次连接采用 OpenSSH `accept-new`，主机密钥变化时会拒绝连接。

连接状态包括未连接、连接中、已连接、断开中和失败原因。运行中修改设置需要先断开。点击“断开”会关闭 SSH 通道，服务器内核收到 EOF 后停止工具并退出；已部署的程序保留，方便下次连接。删除记录清除网关保存的密码，不删除服务器文件。

## 旧的内核侧 SSH 配置

原来的 `kernel.conf` 配置仍可使用：

```text
remote = prod deploy@prod.example.com
```

这种模式由某台内核连接服务器，服务器只运行执行器，技能从负责连接的内核上传。网页管理的 SSH 连接则在服务器上启动自己的内核，使用服务器安装目录的 `skills/`。两种模式可以并存。

## 常见问题

- `Permission denied (publickey,password)`：SSH 认证失败，检查用户、密码、密钥路径与 SSH 配置。
- `Connection refused`、连接超时：检查地址、端口、网络与 SSH 服务是否启动。
- 提示缺少 `cc` / `make`：目标机不能运行包内程序，需要安装构建依赖后再连接。
- 节点在线但工具被拒：确认会话选对了机器、目录存在，且工作目录不包含 nano-agent 安装目录。
- Windows 盘目录 `ls`、`Read` 都提示没有权限，但系统目录权限正常：旧版存在 DrvFS/9p 的 Landlock inode 问题，更新执行器后重试，不需要 `chmod 777` 或关闭沙箱。
- 手机上执行工具暂未实现；本阶段只支持 Linux 和 Windows WSL2。

## 管理接口

接口由网页登录会话认证。写请求必须有 `X-Nano: 1`，使用已经授权的管理客户端；聊天 agent 的普通 Shell 权限不会自动变成网关管理权限。

| 方法与路径 | 作用 |
|---|---|
| `GET /api/devices` | 连接列表、SSH 别名、实际电脑监听地址、主监听地址和构建架构；不返回密码 |
| `POST /api/devices` | 新建或修改 SSH 连接，返回 `id` |
| `POST /api/devices/{id}/connect` | 开始部署并连接，异步返回；查询列表查看状态 |
| `POST /api/devices/{id}/disconnect` | 取消连接或断开 SSH |
| `DELETE /api/devices/{id}` | 删除已断开的记录 |
| `PUT /api/devices/listener` | `{"address":"0.0.0.0:7700"}` 热启用；空地址关闭，设置持久化 |
| `GET /api/devices/package?platform=windows&gateway=192.168.1.2:7700` | 下载带网关信息的 Windows ZIP；`platform=linux` 为 tar.gz |

保存连接的字段：`id`（新建省略）、`name`、`host`、`port`（0 沿用配置）、`user`、`identity_file`、`password`、`auto_connect`。修改时密码留空保持已有密码；`clear_password: true` 清除密码。`state` 可能为 `offline`、`connecting`、`reconnecting`、`online`、`disconnecting`、`error`，`detail` 是可展示的失败原因。

目前聊天 agent 尚无内置的“管理连接”工具。这些文档和技能为后续接入管理工具提供现成的操作规范；新增工具时仍需按用户的实际授权执行。
