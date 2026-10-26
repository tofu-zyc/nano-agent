#!/usr/bin/env python3
"""从 WSL 验证 Linux SSH 网关 → 独立 Windows 内核 → Driver → 自有桌面夹具。

需要 Windows 图形会话、已有的 Cua Driver、Zig、已编译的 Windows 内核/执行器、Pillow。
所有配置和程序副本在临时目录，不连接或终止正式内核。默认使用确定性模型替身；
显式提供 --real-model-home 时使用目标 Linux 账号配置的真实模型，会消费 API 用量。
示例：python3 scripts/e2e-desktop-win.py --ssh linux-pc --gateway /tmp/test/nano-gateway
  --driver 'C:\\Users\\me\\AppData\\Local\\Programs\\Cua\\cua-driver\\bin\\cua-driver.exe'
"""
import argparse
import json
from pathlib import Path
import shlex
import shutil
import signal
import socket
import subprocess
import tempfile
import time

from PIL import Image

SOURCE = Path(__file__).resolve().parent.parent
POWERSHELL = '/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe'


def winpath(path):
    return subprocess.check_output(['wslpath', '-w', str(path)], text=True).strip()


def psquote(value):
    return "'" + str(value).replace("'", "''") + "'"


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def stop(proc):
    if proc is not None and proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def main():
    def interrupted(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ssh', required=True, help='SSH 测试目标别名')
    parser.add_argument('--gateway', required=True, help='Linux 目标机上待测网关的绝对路径')
    parser.add_argument('--kernel', help='Linux 目标机上的内核，默认 ~/.nano-agent/lib/nano-kernel')
    parser.add_argument('--executor', help='Linux 目标机上的执行器，默认 ~/.nano-agent/lib/nano-exec')
    parser.add_argument('--driver', required=True, help='Windows 已安装的 Driver 绝对路径')
    parser.add_argument('--fixture-root', type=Path, help='复用已启动的自有夹具目录（WSL 路径）')
    parser.add_argument('--output', type=Path, default=SOURCE/'build/verification-windows')
    parser.add_argument('--real-model-home', help='显式选择 Linux 目标机已有的真实模型配置目录（会消费 API 用量）')
    opts = parser.parse_args()
    if not Path(POWERSHELL).is_file():
        parser.error('该用例必须从可调用 Windows 程序的 WSL 环境运行')
    opts.output.mkdir(parents=True, exist_ok=True)
    report_file = opts.output/'report.json'
    report_file.write_text(json.dumps({'ok': False, 'status': 'running'}))

    def remote(args, **kwargs):
        return subprocess.run(['ssh', opts.ssh, shlex.join(args)], check=True, **kwargs)

    def stop_windows(pid, executable):
        command = f"$taskProcess = Get-Process -Id {int(pid)} -ErrorAction SilentlyContinue; if ($taskProcess -and $taskProcess.Path -eq {psquote(executable)}) {{ Stop-Process -Id {int(pid)} -Force }}"
        subprocess.run([POWERSHELL, '-NoProfile', '-Command', command], capture_output=True, timeout=15, check=True)

    if opts.fixture_root:
        root = opts.fixture_root.resolve()
        assert (root/'desktop-fixture.exe').is_file() and (root/'fixture.json').is_file()
    else:
        temporary = subprocess.check_output([POWERSHELL, '-NoProfile', '-Command', '[Console]::Write($env:TEMP)'], text=True)
        temp_path = subprocess.check_output(['wslpath', '-u', temporary], text=True).strip()
        root = Path(tempfile.mkdtemp(prefix='nano-desktop-e2e-', dir=temp_path))
        subprocess.run([str(SOURCE/'build/tools/zig/zig'), 'cc', '-target', 'x86_64-windows-gnu',
                        '-O2', '-Wall', '-Wextra', '-municode', '-Wl,--subsystem,windows',
                        str(SOURCE/'scripts/fixtures/desktop-win.c'), '-luser32', '-lgdi32',
                        '-o', str(root/'desktop-fixture.exe')], check=True)
    root_win = winpath(root)
    kernel_root = Path(tempfile.mkdtemp(prefix='nano-desktop-kernel-', dir=root.parent))
    kernel_directory = winpath(kernel_root)
    kernel_win = kernel_directory + '\\nano-kernel.exe'
    config_win = kernel_directory + '\\kernel.conf'
    log_win = kernel_directory + '\\kernel.log'
    pid_win = kernel_directory + '\\kernel.pid'
    (root/'application-output.txt').unlink(missing_ok=True)
    node = 'desktop-test-' + root.name.rsplit('-', 1)[-1][:12]
    for name in ('nano-kernel.exe', 'nano-exec.exe'):
        shutil.copy2(SOURCE/'kernel/build'/name, kernel_root/name)
    remote_root = remote(['python3', '-c', 'import tempfile; print(tempfile.mkdtemp(prefix="nano-win-desktop-"))'],
                         capture_output=True, text=True).stdout.strip()
    remote_home = remote(['python3', '-c', 'from pathlib import Path; print(Path.home())'], capture_output=True, text=True).stdout.strip()
    opts.kernel = opts.kernel or remote_home + '/.nano-agent/lib/nano-kernel'
    opts.executor = opts.executor or remote_home + '/.nano-agent/lib/nano-exec'
    remote_port = int(remote(['python3', '-c', 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'],
                            capture_output=True, text=True).stdout.strip())
    helper = 'e2e-autonomy-real.py' if opts.real_model_home else 'e2e-autonomy.py'
    subprocess.run(['scp', str(SOURCE/'scripts'/helper), f'{opts.ssh}:{remote_root}/{helper}'], check=True)
    local_port = port()
    token = 'isolated-real-e2e' if opts.real_model_home else 'isolated-e2e'
    (kernel_root/'kernel.conf').write_text(f'gateway = tcp:127.0.0.1:{local_port}\ntoken = {token}\nnode = {node}\nexec = {kernel_directory}\\nano-exec.exe\n', encoding='utf-8')
    launcher = kernel_root/'run-kernel.ps1'
    launcher.write_text(f'''$ErrorActionPreference = 'Stop'
$env:SystemRoot = 'C:\\Windows'
$env:ComSpec = 'C:\\Windows\\System32\\cmd.exe'
$env:PATH = 'C:\\Windows\\System32;C:\\Windows;C:\\Windows\\System32\\WindowsPowerShell\\v1.0'
$env:NANO_HOME = {psquote(kernel_directory)}
$taskProcess = Start-Process -FilePath {psquote(kernel_win)} -ArgumentList @('-c',{psquote(config_win)}) -WindowStyle Hidden -PassThru -RedirectStandardError {psquote(log_win)}
[IO.File]::WriteAllText({psquote(pid_win)}, [string]$taskProcess.Id)
try {{ $taskProcess.WaitForExit() }} finally {{ if (!$taskProcess.HasExited) {{ Stop-Process -Id $taskProcess.Id -Force }} }}
''', encoding='utf-8-sig')
    tunnel, kernel, fixture = None, None, None
    with (opts.output/'ssh.log').open('w') as ssh_log, (opts.output/'gateway-test.log').open('w') as gateway_log:
        try:
            tunnel = subprocess.Popen(['ssh', '-N', '-o', 'ExitOnForwardFailure=yes', '-L',
                                       f'127.0.0.1:{local_port}:127.0.0.1:{remote_port}', opts.ssh], stderr=ssh_log)
            kernel = subprocess.Popen([POWERSHELL, '-NoProfile', '-Command', launcher.read_text(encoding='utf-8-sig')],
                                      stdin=subprocess.DEVNULL, stdout=ssh_log, stderr=ssh_log)
            deadline = time.monotonic() + 10
            while not (kernel_root/'kernel.pid').exists():
                assert kernel.poll() is None and tunnel.poll() is None, '测试内核或 SSH 转发启动失败，见 ssh.log'
                assert time.monotonic() < deadline, '测试内核启动超时'
                time.sleep(.1)
            if not opts.fixture_root:
                fixture = subprocess.Popen([str(root/'desktop-fixture.exe')])
            deadline = time.monotonic() + 12
            while not (root/'fixture.json').exists():
                assert time.monotonic() < deadline, '独立 Windows 窗口没有启动'
                time.sleep(.1)
            info = json.loads((root/'fixture.json').read_text())
            command = ['timeout', '-s', 'INT', '-k', '10s', '150s', 'python3', remote_root+'/'+helper,
                       '--gateway', opts.gateway, '--kernel', opts.kernel, '--executor', opts.executor,
                       '--kernel-listen', f'tcp:127.0.0.1:{remote_port}', '--desktop-machine', node,
                       '--desktop-driver', opts.driver, '--desktop-driver-arg=--grant', '--desktop-driver-arg=existing-profile',
                       '--desktop-workspace', root_win, '--desktop-pid', str(info['pid']),
                       '--desktop-window', str(info['window_id']), '--desktop-output', root_win+'\\application-output.txt',
                       '--report', remote_root+'/report.json']
            if opts.real_model_home:
                command += ['--source-home', opts.real_model_home]
            print('测试目录', root, '远端', remote_root, flush=True)
            result = subprocess.run(['ssh', opts.ssh, shlex.join(command)], stdout=gateway_log, stderr=gateway_log, timeout=170)
            assert result.returncode == 0, '远端网关测试失败：' + (opts.output/'gateway-test.log').read_text()[-4000:]
            subprocess.run(['scp', f'{opts.ssh}:{remote_root}/report.json', str(opts.output/'gateway-report.json')], check=True)
            subprocess.run(['scp', f'{opts.ssh}:{remote_root}/report.png', str(opts.output/'window.png')], check=True)
            gateway_report = json.loads((opts.output/'gateway-report.json').read_text())
            assert gateway_report['ok']
            if opts.real_model_home:
                assert gateway_report['model_kind'] == 'real' and gateway_report['screenshot_results'] > 0
            else:
                assert gateway_report['image_requests'] > 0
            actual = (root/'application-output.txt').read_text(encoding='utf-8')
            assert actual == '中文验证：你好 nano 42', repr(actual)
            image = Image.open(opts.output/'window.png').convert('RGB')
            assert min(image.size) > 20 and any(lo != hi for lo, hi in image.getextrema()), '驱动截图是空白或纯色'
            report = {'ok': True, 'mode': 'linux-gateway-windows-kernel', 'desktop_machine': node,
                      'application_output': actual, 'image_requests': gateway_report.get('image_requests'),
                      'model_kind': gateway_report.get('model_kind', 'deterministic'), 'model': gateway_report.get('model'),
                      'windows_directory': root_win, 'remote_directory': remote_root,
                      'checks': ['真实 Linux 网关经 Windows C 内核的 pipe_* 调用桌面 Driver',
                                 '自主任务经审批后取得截图并传给模型接口，写入完整中英文并点击确认',
                                 '网关通过 Windows 执行器读取实际应用输出，关联真实 Read 证据验收',
                                 '测试端独立核对应用写出的 UTF-8 文件和非空白截图']}
            for check in report['checks']:
                print('ok', check, flush=True)
        finally:
            try:
                if (kernel_root/'kernel.pid').exists():
                    stop_windows(int((kernel_root/'kernel.pid').read_text(encoding='utf-8-sig')), kernel_win)
                if fixture is not None and (root/'fixture.json').exists():
                    stop_windows(json.loads((root/'fixture.json').read_text())['pid'], root_win+'\\desktop-fixture.exe')
            finally:
                stop(kernel)
                stop(tunnel)
                stop(fixture)
    report_file.write_text(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
