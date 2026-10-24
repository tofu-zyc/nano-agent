# nano-agent 内核 Windows 安装脚本（离线包）
#
# 解压 make release 生成的 nano-kernel-<版本>-windows-x86_64.zip，在 PowerShell 里运行：
#   powershell -ExecutionPolicy Bypass -File .\install-kernel.ps1
#
# 再运行一次就是升级，保留 kernel.conf 和 paired.token。
# 参数：
#   -Gateway tcp:127.0.0.1:7700   网关地址（不写会问）
#   -Name my-pc                    这台电脑的名字（默认计算机名）
#   -Dir C:\Users\me\.nano-agent\kernel
#   -NoService                     只安装，不注册开机自启，也不启动
#   -NoTask                        不用任务计划程序，在「启动」文件夹里放快捷方式（任务计划建不了时自动退到这个）
#   -Yes                           不提问（必须给 -Gateway）
#   -NoPath                        不把安装目录加进当前用户的 PATH（测试用）
#   -Uninstall                     停止并删除
param(
    [string]$Gateway = "",
    [string]$Name = "",
    [string]$Dir = "",
    [string]$TaskName = "nano-agent-kernel",
    [switch]$NoService,
    [switch]$NoTask,
    [switch]$NoPath,
    [switch]$Yes,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $Dir) {
    if (-not $env:USERPROFILE) { throw "没有 USERPROFILE，没法确定安装目录" }
    $Dir = Join-Path $env:USERPROFILE ".nano-agent\kernel"
}
$Kernel = Join-Path $Dir "nano-kernel.exe"
$Exec = Join-Path $Dir "nano-exec.exe"
$Conf = Join-Path $Dir "kernel.conf"
$Log = Join-Path $Dir "kernel.log"
$Runner = Join-Path $Dir "run-kernel.ps1"
$Schtasks = Join-Path $env:SystemRoot "System32\schtasks.exe"
# 「启动」文件夹里的快捷方式（NANO_STARTUP_DIR 给测试用）
$StartupDir = $env:NANO_STARTUP_DIR
if (-not $StartupDir) { $StartupDir = [Environment]::GetFolderPath("Startup") }
$StartupLink = Join-Path $StartupDir "$TaskName.lnk"

function Say($s) { Write-Host $s }
function Die($s) { Write-Host "错误：$s" -ForegroundColor Red; exit 1 }

function Stop-Kernel {
    try { & $Schtasks /End /TN $TaskName 2>$null | Out-Null } catch { }
    # 手动运行的 run-kernel.ps1 会在内核退出后把它重新拉起，先停掉它
    Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -and $_.CommandLine.Contains($Runner) } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Get-Process nano-kernel -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $Kernel } |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 300
}

# 内核目录加进当前用户的 PATH（新开的终端里能直接用 nano-kernel status 之类）
function Add-UserPath($d) {
    $p = [Environment]::GetEnvironmentVariable("Path", "User")
    $parts = @()
    if ($p) { $parts = $p -split ';' | Where-Object { $_ } }
    if ($parts -notcontains $d) { [Environment]::SetEnvironmentVariable("Path", (($parts + $d) -join ';'), "User") }
}
function Remove-UserPath($d) {
    $p = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not $p) { return }
    $parts = $p -split ';' | Where-Object { $_ -and $_ -ne $d }
    [Environment]::SetEnvironmentVariable("Path", ($parts -join ';'), "User")
}

function Remove-Task {
    try { & $Schtasks /Delete /TN $TaskName /F 2>$null | Out-Null } catch { }
    Remove-Item -Force $StartupLink -ErrorAction SilentlyContinue
}

if ($Uninstall) {
    if (-not (Test-Path $Dir)) { Die "$Dir 下面没有安装内核" }
    if (-not $Yes) {
        $a = Read-Host "停止内核并删除 $Dir（包括配置和配对口令）？[y/N]"
        if ($a -notmatch '^(y|Y|yes|是)$') { Say "已取消"; exit 0 }
    }
    Stop-Kernel
    Remove-Task
    if (-not $NoPath) { Remove-UserPath $Dir }
    Remove-Item -Recurse -Force $Dir
    Say "已卸载。网关网页拓扑图上配对过的这台电脑可以一并移除。"
    exit 0
}

$PkgKernel = Join-Path $Here "nano-kernel.exe"
$PkgExec = Join-Path $Here "nano-exec.exe"
if (-not (Test-Path $PkgKernel) -or -not (Test-Path $PkgExec)) {
    Die "这个脚本要在解压后的安装包里运行（旁边要有 nano-kernel.exe 和 nano-exec.exe）。现在只有源码安装，暂不支持联网下载。"
}

$Upgrade = Test-Path $Conf
$Utf8 = New-Object System.Text.UTF8Encoding($false)
$ConfKeys = 'gateway|token|node|exec|skills|max_steps|run_timeout|approval_timeout|max_output'
$ConfHead = "# nano-agent 内核配置（安装脚本生成，可以改，再运行安装脚本会保留）。这里只管连哪个网关；每条消息的上限等设置在网关网页的拓扑图里改"

# 以前的安装脚本按系统编码（中文系统是 GBK）读 kernel.conf：第一行的中文说明读成了乱码，吃掉了换行，
# 后面的 gateway = … 跟着进了注释；最后一行又没有换行，nano-kernel connect 追加时两行粘在一起。
# 这里按 UTF-8 读，把这两种坏掉的地方修好，其他行原样保留。
function Repair-ConfLines([string]$text) {
    $lines = New-Object System.Collections.Generic.List[string]
    foreach ($line in ($text -split "\r?\n")) {
        if ($line -match '^\s*#\s*nano-agent') {
            # 安装脚本写的说明行：去掉（写回时换成新的），粘在后面的配置项救回来
            if ($line -match "(?<![A-Za-z_])((?:$ConfKeys)\s*=.*)$") { $line = $Matches[1] } else { continue }
        }
        if ($line -match '^\s*#') { $lines.Add($line); continue }
        $parts = @()
        while ($line -match "^(\s*(?:$ConfKeys)\s*=.*?\S)((?:$ConfKeys)\s*=.*)$") {
            $parts += $Matches[1]
            $line = $Matches[2]
        }
        foreach ($p in ($parts + $line)) {
            # 以前安装脚本写的默认上限：现在由网关每一轮带过来，这几行没用了
            if ($p -match '^\s*(max_steps\s*=\s*16|run_timeout\s*=\s*600|approval_timeout\s*=\s*300|max_output\s*=\s*65536)\s*$') { continue }
            $lines.Add($p)
        }
    }
    while ($lines.Count -and -not $lines[$lines.Count - 1].Trim()) { $lines.RemoveAt($lines.Count - 1) }
    return ,$lines
}
$ConfLines = New-Object System.Collections.Generic.List[string]
if ($Upgrade) { $ConfLines = Repair-ConfLines ([System.IO.File]::ReadAllText($Conf, [System.Text.Encoding]::UTF8)) }
function Conf-Get($key) {
    $line = $ConfLines | Where-Object { $_ -match "^\s*$key\s*=" } | Select-Object -Last 1
    if ($line) { return ($line -replace "^\s*$key\s*=\s*", "").Trim() }
    return ""
}
function Conf-Set($key, $value) {
    $hit = $false
    for ($i = 0; $i -lt $ConfLines.Count; $i++) {
        if ($ConfLines[$i] -match "^\s*$key\s*=") {
            if ($hit) { $ConfLines.RemoveAt($i); $i--; continue }
            $ConfLines[$i] = "$key = $value"
            $hit = $true
        }
    }
    if (-not $hit) { $ConfLines.Add("$key = $value") }
}

if (-not $Gateway -and $Upgrade) { $Gateway = Conf-Get "gateway" }
if (-not $Gateway) {
    if ($Yes) { Die "没有网关地址：加上 -Gateway tcp:主机:7700" }
    $Gateway = Read-Host "网关地址（例如 tcp:192.168.1.2:7700）"
    if (-not $Gateway) { $Gateway = "tcp:127.0.0.1:7700" }
}
if ($Gateway -notmatch '^(tcp|unix):') {
    if ($Gateway -match ':') { $Gateway = "tcp:$Gateway" }
    else { $Gateway = "tcp:${Gateway}:7700" }
}
if (-not $Name) {
    if ($Upgrade) { $Name = Conf-Get "node" }
    if (-not $Name) {
        $Name = if ($env:COMPUTERNAME) { $env:COMPUTERNAME } else { "my-pc" }
        if (-not $Yes) {
            $a = Read-Host "这台电脑的名字（在网关里显示，默认 $Name）"
            if ($a) { $Name = $a }
        }
    }
}
if (-not $Name -or $Name -match '[\s:/]' -or $Name -in @("本机", "local", "LOCAL")) {
    Die "名字不能为空，不能有空格、冒号和斜杠，也不能叫“本机”：$Name"
}

Say ""
Say "==> 安装到 $Dir"
Stop-Kernel
New-Item -ItemType Directory -Force -Path $Dir | Out-Null

Copy-Item $PkgKernel (Join-Path $Dir "nano-kernel.new") -Force
Copy-Item $PkgExec (Join-Path $Dir "nano-exec.new") -Force
Move-Item (Join-Path $Dir "nano-kernel.new") $Kernel -Force
Move-Item (Join-Path $Dir "nano-exec.new") $Exec -Force
$PkgRg = Join-Path $Here "rg.exe"
if (Test-Path $PkgRg) { Copy-Item $PkgRg (Join-Path $Dir "rg.exe") -Force }
$Version = if (Test-Path (Join-Path $Here "VERSION")) { (Get-Content (Join-Path $Here "VERSION") -Raw).Trim() } else { "dev" }
Set-Content -Path (Join-Path $Dir "VERSION") -Value $Version -Encoding ASCII

if ($Upgrade) { Say "==> 升级（保留配置和配对口令）" }
Conf-Set "gateway" $Gateway
Conf-Set "node" $Name
if (-not (Conf-Get "exec")) { Conf-Set "exec" $Exec }
# 按 UTF-8（不带 BOM）写，每行都以换行结尾
$text = (@($ConfHead) + $ConfLines) -join "`r`n"
[System.IO.File]::WriteAllText($Conf, $text + "`r`n", $Utf8)

# 没有口令：第一次连网关时请求配对，在网关网页上核对确认码。
# 配对口令由内核写在 kernel.conf 旁边的 paired.token。

# 退出码 75：内核换上了新版本（或退回了旧版本），马上重新拉起；0：正常退出（被网关拒绝等）；其他：崩溃，等一会儿再拉起
# 日志超过 5 MB 就换一个文件（内核自己用 --log 追加写，换的时候没有进程拿着它）。
# 用 .NET 直接启动内核：Start-Process 在没有控制台时拿不到可等待的句柄，WaitForExit 会立刻返回。
# 脚本里不放中文注释，并且带 BOM 写出：PowerShell 5.1 按系统代码页读没有 BOM 的文件，路径里的中文才不会坏。
$RunnerText = @"
`$ErrorActionPreference = 'Continue'
`$log = "$Log"
while (`$true) {
    try { if ((Test-Path `$log) -and (Get-Item `$log).Length -gt 5MB) { Move-Item -Force `$log "`$log.1" } } catch { }
    `$psi = New-Object System.Diagnostics.ProcessStartInfo
    `$psi.FileName = "$Kernel"
    `$psi.Arguments = '-c "$Conf" --log "' + `$log + '"'
    `$psi.UseShellExecute = `$false
    `$psi.CreateNoWindow = `$true
    try { `$p = [System.Diagnostics.Process]::Start(`$psi) } catch { Start-Sleep -Seconds 5; continue }
    `$p.WaitForExit()
    `$code = `$p.ExitCode
    `$p.Dispose()
    if (`$code -eq 75) { continue }
    if (`$code -eq 0) { break }
    Start-Sleep -Seconds 5
}
"@
[System.IO.File]::WriteAllText($Runner, $RunnerText, (New-Object System.Text.UTF8Encoding($true)))

# 留一份安装脚本在安装目录里：以后卸载用 powershell -File "$Dir\install-kernel.ps1" -Uninstall
$Self = $MyInvocation.MyCommand.Path
$Kept = Join-Path $Dir "install-kernel.ps1"
if ($Self -and ((Resolve-Path $Self).Path -ne $Kept)) { Copy-Item -Force $Self $Kept }

$checkLog = Join-Path $Dir "check.log"
$check = Start-Process -FilePath $Kernel -ArgumentList @('-c', $Conf, '-t') -NoNewWindow -Wait -PassThru -RedirectStandardError $checkLog
if ($check.ExitCode -ne 0) {
    Get-Content $checkLog -ErrorAction SilentlyContinue
    Die "配置检查没通过：$Conf"
}
Remove-Item $checkLog -ErrorAction SilentlyContinue

if (-not $NoPath) { Add-UserPath $Dir }
if ($NoService) {
    [System.IO.File]::WriteAllText((Join-Path $Dir "service"), "none`n")
    Say "装好了，没有注册开机自启，也没有启动。启动：nano-kernel start（新开的终端里能用；nano-kernel help 看全部命令）"
    exit 0
}
Say ""
Say "==> 设置登录时自动启动"
$arg = "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Runner`""
$service = ""
if (-not $NoTask) {
    try {
        # 触发器和运行身份都限定为当前用户：普通用户（不是管理员）建不了「任何用户登录时」触发的任务
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument $arg -WorkingDirectory $Dir
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $me
        $principal = New-ScheduledTaskPrincipal -UserId $me -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew
        Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force -ErrorAction Stop | Out-Null
        $service = "task:$TaskName"
        Remove-Item -Force $StartupLink -ErrorAction SilentlyContinue
        Say "任务计划程序：$TaskName（登录时启动）"
    } catch {
        Say "任务计划程序建不了任务（$($_.Exception.Message.Trim())），改用「启动」文件夹"
    }
}
if (-not $service) {
    # 退路：「启动」文件夹里放快捷方式，登录时运行 run-kernel.ps1（不需要任何权限）
    try { & $Schtasks /Delete /TN $TaskName /F 2>$null | Out-Null } catch { }
    try {
        New-Item -ItemType Directory -Force -Path $StartupDir | Out-Null
        $shell = New-Object -ComObject WScript.Shell
        $lnk = $shell.CreateShortcut($StartupLink)
        $lnk.TargetPath = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
        $lnk.Arguments = $arg
        $lnk.WorkingDirectory = $Dir
        $lnk.WindowStyle = 7
        $lnk.Description = "nano-agent kernel"
        $lnk.Save()
        $service = "startup:$TaskName"
        Say "「启动」文件夹：$StartupLink"
    } catch {
        Say "「启动」文件夹也放不进快捷方式（$($_.Exception.Message.Trim())），这次先直接启动，登录后要手动 nano-kernel start"
        $service = "none"
    }
}
[System.IO.File]::WriteAllText((Join-Path $Dir "service"), "$service`n")

# 启动：任务计划就运行任务，其他情况直接在后台拉起 run-kernel.ps1；nano-kernel start 等到内核进程起来才返回
if (-not (Test-Path $Log)) { New-Item -ItemType File -Path $Log | Out-Null }
$start = (Get-Item $Log).Length
& $Kernel start --quiet
if ($LASTEXITCODE -ne 0) {
    Get-Content $Log -Tail 20 -ErrorAction SilentlyContinue
    Die "内核没有启动起来（上面是日志）"
}

function New-Log {
    if (-not (Test-Path $Log)) { return "" }
    $fs = [System.IO.File]::Open($Log, 'Open', 'Read', 'ReadWrite')
    try {
        [void]$fs.Seek($start, 'Begin')
        $sr = New-Object System.IO.StreamReader($fs)
        return $sr.ReadToEnd()
    } finally { $fs.Dispose() }
}

Say ""
Say "==> 连接网关 $Gateway"
$code = ""
$deadline = (Get-Date).AddSeconds(20)
$codeDeadline = $null
while ($true) {
    $out = New-Log
    if ($out -match "网关已接受连接") {
        Say "已接入网关。打开网关网页，在会话里选这台电脑上的工作目录就能开始工作。"
        Say "管理内核（新开的终端里）：nano-kernel status | stop | start | connect 网关地址 | logs -f"
        exit 0
    }
    if ($out -match "没有同意这台电脑接入") {
        Die "网关拒绝了这台电脑。要重新申请：nano-kernel restart（新开的终端里）"
    }
    if (-not $code -and $out -match "核对确认码\s*([0-9]{6})") {
        $code = $Matches[1]
        Say ""
        Say "  确认码：$($code.Substring(0,3)) $($code.Substring(3))"
        Say "  打开网关网页（会弹出接入请求），核对确认码一致后点「允许接入」。"
        Say "  （可以关掉这个窗口，内核会在后台一直等着。）"
        $codeDeadline = (Get-Date).AddMinutes(5)
    }
    if (-not $code -and (Get-Date) -gt $deadline) { break }
    if ($code -and (Get-Date) -gt $codeDeadline) { break }
    Start-Sleep -Seconds 1
}

if ($code) {
    Say "还没等到网关确认。内核在后台继续等，点了允许就会接上。"
} else {
    $alive = Get-Process nano-kernel -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Kernel }
    if (-not $alive) {
        Get-Content $Log -Tail 20 -ErrorAction SilentlyContinue
        Die "内核启动后又退出了（上面是日志）"
    }
    Say "暂时没连上网关，内核会在后台一直重试。现在的状态："
    & $Kernel status
    Say "检查：网关地址对不对（$Gateway）、网关网页拓扑图「接入机器」里电脑接入有没有启用、防火墙是否放行端口。"
    Say "看状态：nano-kernel status；看日志：nano-kernel logs -f（新开的终端里）"
}
exit 0
