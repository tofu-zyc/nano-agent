#!/usr/bin/env python3
"""网关查看/控制 e2e：临时 NANO_HOME、后台进程模式、mock 模型、不碰真实配置和服务。"""
import json
import os
import queue
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import http.cookiejar

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
BIN = os.path.join(ROOT, "gateway", "nano-gateway")
TMP = tempfile.mkdtemp(prefix="nano-control-")
HOME = os.path.join(TMP, "home")
CFG = os.path.join(TMP, "gateway.toml")
CONF_DIR = os.path.join(TMP, "config")
DATA = os.path.join(TMP, "data")
RUN = os.path.join(TMP, "run")
os.makedirs(os.path.join(HOME, "skills"), exist_ok=True)
os.makedirs(CONF_DIR, exist_ok=True)
os.makedirs(DATA, exist_ok=True)
os.makedirs(RUN, exist_ok=True)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


PORT = free_port()
with open(CFG, "w", encoding="utf-8") as f:
    f.write(f"""[kernel]
listen = "unix:{TMP}/k.sock"
token = "control-e2e"
local = false
[llm]
provider = "mock"
[skills]
dir = "{HOME}/skills"
[persona]
dir = "{TMP}/personas"
[session]
data_dir = "{DATA}"
[workspace]
mode = "auto"
[cli]
enabled = true
[web]
listen = "127.0.0.1:{PORT}"
""")
env = dict(os.environ, NANO_HOME=TMP, XDG_CONFIG_HOME=CONF_DIR)
env.pop("NANO_GATEWAY_SUPERVISED", None)


def run(*args, timeout=20):
    return subprocess.run([BIN, *args, "-c", CFG], env=env, capture_output=True, text=True, timeout=timeout)


def expect_output(desc, text, want):
    assert want in text, f"{desc}：没等到 {want!r}\n{text}"


def wait_status(want="网关运行中", timeout=20):
    end = time.time() + timeout
    last = ""
    while time.time() < end:
        r = run("status")
        last = r.stdout + r.stderr
        if want in last:
            return last
        time.sleep(0.2)
    raise AssertionError(f"等不到状态 {want}：\n{last}")


class Chat:
    def __init__(self):
        self.p = subprocess.Popen([BIN, "chat", "-c", CFG], env=env, stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
        self.q = queue.Queue()
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        for line in self.p.stdout:
            self.q.put(line.rstrip("\n"))

    def send(self, s):
        self.p.stdin.write(s + "\n")
        self.p.stdin.flush()

    def expect(self, want, timeout=20):
        seen = []
        end = time.time() + timeout
        while time.time() < end:
            try:
                line = self.q.get(timeout=0.2)
            except queue.Empty:
                continue
            seen.append(line)
            if want in line:
                return
        raise AssertionError(f"没等到 {want!r}；最近输出：{seen[-10:]}")

    def close(self):
        self.p.terminate()
        try:
            self.p.wait(3)
        except subprocess.TimeoutExpired:
            self.p.kill()


def http_call(opener, method, path, body=None):
    req = urllib.request.Request("http://127.0.0.1:%d%s" % (PORT, path), method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Content-Type": "application/json", "X-Nano": "1"})
    try:
        with opener.open(req, timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


chat = None
try:
    r = run("version")
    assert r.returncode == 0 and r.stdout.strip(), (r.returncode, r.stdout, r.stderr)
    print("  ok   version")

    r = run("start")
    expect_output("start", r.stdout + r.stderr, "启动")
    print("  ok   start")

    out = wait_status()
    expect_output("status", out, "运行方式 background")
    print("  ok   status")

    r = run("logs", "-n", "5")
    assert r.returncode == 0 and "网关已启动" in (r.stdout + r.stderr), r.stdout + r.stderr
    print("  ok   logs")
    r = run("url")
    assert r.returncode == 0 and "#k=" in r.stdout, r.stdout + r.stderr
    print("  ok   url")

    # 终端命令 restart 也先检查配置，走 stop/start
    r = run("restart")
    expect_output("restart", r.stdout + r.stderr, "启动")
    wait_status()
    print("  ok   restart（终端）")

    chat = Chat()
    chat.expect("已连上网关")
    chat.send("/status")
    chat.expect("网关运行中")
    chat.send("/help")
    chat.expect("/status")
    chat.expect("/restart")
    print("  ok   /status、/help")

    chat.send("/restart")
    chat.expect("网关正在重启")
    chat.expect("网关已重启")
    print("  ok   /restart 和重启后的回话")

    # 网页重启：登录后 POST /api/restart；重启后 Cookie 仍然有效
    url = open(os.path.join(DATA, "web.url"), encoding="utf-8").read().strip()
    pw = url.split("#k=", 1)[1]
    base = "http://127.0.0.1:%d" % PORT
    cj = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    code, _ = http_call(op, "POST", "/api/login", {"password": pw})
    assert code == 200, code
    code, body = http_call(op, "POST", "/api/restart", {})
    assert code == 200, (code, body)
    time.sleep(2.5)
    code, body = http_call(op, "GET", "/api/me")
    assert code == 200, (code, body)
    code, body = http_call(op, "GET", "/api/overview")
    assert code == 200 and json.loads(body).get("runtime", {}).get("uptime_ms", 0) > 0, body
    print("  ok   网页重启，重启后登录仍有效")

    # 配置写坏时聊天 /restart 被拒绝，网关还在运行
    backup = CFG + ".bak"
    shutil.copy2(CFG, backup)
    with open(CFG, "w", encoding="utf-8") as f:
        f.write("[broken\n")
    chat.send("/restart")
    chat.expect("没有重启")
    shutil.copy2(backup, CFG)
    chat.send("/status")
    chat.expect("网关运行中")
    print("  ok   坏配置拒绝重启，网关保持运行")

    # 看护进程：kill -9 网关子进程，几秒后换一个新 pid
    pid1 = int(open(os.path.join(RUN, "gateway.pid")).read().strip())
    os.kill(pid1, signal.SIGKILL)
    time.sleep(4)
    pid2 = int(open(os.path.join(RUN, "gateway.pid")).read().strip())
    assert pid2 != pid1, (pid1, pid2)
    wait_status()
    print("  ok   看护进程在 kill -9 后拉起网关")

    r = run("stop")
    expect_output("stop", r.stdout + r.stderr, "停止")
    r = run("status")
    assert r.returncode != 0 and "没有在运行" in (r.stdout + r.stderr), (r.returncode, r.stdout, r.stderr)
    print("  ok   stop")
    print("control-e2e: 12 passed")
finally:
    if chat:
        chat.close()
    try:
        run("stop", timeout=10)
    except Exception:
        pass
    shutil.rmtree(TMP, ignore_errors=True)
