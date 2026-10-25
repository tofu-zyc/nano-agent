#!/usr/bin/env python3
"""Linux 离线安装测试：用 make release 的包，在临时 HOME 里安装、配对、升级、卸载。

网关是 Python 假实现（unix socket），只处理配对和 welcome；systemctl 是假的（总是失败），
所以安装脚本走没有 systemd 的后台进程分支。不会碰真实的 ~/.nano-agent。
"""
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
PAIRED = "paired-linux-install-test"


def wslpath(flag, p):
    return subprocess.check_output(["wslpath", flag, p], text=True).strip()


class FakeGateway(threading.Thread):
    def __init__(self, path):
        super().__init__(daemon=True)
        self.path = path
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(path)
        self.sock.listen()
        self.accepted = threading.Event()
        self.paired = threading.Event()
        self.stop = threading.Event()
        self.clients = []

    def run(self):
        while not self.stop.is_set():
            try:
                c, _ = self.sock.accept()
            except OSError:
                return
            threading.Thread(target=self.handle, args=(c,), daemon=True).start()

    def handle(self, c):
        self.clients.append(c)
        try:
            f = c.makefile("rwb")
            line = f.readline()
            if not line:
                return
            hello = json.loads(line)
            if hello.get("token") == PAIRED:
                f.write((json.dumps({"t": "welcome", "v": 10}) + "\n").encode())
                f.flush()
                self.accepted.set()
                time.sleep(30)
                return
            f.write((json.dumps({"t": "pairing", "code": "654321"}) + "\n").encode())
            f.flush()
            time.sleep(0.3)
            f.write((json.dumps({"t": "paired", "token": PAIRED}) + "\n").encode())
            f.flush()
            self.paired.set()
        except Exception:
            pass
        finally:
            try:
                c.close()
            except Exception:
                pass

    def close(self):
        self.stop.set()
        try:
            self.sock.close()
        except Exception:
            pass
        for c in self.clients:
            try:
                c.close()
            except Exception:
                pass


def run_install(args, env):
    pkg = os.path.join(TMP, "pkg", "nano-kernel-test-linux-x86_64")
    return subprocess.run([os.path.join(pkg, "install-kernel.sh"), *args], env=env,
                          capture_output=True, text=True, errors="replace")


def wait_file(path, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if os.path.exists(path):
            return True
        time.sleep(0.2)
    return False


if not os.path.exists(os.path.join(ROOT, "dist/nano-kernel-test-linux-x86_64.tar.gz")):
    subprocess.run(["make", "release", "VERSION=test"], cwd=ROOT, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

TMP = tempfile.mkdtemp(prefix="nano-inst-linux-")
PKG = os.path.join(TMP, "pkg")
HOME = os.path.join(TMP, "home")
BIN = os.path.join(TMP, "fakebin")
for d in (PKG, HOME, BIN):
    os.makedirs(d)
subprocess.run(["tar", "xzf", os.path.join(ROOT, "dist/nano-kernel-test-linux-x86_64.tar.gz"), "-C", PKG], check=True)
with open(os.path.join(BIN, "systemctl"), "w", encoding="utf-8") as f:
    f.write("#!/bin/sh\nexit 1\n")
os.chmod(os.path.join(BIN, "systemctl"), 0o755)
SOCK = os.path.join(TMP, "g.sock")
KERNEL_DIR = os.path.join(HOME, ".nano-agent", "kernel")
CONF = os.path.join(KERNEL_DIR, "kernel.conf")
TOKEN = os.path.join(KERNEL_DIR, "paired.token")
CTL = os.path.join(KERNEL_DIR, "nano-kernel")
env = dict(os.environ, HOME=HOME, PATH=BIN + ":" + os.environ.get("PATH", ""))
env.pop("NANO_HOME", None)

gw = FakeGateway(SOCK)
gw.start()
try:
    r = run_install(["--gateway", "unix:" + SOCK, "--name", "linpc", "--yes", "--no-service"], env)
    assert r.returncode == 0, r.stdout + r.stderr
    assert os.path.exists(CONF) and os.path.exists(os.path.join(KERNEL_DIR, "nano-kernel")), r.stdout + r.stderr
    assert os.path.exists(os.path.join(KERNEL_DIR, "rg")), "离线包里的 rg 没有装"
    assert not os.path.exists(TOKEN)
    assert not os.path.exists(os.path.join(KERNEL_DIR, "nano-kernel-ctl")), "不该再有 nano-kernel-ctl"
    print("  ok   安装（离线包，临时 HOME）")

    # 帮助：nano-kernel --help 列出管理命令；命令 --help 只看用法，不会真的执行
    r = subprocess.run([CTL, "--help"], env=env, capture_output=True, text=True)
    assert r.returncode == 0 and "nano-kernel connect" in r.stdout, r.stdout + r.stderr
    r = subprocess.run([CTL, "start", "--help"], env=env, capture_output=True, text=True)
    assert r.returncode == 0 and r.stdout.startswith("用法：nano-kernel start") and not os.path.exists(os.path.join(KERNEL_DIR, "kernel.pid")), r.stdout + r.stderr
    r = subprocess.run([CTL, "status", "extra"], env=env, capture_output=True, text=True)
    assert r.returncode == 2 and "不认识的参数" in r.stderr, r.stdout + r.stderr
    print("  ok   nano-kernel --help / 命令 --help")

    r = subprocess.run([CTL, "start", "--quiet"], env=env, capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr
    assert wait_file(TOKEN, 30), "没有拿到 paired.token"
    assert gw.paired.wait(10) and gw.accepted.wait(10), "假网关没有收到配对和重连"
    print("  ok   启动后配对并重新连上网关")

    # nano-kernel 管理命令：在 PATH 里（~/.nano-agent/bin 的链接）、status、connect 改连另一个网关、stop、start
    NK = os.path.join(HOME, ".nano-agent", "bin", "nano-kernel")
    assert os.path.realpath(NK) == os.path.join(KERNEL_DIR, "nano-kernel"), os.path.realpath(NK)

    def nk(*args):
        r = subprocess.run([NK, *args], env=env, capture_output=True, text=True, errors="replace", timeout=60)
        return r.returncode, r.stdout + r.stderr

    end = time.time() + 15
    while time.time() < end and "已连接" not in nk("status")[1]:
        time.sleep(0.3)
    code, text = nk("status")
    assert code == 0 and "已连接" in text and "linpc" in text and "后台进程" in text, text
    gw2 = FakeGateway(SOCK + "2")
    gw2.start()
    code, text = nk("connect", "unix:" + SOCK + "2", "--name", "linpc-b")
    assert code == 0 and gw2.accepted.wait(15), text
    conf = open(CONF, encoding="utf-8").read()
    assert "gateway = unix:" + SOCK + "2" in conf and "node = linpc-b" in conf, conf
    code, text = nk("stop")
    assert code == 0, text
    code, text = nk("status")
    assert code == 3 and "没有运行" in text, text
    assert not os.path.exists(os.path.join(KERNEL_DIR, "kernel.stop")) and not os.path.exists(os.path.join(KERNEL_DIR, "kernel.pid"))
    code, text = nk("connect", "unix:" + SOCK, "--name", "linpc")
    assert code == 0 and "没有在运行" in text, text
    gw2.close()
    print("  ok   nano-kernel status / connect / stop")

    old_token = open(TOKEN, encoding="ascii").read().strip()
    subprocess.run([CTL, "stop"], env=env, check=True, capture_output=True)
    r = run_install(["--gateway", "unix:" + SOCK, "--name", "linpc2", "--yes", "--no-service"], env)
    assert r.returncode == 0, r.stdout + r.stderr
    conf = open(CONF, encoding="utf-8").read()
    assert "node = linpc2" in conf and "gateway = unix:" + SOCK in conf, conf
    assert open(TOKEN, encoding="ascii").read().strip() == old_token, "升级丢了配对口令"
    print("  ok   升级保留配置和 paired.token")

    r = subprocess.run([os.path.join(PKG, "nano-kernel-test-linux-x86_64", "install-kernel.sh"), "--uninstall", "--yes"],
                       env=env, capture_output=True, text=True, errors="replace")
    assert r.returncode == 0 and not os.path.exists(KERNEL_DIR), r.stdout + r.stderr
    print("  ok   卸载")
    print("install-linux: 6 passed")
finally:
    try:
        subprocess.run([CTL, "stop"], env=env, capture_output=True)
    except Exception:
        pass
    gw.close()
    shutil.rmtree(TMP, ignore_errors=True)
