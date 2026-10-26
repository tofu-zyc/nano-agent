# nano-agent Windows 网关安装脚本（离线包）
#
# 解压 make release 生成的 nano-gateway-<版本>-windows-x86_64.zip，在 PowerShell 里运行：
#   powershell -ExecutionPolicy Bypass -File .\install-gateway.ps1
#
# 再运行一次就是升级，保留 gateway.toml 和 data/。
param(
    [string]$Dir = "",
    [string]$TaskName = "nano-agent-gateway",
    [switch]$NoTask,
    [switch]$NoStart,
    [switch]$NoPath,
    [switch]$Yes,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $Dir) {
    if (-not $env:USERPROFILE) { throw "没有 USERPROFILE，没法确定安装目录" }
    $Dir = Join-Path $env:USERPROFILE ".nano-agent"
}
$Dir = [IO.Path]::GetFullPath($Dir)
$Bin = Join-Path $Dir "bin"
$Lib = Join-Path $Dir "lib"
$Exe = Join-Path $Bin "nano-gateway.exe"
$Conf = Join-Path $Dir "gateway.toml"
$Runner = Join-Path $Dir "run-gateway.ps1"
$Schtasks = Join-Path $env:SystemRoot "System32\schtasks.exe"
$StartupDir = $env:NANO_STARTUP_DIR
if (-not $StartupDir) { $StartupDir = [Environment]::GetFolderPath("Startup") }
$StartupLink = Join-Path $StartupDir "$TaskName.lnk"
$env:NANO_HOME = $Dir

function Say($s) { Write-Host $s }
function Die($s) { Write-Host "错误：$s" -ForegroundColor Red; exit 1 }

function Stop-Gateway {
    if (Test-Path $Exe) {
        try { & $Exe stop --quiet 2>$null | Out-Null } catch { }
    }
    Get-CimInstance Win32_Process -Filter "Name = 'nano-gateway.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.ExecutablePath -and ($_.ExecutablePath -ieq $Exe) } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 400
}

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
    if (-not (Test-Path $Dir)) { Die "$Dir 下面没有安装网关" }
    if (-not $Yes) {
        $a = Read-Host "停止网关并删除 $Dir（包括配置、会话和密钥）？[y/N]"
        if ($a -notmatch '^(y|Y|yes|是)$') { Say "已取消"; exit 0 }
    }
    Stop-Gateway
    Remove-Task
    if (-not $NoPath) { Remove-UserPath $Bin }
    Remove-Item -Recurse -Force $Dir
    Say "已卸载。"
    exit 0
}

$PkgExe = Join-Path $Here "nano-gateway.exe"
$PkgKernel = Join-Path $Here "lib\nano-kernel.exe"
$PkgExec = Join-Path $Here "lib\nano-exec.exe"
if (-not (Test-Path $PkgExe) -or -not (Test-Path $PkgKernel) -or -not (Test-Path $PkgExec)) {
    Die "这个脚本要在解压后的安装包里运行（旁边要有 nano-gateway.exe 和 lib\nano-kernel.exe、lib\nano-exec.exe）。"
}

$Upgrade = Test-Path $Conf
if ($Upgrade) { Say "==> 升级（保留 gateway.toml 和 data/）" } else { Say "==> 安装到 $Dir" }
Stop-Gateway
New-Item -ItemType Directory -Force -Path $Bin, $Lib | Out-Null

Copy-Item $PkgExe (Join-Path $Bin "nano-gateway.exe") -Force
Copy-Item $PkgKernel (Join-Path $Lib "nano-kernel.exe") -Force
Copy-Item $PkgExec (Join-Path $Lib "nano-exec.exe") -Force
$PkgRg = Join-Path $Here "lib\rg.exe"
if (Test-Path $PkgRg) { Copy-Item $PkgRg (Join-Path $Lib "rg.exe") -Force }
$PkgVersion = Join-Path $Here "VERSION"
if (Test-Path $PkgVersion) { Copy-Item $PkgVersion (Join-Path $Dir "VERSION") -Force }

# 自带的人格、会话人格和技能：只补不存在的文件，用户改过的不覆盖
foreach ($sub in @("agents", "personas", "skills")) {
    $src = Join-Path $Here $sub
    if (-not (Test-Path $src)) { continue }
    Get-ChildItem -Path $src -Recurse -File | ForEach-Object {
        $rel = $_.FullName.Substring($src.Length).TrimStart('\', '/')
        $dst = Join-Path (Join-Path $Dir $sub) $rel
        if (-not (Test-Path $dst)) {
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item $_.FullName $dst
        }
    }
}

# 默认人格：新安装时 agents/default/persona.md 是唯一来源，personas/default.md 给它用
$Persona = Join-Path $Dir "personas\default.md"
if (-not (Test-Path $Persona)) {
    $SourcePersona = Join-Path $Dir "agents\default\persona.md"
    if (Test-Path $SourcePersona) {
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Persona) | Out-Null
        Copy-Item $SourcePersona $Persona -Force
    }
}

if (-not $Upgrade) {
    $example = Join-Path $Here "gateway.example.toml"
    if (-not (Test-Path $example)) { Die "安装包里没有 gateway.example.toml" }
    $text = [System.IO.File]::ReadAllText($example, [System.Text.Encoding]::UTF8)
    $token = [Guid]::NewGuid().ToString("N")
    $text = $text -replace '(?m)^listen = "unix:/tmp/nano-agent\.sock"', 'listen = "tcp:127.0.0.1:7700"'
    $text = $text -replace '(?m)^token\s*=\s*".*"', ('token  = "' + $token + '"')
    [System.IO.File]::WriteAllText($Conf, $text, (New-Object System.Text.UTF8Encoding($false)))
}

$RunnerText = @"
`$env:NANO_HOME = "$Dir"
`$ErrorActionPreference = 'SilentlyContinue'
& "$Exe" start -c "$Conf" --quiet
"@
[System.IO.File]::WriteAllText($Runner, $RunnerText, (New-Object System.Text.UTF8Encoding($true)))

if (-not $NoTask) {
    $tr = "powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Runner`""
    $taskOK = $false
    try {
        & $Schtasks /Create /TN $TaskName /SC ONLOGON /RL LIMITED /F /TR $tr 2>$null | Out-Null
        $taskOK = ($LASTEXITCODE -eq 0)
    } catch {
        $taskOK = $false
    }
    if (-not $taskOK) {
        try {
            New-Item -ItemType Directory -Force -Path $StartupDir | Out-Null
            $ws = New-Object -ComObject WScript.Shell
            $lnk = $ws.CreateShortcut($StartupLink)
            $lnk.TargetPath = "powershell.exe"
            $lnk.Arguments = "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Runner`""
            $lnk.WindowStyle = 7
            $lnk.Save()
            Say "任务计划建不了（可能要管理员），已放进启动文件夹：$StartupLink"
        } catch {
            Say "没能设置开机自启（不影响现在使用）：$($_.Exception.Message)"
        }
    }
}

if (-not $NoPath) { Add-UserPath $Bin }

if (-not $NoStart) {
    & $Exe start -c $Conf
    if ($LASTEXITCODE -ne 0) { Die "网关没有启动：$LASTEXITCODE" }
    Start-Sleep -Seconds 2
    try { & $Exe url -c $Conf } catch { }
}

Say ""
Say "装好了。"
Say "  程序：$Exe"
Say "  配置：$Conf"
Say "  数据：$(Join-Path $Dir 'data')"
if (-not $NoPath) { Say "  已把 $Bin 加进当前用户 PATH（新开终端生效）" }
Say "  启动/停止：nano-gateway start | stop | status | url"
Say "  网页登录链接：nano-gateway url"
