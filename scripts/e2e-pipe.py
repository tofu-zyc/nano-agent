#!/usr/bin/env python3
"""内核管道透传的端到端测试：假装成网关，跟真内核说话。"""
import json, os, socket, subprocess, tempfile, time, sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
KERNEL = os.path.join(ROOT, 'kernel', 'nano-kernel')
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
    s.close()
except Exception as e:
    fail('异常：%s' % e)
finally:
    proc.terminate()
    try: proc.wait(5)
    except Exception: proc.kill()
print('pipe e2e: %d failures' % len(fails))
sys.exit(1 if fails else 0)