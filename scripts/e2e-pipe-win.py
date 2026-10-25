#!/usr/bin/env python3
"""Windows 内核的管道透传测试：假网关（TCP）+ 真 Windows 内核。"""
import json, os, shutil, socket, subprocess, sys, tempfile, time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
CMD = '/mnt/c/Windows/System32/cmd.exe'
POWERSHELL = '/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe'
if not os.path.exists(CMD) or not os.path.exists(os.path.join(ROOT, 'kernel/build/nano-kernel.exe')):
    print('（没有 WSL interop 或 Windows 内核，跳过）')
    sys.exit(0)

def wslpath(flag, p):
    return subprocess.run(['wslpath', flag, p], capture_output=True, text=True).stdout.strip()

def free_port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p

win_tmp = wslpath('-u', subprocess.run([CMD, '/c', 'echo %TEMP%'], cwd='/mnt/c', capture_output=True, text=True).stdout.strip().splitlines()[-1].strip())
ROOT_W = tempfile.mkdtemp(prefix='nano-pipe-win-', dir=win_tmp)
ROOT_W_WIN = wslpath('-w', ROOT_W)
for name in ('nano-kernel.exe', 'nano-exec.exe'):
    shutil.copy2(os.path.join(ROOT, 'kernel/build', name), os.path.join(ROOT_W, name))
PORT = free_port()
conf = os.path.join(ROOT_W, 'kernel.conf')
with open(conf, 'w', encoding='utf-8', newline='\n') as f:
    f.write('gateway = tcp:127.0.0.1:%d\ntoken = e2e\nnode = winpc\nexec = %s\\nano-exec.exe\n' % (PORT, ROOT_W_WIN))
ps1 = os.path.join(ROOT_W, 'run.ps1')
log = os.path.join(ROOT_W, 'kernel.log')
with open(ps1, 'w', encoding='utf-8-sig', newline='\r\n') as f:
    f.write('$ErrorActionPreference = "Stop"\n')
    f.write('$env:SystemRoot = \'C:\\Windows\'\n$env:ComSpec = \'C:\\Windows\\System32\\cmd.exe\'\n')
    f.write('$env:PATHEXT = \'.COM;.EXE;.BAT;.CMD\'\n')
    f.write('$env:PATH = \'C:\\Windows\\System32;C:\\Windows;C:\\Windows\\System32\\Wbem;C:\\Windows\\System32\\WindowsPowerShell\\v1.0\'\n')
    f.write('$p = Start-Process -FilePath \'%s\\nano-kernel.exe\' -ArgumentList @(\'-c\',\'%s\\kernel.conf\') -NoNewWindow -PassThru -RedirectStandardError \'%s\'\n' % (ROOT_W_WIN, ROOT_W_WIN, log))
    f.write('$p.WaitForExit()\n')
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(('127.0.0.1', PORT)); srv.listen(1); srv.settimeout(25)
k = subprocess.Popen([POWERSHELL, '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', wslpath('-w', ps1)],
                     stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
fails = []
def ok(m): print('  ok   ' + m)
def fail(m):
    fails.append(m); print('  FAIL ' + m)
try:
    s, _ = srv.accept(); s.settimeout(10)
    buf = [b'']
    def recv():
        while b'\n' not in buf[0]:
            d = s.recv(65536)
            if not d: raise EOFError('内核断开了')
            buf[0] += d
        line, rest = buf[0].split(b'\n', 1); buf[0] = rest
        return json.loads(line)
    def send(**m):
        s.sendall((json.dumps(m) + '\n').encode())
    hello = recv()
    if hello.get('t') == 'hello': ok('Windows 内核 hello（v=%s node=%s）' % (hello.get('v'), hello.get('node')))
    else: fail('第一条不是 hello：%r' % hello)
    send(t='welcome', v=10)
    time.sleep(0.3)
    send(t='pipe_open', pipe='p1', argv=['C:\\Windows\\System32\\cmd.exe', '/c', 'echo hi'])
    got, closed = None, None
    for _ in range(60):
        m = recv()
        if m.get('t') == 'pipe_data' and m.get('data') == 'hi': got = m
        if m.get('t') == 'pipe_closed' and m.get('pipe') == 'p1':
            closed = m; break
    if got: ok('cmd /c echo hi 的输出回来了（pipe_data）')
    else: fail('没收到 pipe_data')
    if closed: ok('进程退出收到 pipe_closed（exit=%s）' % closed.get('exit'))
    else: fail('没收到 pipe_closed')
    send(t='pipe_open', pipe='p2', argv=['C:\\definitely-not-here.exe'])
    bad = None
    for _ in range(60):
        m = recv()
        if m.get('t') == 'pipe_closed' and m.get('pipe') == 'p2':
            bad = m; break
    if bad is not None: ok('命令不存在时回 pipe_closed（exit=%s）' % bad.get('exit'))
    else: fail('命令不存在时没有回 pipe_closed')
    s.close()
except Exception as e:
    fail('异常：%s' % e)
finally:
    k.terminate()
    try: k.wait(5)
    except Exception: k.kill()
    try:
        subprocess.run([POWERSHELL, '-NoProfile', '-Command',
                        'Get-Process nano-kernel -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq \'%s\\nano-kernel.exe\' } | Stop-Process -Force' % ROOT_W_WIN],
                       capture_output=True, timeout=20)
    except Exception: pass
print('pipe-win: %d failures' % len(fails))
sys.exit(1 if fails else 0)