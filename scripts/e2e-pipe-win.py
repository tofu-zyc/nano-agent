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
    def wait_for(pred, n=200):
        for _ in range(n):
            m = recv()
            if pred(m): return m
        return None
    # 写 stdin：findstr 原样输出每一行；关闭时写线程要先把攒下的写完再关 stdin
    send(t='pipe_open', pipe='p3', argv=['C:\\Windows\\System32\\findstr.exe', '/R', '^'])
    time.sleep(0.5)
    send(t='pipe_write', pipe='p3', data='hello\u4e16\u754c')
    send(t='pipe_write', pipe='p3', data='second')
    send(t='pipe_close', pipe='p3')
    lines, closed3 = [], None
    for _ in range(200):
        m = recv()
        if m.get('pipe') != 'p3': continue
        if m.get('t') == 'pipe_data': lines.append(m.get('data'))
        if m.get('t') == 'pipe_closed': closed3 = m; break
    if lines == ['hello\u4e16\u754c', 'second']: ok('pipe_write 写进去的两行都原样回来了')
    else: fail('写进去的没有原样回来：%r' % lines)
    if closed3 is not None and closed3.get('exit') == 0: ok('pipe_close：写完再关 stdin，进程正常退出')
    else: fail('pipe_close 之后的退出不对：%r' % closed3)
    # 对方不读 stdin 时写一大块：主循环不能卡在 WriteFile 上（以前是阻塞写，整个内核跟着停）
    send(t='pipe_open', pipe='p4', argv=['C:\\Windows\\System32\\PING.EXE', '-n', '30', '127.0.0.3'])
    time.sleep(0.5)
    send(t='pipe_write', pipe='p4', data='x' * (2 << 20))
    t0 = time.time()
    send(t='pipe_write', pipe='gone', data='{}')
    stale = wait_for(lambda m: m.get('t') == 'pipe_closed' and m.get('pipe') == 'gone')
    if stale is not None and time.time() - t0 < 5: ok('对方不读时大块写入没有卡住内核（%.1fs 内回了下一条）' % (time.time() - t0))
    elif stale is not None: fail('大块写入以后内核卡了 %.1fs' % (time.time() - t0))
    else: fail('写不存在的管道没有回 pipe_closed')
    # 网关突然断开：内核把进程收掉，自己还能重连
    s.shutdown(socket.SHUT_RDWR); s.close()
    s, _ = srv.accept(); s.settimeout(10)
    buf[0] = b''
    again = recv()
    if again.get('t') == 'hello': ok('网关断开后内核没崩，重新连上来了')
    else: fail('断开后重连的第一条不是 hello：%r' % again)
    time.sleep(1)
    left = subprocess.run([POWERSHELL, '-NoProfile', '-Command',
                           "@(Get-CimInstance Win32_Process -Filter \"Name='PING.EXE'\" | Where-Object { $_.CommandLine -like '*127.0.0.3*' }).Count"],
                          capture_output=True, text=True, timeout=30).stdout.strip()
    if left == '0': ok('网关断开时 Windows 上的 MCP 进程被收掉了')
    else: fail('网关断开以后进程还在：%r' % left)
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