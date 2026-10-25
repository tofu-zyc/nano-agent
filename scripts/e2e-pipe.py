#!/usr/bin/env python3
"""内核管道透传的端到端测试：假装成网关，跟真内核说话。"""
import json, os, socket, subprocess, tempfile, time, sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
# NANO_TEST_KERNEL 可以换成别的内核（比如 -fsanitize=address 编出来的，查内存问题）
KERNEL = os.environ.get('NANO_TEST_KERNEL') or os.path.join(ROOT, 'kernel', 'nano-kernel')
tmp = tempfile.mkdtemp(prefix='nano-pipe-')
sock = os.path.join(tmp, 'k.sock')
conf = os.path.join(tmp, 'kernel.conf')
open(conf, 'w').write('gateway = unix:%s\ntoken = e2e\nnode = test\nexec = %s\n' % (sock, os.path.join(ROOT, 'kernel', 'nano-exec')))
# 内核是客户端：先监听，内核连上来再 accept
srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
srv.bind(sock)
srv.listen(1)
srv.settimeout(15)
proc = subprocess.Popen([KERNEL, '-c', conf], stdout=subprocess.DEVNULL, stderr=open(os.path.join(tmp, 'k.log'), 'w'))
fails = []
def ok(msg): print('  ok   ' + msg)
def fail(msg):
    fails.append(msg); print('  FAIL ' + msg)
try:
    s, _ = srv.accept()
    s.settimeout(5)
    buf = [b'']
    def send(**m):
        s.sendall((json.dumps(m) + '\n').encode())
    def recv():
        while b'\n' not in buf[0]:
            d = s.recv(65536)
            if not d: raise EOFError('内核断开了')
            buf[0] += d
        line, rest = buf[0].split(b'\n', 1)
        buf[0] = rest
        return json.loads(line)
    hello = recv()
    if hello.get('t') != 'hello':
        fail('内核第一条不是 hello：%r' % hello)
    else:
        ok('内核 hello 到了（v=%s node=%s）' % (hello.get('v'), hello.get('node')))
    send(t='welcome', v=10)
    time.sleep(0.2)
    # 起一个 cat：写什么就回什么
    send(t='pipe_open', pipe='p1', argv=['/bin/cat'])
    time.sleep(0.3)
    send(t='pipe_write', pipe='p1', data='hello\u4e16\u754c')
    got = None
    for _ in range(50):
        m = recv()
        if m.get('t') == 'pipe_data' and m.get('pipe') == 'p1':
            got = m.get('data'); break
    if got == 'hello\u4e16\u754c': ok('pipe_write → pipe_data 原样回来')
    else: fail('pipe_data 不对：%r' % got)
    # 再写一行，确认是流不是一次性
    send(t='pipe_write', pipe='p1', data='second')
    got2 = None
    for _ in range(50):
        m = recv()
        if m.get('t') == 'pipe_data' and m.get('data') == 'second':
            got2 = m.get('data'); break
    if got2: ok('连续写第二行也回来了')
    else: fail('第二行没回来')
    # 关掉：cat 看到 EOF 就退出
    send(t='pipe_close', pipe='p1')
    closed = None
    for _ in range(80):
        m = recv()
        if m.get('t') == 'pipe_closed' and m.get('pipe') == 'p1':
            closed = m; break
    if closed is not None and closed.get('exit') == 0: ok('pipe_close → pipe_closed（exit=0）')
    elif closed is not None: ok('pipe_close → pipe_closed（exit=%s）' % closed.get('exit'))
    else: fail('没有收到 pipe_closed')
    # 起不来的命令要回一条错误，而不是卡住
    send(t='pipe_open', pipe='p2', argv=['/nonexistent-cmd-xyz'])
    bad = None
    for _ in range(80):
        m = recv()
        if m.get('t') == 'pipe_closed' and m.get('pipe') == 'p2':
            bad = m; break
    if bad is not None: ok('命令不存在时回 pipe_closed（exit=%s）' % bad.get('exit'))
    else: fail('命令不存在时没有回 pipe_closed')
    # 网关以为还活着、内核这边已经没有的管道（内核重启过）：写过去要回 pipe_closed，不能让网关干等
    send(t='pipe_write', pipe='gone', data='{}')
    stale = None
    for _ in range(80):
        m = recv()
        if m.get('t') == 'pipe_closed' and m.get('pipe') == 'gone':
            stale = m; break
    if stale is not None: ok('写不存在的管道回 pipe_closed（%s）' % stale.get('detail'))
    else: fail('写不存在的管道没有回 pipe_closed')
    # 网关突然断开：内核要把它让起的进程（连同进程组）都收掉，自己不能崩，还能重连
    send(t='pipe_open', pipe='p3', argv=['sh', '-c', 'echo $$ > "$0"; while :; do echo tick; sleep 0.002; done',
                                         os.path.join(tmp, 'p3.pid')])
    time.sleep(0.5)
    def alive(pid):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        st = open('/proc/%d/stat' % pid).read().split(') ')[-1].split()[0] if os.path.exists('/proc/%d/stat' % pid) else ''
        return st != 'Z'
    try: p3 = int(open(os.path.join(tmp, 'p3.pid')).read())
    except Exception: p3 = 0
    if not p3: fail('p3 没起来')
    s.shutdown(socket.SHUT_RDWR); s.close()
    s, _ = srv.accept()
    s.settimeout(5)
    buf[0] = b''
    again = recv()
    if again.get('t') == 'hello': ok('网关断开后内核没崩，重新连上来了')
    else: fail('断开后重连的第一条不是 hello：%r' % again)
    if p3 and not alive(p3): ok('网关断开时 MCP 进程被收掉了')
    elif p3: fail('网关断开以后 MCP 进程还活着（pid %d）' % p3)
    if proc.poll() is not None: fail('内核退出了（%s）' % proc.returncode)
    s.close()
except Exception as e:
    fail('异常：%s' % e)
finally:
    proc.terminate()
    try: proc.wait(5)
    except Exception: proc.kill()
print('pipe e2e: %d failures' % len(fails))
sys.exit(1 if fails else 0)