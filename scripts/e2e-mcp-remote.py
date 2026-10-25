#!/usr/bin/env python3
"""MCP 透传的端到端测试：真网关 + 真内核 + 内核那台机器上的 MCP 进程。"""
import json, os, socket, subprocess, tempfile, time, urllib.request, sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
GATEWAY = os.path.join(ROOT, 'gateway', 'nano-gateway')
KERNEL = os.path.join(ROOT, 'kernel', 'nano-kernel')
SERVER = os.path.join(ROOT, 'scripts', 'fixtures', 'mcp-echo.py')
PW = 'mcp-pw'
tmp = tempfile.mkdtemp(prefix='nano-mcpe2e-')
home, nano, sockdir = tmp + '/home', tmp + '/nano', tmp + '/sock'
os.makedirs(home); os.makedirs(nano); os.makedirs(sockdir)
ksock = os.path.join(sockdir, 'k.sock')
def free_port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p
port = free_port()
open(nano + '/gateway.toml', 'w').write('[kernel]\nlisten = "unix:%s"\ntoken = "t"\n[web]\nlisten = "127.0.0.1:%d"\npassword = "%s"\n[cli]\nenabled = false\n[llm]\nprovider = "mock"\nmodel = "mock"\n' % (ksock, port, PW))
open(tmp + '/kernel.conf', 'w').write('gateway = unix:%s\ntoken = t\nnode = pc\nexec = %s\n' % (ksock, os.path.join(ROOT, 'kernel', 'nano-exec')))
env = {'PATH': '/usr/bin:/bin', 'HOME': home, 'NANO_HOME': nano, 'USERPROFILE': home, 'XDG_CONFIG_HOME': home + '/.config'}
gw = subprocess.Popen([GATEWAY, '-c', nano + '/gateway.toml'], cwd=tmp, env=env, stdout=subprocess.DEVNULL, stderr=open(tmp + '/gw.log', 'w'))
url = 'http://127.0.0.1:%d/' % port
for _ in range(100):
    try: urllib.request.urlopen(url, timeout=1); break
    except Exception: time.sleep(0.1)
kn = subprocess.Popen([KERNEL, '-c', tmp + '/kernel.conf'], stdout=subprocess.DEVNULL, stderr=open(tmp + '/k.log', 'w'))
fails = []
def ok(m): print('  ok   ' + m)
def fail(m):
    fails.append(m); print('  FAIL ' + m)
def api(method, path, body=None, cookie=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url.rstrip('/') + path, data=data, method=method)
    req.add_header('Content-Type', 'application/json')
    req.add_header('X-Nano', '1')
    if cookie: req.add_header('Cookie', cookie)
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            return r.status, json.loads(r.read() or b'{}'), r.headers.get('Set-Cookie')
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), None
try:
    st, _, ck = api('POST', '/api/login', {'password': PW})
    cookie = (ck or '').split(';')[0]
    node = None
    for i in range(100):
        st, ov, _ = api('GET', '/api/overview', cookie=cookie)
        ks = (ov.get('kernels') or []) if isinstance(ov, dict) else []
        if ks:
            pick = next((k for k in ks if not k.get('managed')), ks[0])
            node = pick.get('node')
            break
        time.sleep(0.2)
    if not node: raise SystemExit('内核没连上')
    ok('内核连上了：%s' % node)
    st, out, _ = api('POST', '/api/mcp', {'items': [{
        'name': 'remote', 'machine': node, 'kind': 'stdio',
        'command': sys.executable, 'args': [SERVER], 'enabled': True}]}, cookie)
    if st == 200: ok('配置了 machine=%s 的 MCP 服务' % node)
    else: fail('保存失败：%s %s' % (st, str(out)[:150]))
    st, out, _ = api('POST', '/api/mcp/test', {'name': 'remote'}, cookie)
    tools = [t.get('Exposed') for t in (out.get('tools') or [])] if isinstance(out, dict) else []
    if tools == ['remote__echo']: ok('经内核透传列到了工具：%s' % tools)
    else: fail('列工具不对：%s' % str(out)[:200])
    st, out, _ = api('POST', '/api/mcp', {'items': [{
        'name': 'remote', 'machine': node, 'kind': 'stdio',
        'command': '/nonexistent-xyz', 'args': [], 'enabled': True}]}, cookie)
    st, out, _ = api('POST', '/api/mcp/test', {'name': 'remote'}, cookie)
    err = out.get('error') if isinstance(out, dict) else str(out)
    if err and 'exit' in str(err).lower() or (err and '127' in str(err)): ok('命令不存在时报错：%s' % str(err)[:80])
    elif err: ok('命令不存在时报错：%s' % str(err)[:80])
    else: fail('命令不存在时没有报错')
finally:
    for p in (kn, gw):
        try: p.terminate()
        except Exception: pass
    for p in (kn, gw):
        try: p.wait(5)
        except Exception: p.kill()
print('mcp remote e2e: %d failures' % len(fails))
sys.exit(1 if fails else 0)