# nano-agent Windows 一行安装（内核）：从 GitHub Release 下载 Windows 包，校验 SHA256，再运行包里的 install-kernel.ps1。
#
#   & ([scriptblock]::Create((irm https://github.com/tofu-zyc/nano-agent/releases/latest/download/install.ps1))) -Gateway tcp:192.168.1.2:7700
#
# 再运行一次就是升级，保留 kernel.conf 和 paired.token。参数：
#   -Gateway 地址     网关地址（不写会问）
#   -Name 名字        这台电脑的名字（默认计算机名）
#   -Dir 目录         安装目录（默认 %USERPROFILE%\.nano-agent\kernel）
#   -Version x.y.z    装指定版本（默认最新）
#   -NoService        只安装，不注册开机自启，也不启动
#   -Yes              不提问（必须给 -Gateway）
#   -Uninstall        停止并删除
# 环境变量 NANO_DOWNLOAD_URL 可以换下载地址（镜像）。
param(
    [string]$Gateway = "",
    [string]$Name = "",
    [string]$Dir = "",
    [string]$Version = "",
    [string]$TaskName = "",
    [switch]$NoService,
    [switch]$Yes,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest 的进度条会让下载慢很多
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

$Base = "https://github.com/tofu-zyc/nano-agent/releases"
if ($env:NANO_DOWNLOAD_URL) { $Base = $env:NANO_DOWNLOAD_URL.TrimEnd('/') }
$Tmp = Join-Path ([IO.Path]::GetTempPath()) ("nano-agent-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Tmp | Out-Null

# 用 throw 而不是 exit：这段脚本常在用户自己的 PowerShell 窗口里运行，exit 会把窗口关掉。
# 字符串只用 ASCII：irm 拿到的是没有编码信息的下载内容，中文会被解成乱码（注释乱了不要紧）
function Get-File($url, $out) {
    try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $out }
    catch { throw "download failed: $url ($($_.Exception.Message)). Set NANO_DOWNLOAD_URL to use a mirror." }
}

try {
    if (-not $Version) {
        Get-File "$Base/latest/download/VERSION" (Join-Path $Tmp "VERSION")
        $Version = (Get-Content -Raw (Join-Path $Tmp "VERSION")).Trim()
    }
    $Version = $Version.TrimStart('v')
    if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "bad version: $Version" }

    $Pkg = "nano-kernel-$Version-windows-x86_64"
    $Url = "$Base/download/v$Version"
    Write-Host "Downloading $Pkg.zip ($Url) ..."
    $Zip = Join-Path $Tmp "$Pkg.zip"
    $Sums = Join-Path $Tmp "SHA256SUMS"
    Get-File "$Url/$Pkg.zip" $Zip
    Get-File "$Url/SHA256SUMS" $Sums
    $want = ""
    foreach ($line in Get-Content $Sums) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq "$Pkg.zip") { $want = $parts[0].ToLower() }
    }
    if (-not $want) { throw "$Pkg.zip is not listed in SHA256SUMS" }
    # 不用 Get-FileHash / Expand-Archive：它们在模块里，有的环境（比如 PSModulePath 不对）加载不了
    $sha = [Security.Cryptography.SHA256]::Create()
    $fs = [IO.File]::OpenRead($Zip)
    try { $got = ([BitConverter]::ToString($sha.ComputeHash($fs)) -replace '-', '').ToLower() } finally { $fs.Close() }
    if ($got -ne $want) { throw "SHA256 mismatch for $Pkg.zip (broken download or tampered file)" }
    Write-Host "SHA256 OK"
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [IO.Compression.ZipFile]::ExtractToDirectory($Zip, $Tmp)
    $Script = Join-Path $Tmp "$Pkg\install-kernel.ps1"
    if (-not (Test-Path $Script)) { throw "install-kernel.ps1 not found in the package" }

    $a = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $Script)
    if ($Gateway) { $a += @("-Gateway", $Gateway) }
    if ($Name) { $a += @("-Name", $Name) }
    if ($Dir) { $a += @("-Dir", $Dir) }
    if ($TaskName) { $a += @("-TaskName", $TaskName) }
    if ($NoService) { $a += "-NoService" }
    if ($Yes) { $a += "-Yes" }
    if ($Uninstall) { $a += "-Uninstall" }
    & powershell.exe @a
    if ($LASTEXITCODE -ne 0) { throw "install-kernel.ps1 failed (exit code $LASTEXITCODE)" }
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
