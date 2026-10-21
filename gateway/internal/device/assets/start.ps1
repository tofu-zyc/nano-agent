param([string]$Gateway, [string]$Token, [string]$Node, [string]$Distro, [string]$InstallDir, [switch]$NoUI)
$ErrorActionPreference = 'Stop'
if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    throw 'Windows 启动器需要 WSL2。请先在管理员终端运行 wsl --install，安装后重新打开启动器。'
}
$distros = @((& wsl.exe --list --quiet) | ForEach-Object { ($_ -replace "`0", '').Trim() } | Where-Object { $_ })
if ($distros.Count -eq 0) { throw '尚未安装 WSL 发行版，请先运行 wsl --install。' }
if (-not $Distro) { $Distro = $distros[0] }
$configPath = Join-Path $PSScriptRoot 'kernel.conf'
$existing = @{}
foreach ($line in Get-Content -LiteralPath $configPath -Encoding UTF8) {
    if ($line -match '^([a-z_]+)\s*=\s*(.*)$') { $existing[$Matches[1]] = $Matches[2] }
}
if (-not $Gateway) { $Gateway = $existing['gateway'] }
if (-not $Token) { $Token = $existing['token'] }
if (-not $Node) { $Node = $existing['node'] }
if (-not $Node) { $Node = $env:COMPUTERNAME }
if (-not $NoUI) {
    Add-Type -AssemblyName System.Windows.Forms
    Add-Type -AssemblyName System.Drawing
    $form = New-Object Windows.Forms.Form
    $form.Text = 'nano-agent · 连接这台电脑'
    $form.Size = New-Object Drawing.Size(520, 330)
    $form.StartPosition = 'CenterScreen'
    $form.FormBorderStyle = 'FixedDialog'
    $form.MaximizeBox = $false
    $form.Font = New-Object Drawing.Font('Microsoft YaHei UI', 10)
    $boxes = @{}
    $row = 0
    foreach ($item in @(@('Gateway','网关地址',$Gateway), @('Node','电脑名字',$Node))) {
        $label = New-Object Windows.Forms.Label
        $label.Text = $item[1]; $label.Location = New-Object Drawing.Point(22, (28 + $row * 48)); $label.AutoSize = $true
        $box = New-Object Windows.Forms.TextBox
        $box.Text = $item[2]; $box.Location = New-Object Drawing.Point(120, (24 + $row * 48)); $box.Width = 350
        $form.Controls.Add($label); $form.Controls.Add($box); $boxes[$item[0]] = $box; $row++
    }
    $label = New-Object Windows.Forms.Label
    $label.Text = 'WSL 环境'; $label.Location = New-Object Drawing.Point(22, 124); $label.AutoSize = $true
    $combo = New-Object Windows.Forms.ComboBox
    $combo.Location = New-Object Drawing.Point(120, 120); $combo.Width = 350; $combo.DropDownStyle = 'DropDownList'
    $combo.Items.AddRange([object[]]$distros); $combo.SelectedItem = $Distro
    $hint = New-Object Windows.Forms.Label
    $hint.Text = '第一次连接时，在网关网页的「设备」页面核对确认码并点「允许」。工具在 WSL2 里执行，Windows 的目录在网页里选 /mnt/c、/mnt/d 下的路径。'
    $hint.Location = New-Object Drawing.Point(22, 166); $hint.Size = New-Object Drawing.Size(450, 62)
    $connect = New-Object Windows.Forms.Button
    $connect.Text = '连接网关'; $connect.Location = New-Object Drawing.Point(350, 240); $connect.Size = New-Object Drawing.Size(120, 32)
    $connect.DialogResult = 'OK'; $form.AcceptButton = $connect
    $form.Controls.AddRange(@($label, $combo, $hint, $connect))
    if ($form.ShowDialog() -ne 'OK') { exit 0 }
    $Gateway = $boxes['Gateway'].Text.Trim(); $Node = $boxes['Node'].Text.Trim(); $Distro = $combo.SelectedItem
}
if (-not $Gateway -or -not $Node) { throw '请填写网关地址和电脑名字。' }
if ($Node -match '[\s:/\\]' -or "$Gateway$Token$Node" -match "[`r`n]") { throw '名字不能含空格、冒号或斜杠，输入不能含换行。' }
# 一般不写口令：第一次连接时向网关请求配对，口令由网关发给这台电脑（存在安装目录的 paired.token）
$conf = "gateway = $Gateway`nnode = $Node`nexec = bin/nano-exec`n"
if ($Token) { $conf += "token = $Token`n" }
[IO.File]::WriteAllText($configPath, $conf, (New-Object Text.UTF8Encoding($false)))
if ($PSScriptRoot -match '^\\\\wsl(?:\.localhost|\$)\\([^\\]+)\\(.+)$') {
    if ($Matches[1] -ne $Distro) { throw '安装包在另一个 WSL 环境中，请先解压到 Windows 文件夹。' }
    $linuxRoot = '/' + $Matches[2].Replace('\','/')
} else {
    $linuxRoot = ((& wsl.exe --distribution $Distro --exec wslpath -a $PSScriptRoot) -join '').Trim()
    if ($LASTEXITCODE -ne 0) { throw '无法转换安装包路径，请解压到本机磁盘后再启动。' }
}
$path64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($linuxRoot))
$target64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($InstallDir))
$script = @'
set -eu
umask 077
source=$(echo PATH64 | base64 -d)
target=$(echo TARGET64 | base64 -d)
if [ -z "$target" ]; then target="$HOME/.local/share/nano-agent/client"; fi
mkdir -p "$target"
cp -R "$source/." "$target/"
chmod 600 "$target/kernel.conf"
cd "$target"
exec sh "$target/start.sh" --saved
'@
$script = $script.Replace('PATH64', $path64)
$script = $script.Replace('TARGET64', $target64)
$payload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($script))
Write-Host '正在 WSL 中启动；关闭此窗口或按 Ctrl-C 可停止。'
& wsl.exe --distribution $Distro --exec sh -c "echo $payload | base64 -d | sh"
exit $LASTEXITCODE
