# 计划：查看和控制网关

## 现状和问题

- 终端：有 systemd 时只注册了服务，没有统一的命令，只能敲 `systemctl --user ...`；没有 systemd 时才有 `nano-gateway-ctl`。两种情况用法不一样。
- 网页：设置页提示“有 N 项改动要重启网关才生效”，但没有重启按钮。
- 聊天：没有 `/status`、`/restart`。AI 的 Gateway 工具能查状态，GatewayAdmin 不能重启。
- 网关在 WSL 里时，Windows 可能在关掉所有终端后停掉 WSL，网关跟着停，聊天里就叫不动了。

目标：不管网关装成 systemd 服务还是后台进程，终端、网页、聊天里都能看状态、重启；用户在手机上也能完成。

## 1. 统一的命令：`nano-gateway <子命令>`

都做在 Go 程序里（不再靠 shell 脚本），两种运行方式（systemd 用户服务 / 后台进程 + pid 文件）由程序自己判断。

```
nano-gateway status      运行状态：是否在运行、pid、版本、运行了多久、运行方式（systemd / 后台进程）；
                         网页地址、内核监听地址；在线的内核和机器、各聊天渠道是否连上、默认模型；
                         有没有“改了还没重启”的设置；最近的错误（几条，已脱敏）
nano-gateway start | stop | restart
nano-gateway logs [-f] [-n 100]
nano-gateway url         打印带密码的网页登录链接（就是 data/web.url）
nano-gateway doctor      检查常见问题，见第 5 节
nano-gateway version
nano-gateway chat        （已有）
nano-gateway             不带子命令：照旧在前台运行网关（systemd 用的就是这个）
```

- status 和网关通信：走 `run/` 里已有的本机 socket（只有本用户能连），加一个查询请求；网关没运行时只报服务状态和最后几行日志。
- start/stop/restart：有 systemd 单元就调 `systemctl --user`，否则用 pid 文件 + 后台进程（把现在 `nano-gateway-ctl` 的逻辑搬进来，要确认 pid 对应的是自己的程序）。
- `nano-gateway-ctl` 删掉；安装和迁移时把它从旧安装里清掉，`.profile` 里的自启行改成 `nano-gateway start --quiet`。
- 版本号编译时写进去（`make` 用 `git describe`，`make release` 用 VERSION）。
- 安装脚本最后的提示改成这套命令。

## 2. 网关自己重启

网页、聊天、AI 触发的重启都由网关自己完成：

1. **先检查配置**：按重启后的方式重新加载 gateway.toml 和 data/config.json，加载不了就拒绝重启，把错误原样告诉发起的人。远程操作时重启失败等于失联，这一步不能省。
2. 记下是谁、从哪个会话发起的（存到 data/ 里），然后正常收尾：停本机内核、断开渠道、关监听。
3. 用同样的参数重新执行自己（re-exec）。systemd 下 pid 不变，没问题；后台进程模式下 pid 文件也不用改。实现时在两种模式下都实测。
4. 启动后，给发起重启的会话发一条“网关已重启（用时 N 秒）”。

`nano-gateway restart` 在终端里照旧走 systemctl 或 pid 文件，不需要走这条路径，但同样先检查配置。

## 3. 网页和聊天

- 网页：设置页的“要重启才生效”提示旁边加“重启网关”按钮；页面上能看到版本和运行时长（放在已有的状态或拓扑位置，不要另起一页）。重启期间页面显示“重启中”，自动重连。
- 聊天命令（只给管理员）：
  - `/status`：第 1 节 status 的简短版。
  - `/restart`：管理员自己敲的命令就是本人操作，不再额外审批；先检查配置，不通过就报错。
  - `/help` 里加上这两条。
- AI：
  - Gateway（查询，不审批）：status 返回同样的内容；加 `logs`（最近 N 行，脱敏）。
  - GatewayAdmin 加 `restart`，和其他 GatewayAdmin 动作一样要审批。
  - 不加修改监听地址这类网关设置的动作（用户的原则：这些 AI 不用管）。

## 4. 进程掉了能自己回来

- systemd：已有 `Restart=always`。
- 后台进程模式：现在挂了就没了。`nano-gateway start` 起一个很小的看护进程：网关异常退出时隔几秒重新拉起，`stop` 时一起停。重启（第 2 节的 re-exec）不算异常退出。

## 5. doctor

逐项检查，每项给“正常 / 有问题 + 怎么修”：

- 配置能加载；secret.key 存在且权限 0600；data/ 权限。
- 网页端口、内核端口没被别的程序占用；内核监听地址是 unix 时提示“别的电脑和 Windows 内核连不上，要改成 tcp”。
- 本机内核和执行器的程序在，版本和网关一致。
- 服务方式：systemd 单元是否启用；没有 linger 时提示只在登录后运行。
- **WSL**：检测到在 WSL 里时，说明“关掉所有终端后 Windows 可能停掉 WSL，网关跟着停”，并给出保持运行的办法。要真机实测哪种办法管用（比如在 Windows 上注册一个登录时运行的计划任务，让这个发行版一直有个进程），确认能用后让安装脚本在 WSL 里询问是否设置。不要改用户的 .wslconfig，除非用户同意。

## 测试

- Go 单元测试：子命令解析、pid 文件判断、配置检查失败时拒绝重启。
- e2e：在临时 NANO_HOME 里，后台进程模式下 start / status / restart / stop；聊天里 `/status`、`/restart`，重启后收到“已重启”；配置写坏后 `/restart` 被拒绝，网关还在运行；看护进程在 kill -9 后把网关拉回来。
- systemd 模式不要动用户真实的 `nano-gateway.service`：用临时的单元名测，或者只测到生成的命令为止，并在 HANDOFF 里写明。
- WSL 保持运行的办法要在真实 Windows 上试，但不要留下计划任务（测完删掉）。

## 约定

- 不建仓库、不推送、不发布。
- 不启动用户真实的配置（会连上飞书和真实模型），用临时 NANO_HOME 和 mock 模型。
- 不碰用户自己的 ~/.nano-agent、仓库里的 gateway.toml 和 data/。
- 完成后 `make test` 全过，单独提交，更新 HANDOFF.md 和 README 里的用法。
