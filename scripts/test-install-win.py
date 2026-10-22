#!/usr/bin/env python3
"""Windows 内核离线安装测试：在临时 USERPROFILE 里跑 install-kernel.ps1。

覆盖：安装（不注册服务）、启动配对、升级保留 paired.token、卸载。
网关是 Python 假实现，只需要配对和 welcome；不联网。
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
CMD = "/mnt/c/Windows/System32/cmd.exe"
POWERSHELL = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
PAIRED = "paired-install-test"


def wslpath(flag, p):
    return subprocess.check_output(["wslpath", flag, p], text=True).strip()


def win_temp():
    out = subprocess.run([CMD, "/c", "echo %TEMP%"], cwd="/mnt/c", capture_output=True, text=True)
    return out.stdout.strip().splitlines()[-1].strip()


class FakeGateway(threading.Thread):
    def __init__(self):
        super().__init__(daemon=True)
        self.sock = socket.socket()
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen()
        self.port = self.sock.getsockname()[1]
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
                f.write((json.dumps({"t": "welcome", "v": 8}) + "\n").encode())
                f.flush()
                self.accepted.set()
                return
            f.write((json.dumps({"t": "pairing", "code": "123456"}) + "\n").encode())
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


def ps(script, *args):
    return subprocess.run([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, *args],
                          cwd="/mnt/c", capture_output=True, text=True, errors="replace")


def wait_file(path, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if os.path.exists(path):
            return True
        time.sleep(0.2)
    return False


TMP_L = tempfile.mkdtemp(prefix="nano-inst-win-", dir=wslpath("-u", win_temp()))
TMP_W = wslpath("-w", TMP_L)
PKG_L = os.path.join(TMP_L, "pkg")
INST_L = os.path.join(TMP_L, "kernel")
os.makedirs(PKG_L)
for src, dst in [("kernel/build/nano-kernel.exe", "nano-kernel.exe"), ("kernel/build/nano-exec.exe", "nano-exec.exe"),
                 ("build/tools/rg/windows-x86_64.exe", "rg.exe"), ("scripts/install-kernel.ps1", "install-kernel.ps1")]:
    shutil.copy2(os.path.join(ROOT, src), os.path.join(PKG_L, dst))
with open(os.path.join(PKG_L, "VERSION"), "w", encoding="ascii") as f:
    f.write("test\n")
PKG_PS1 = TMP_W + "\\pkg\\install-kernel.ps1"
INST_W = TMP_W + "\\kernel"
CONF = os.path.join(INST_L, "kernel.conf")
TOKEN = os.path.join(INST_L, "paired.token")
RUNNER = os.path.join(INST_L, "run-kernel.ps1")
RUNNER_W = INST_W + "\\run-kernel.ps1"
LOG = os.path.join(INST_L, "kernel.log")

gw = FakeGateway()
gw.start()
runner = None


def cleanup_kernel():
    clean = os.path.join(TMP_L, "clean.ps1")
    clean_w = TMP_W + "\\clean.ps1"
    with open(clean, "w", encoding="utf-8-sig", newline="\r\n") as f:
        f.write("Get-Process nano-kernel -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq '" +
                INST_W + "\\nano-kernel.exe' } | Stop-Process -Force\n")
    ps(clean_w)


try:
    r = ps(PKG_PS1, "-Gateway", f"tcp:127.0.0.1:{gw.port}", "-Name", "installpc", "-Yes", "-NoService", "-Dir", INST_W)
    assert r.returncode == 0 and os.path.exists(CONF), r.stdout + r.stderr
    conf = open(CONF, encoding="utf-8-sig").read()
    assert f"tcp:127.0.0.1:{gw.port}" in conf and "node = installpc" in conf and "exec = " in conf, conf
    assert not os.path.exists(TOKEN)
    print("  ok   安装（离线包，不注册服务）")

    # 手动启动安装包里生成的 run-kernel.ps1；内核请求配对，假网关发口令
    runner = subprocess.Popen([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", RUNNER_W],
                              cwd="/mnt/c", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    assert wait_file(TOKEN, 30), "没有拿到 paired.token"
    assert gw.paired.wait(10) and gw.accepted.wait(10), "假网关没有收到配对和重连"
    print("  ok   启动后配对并重新连上网关")

    old_token = open(TOKEN, encoding="ascii").read().strip()
    cleanup_kernel()
    try:
        runner.wait(5)
    except subprocess.TimeoutExpired:
        runner.kill()

    r = ps(PKG_PS1, "-Gateway", f"tcp:127.0.0.1:{gw.port}", "-Name", "installpc2", "-Yes", "-NoService", "-Dir", INST_W)
    assert r.returncode == 0, r.stdout + r.stderr
    conf = open(CONF, encoding="utf-8-sig").read()
    assert "node = installpc2" in conf and f"tcp:127.0.0.1:{gw.port}" in conf, conf
    assert os.path.exists(TOKEN) and open(TOKEN, encoding="ascii").read().strip() == old_token, "升级丢了配对口令"
    print("  ok   升级保留配置和 paired.token")

    r = ps(PKG_PS1, "-Uninstall", "-Yes", "-Dir", INST_W)
    assert r.returncode == 0 and not os.path.exists(INST_L), r.stdout + r.stderr
    print("  ok   卸载")
    print("install-win: 4 passed")
finally:
    if runner:
        try:
            runner.kill()
        except Exception:
            pass
    cleanup_kernel()
    gw.close()
    shutil.rmtree(TMP_L, ignore_errors=True)
