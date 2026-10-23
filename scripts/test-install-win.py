#!/usr/bin/env python3
"""Windows 内核离线安装测试：在临时 USERPROFILE 里跑 install-kernel.ps1。

覆盖：安装（不注册服务）、启动配对、升级保留 paired.token、卸载。
NANO_TEST_SERVICE=1 时再测注册任务计划程序（GitHub Actions 的 Windows runner 上跑；
WSL interop 里注册任务会被拒绝）。网关是 Python 假实现，只需要配对和 welcome；不联网。
WSL 里通过 interop 跑，或者直接在 Windows 上跑。
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
NATIVE = os.name == "nt"
if NATIVE:
    CMD = os.environ.get("ComSpec", r"C:\Windows\System32\cmd.exe")
    POWERSHELL = "powershell.exe"
    WIN_CWD = None
else:
    CMD = "/mnt/c/Windows/System32/cmd.exe"
    POWERSHELL = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
    WIN_CWD = "/mnt/c"
PAIRED = "paired-install-test"
SERVICE = os.environ.get("NANO_TEST_SERVICE") == "1"
TASK = "nano-agent-kernel-test"


def wslpath(flag, p):
    if NATIVE:
        return os.path.abspath(p)
    return subprocess.check_output(["wslpath", flag, p], text=True).strip()


def win_temp():
    if NATIVE:
        return os.path.realpath(tempfile.gettempdir())
    out = subprocess.run([CMD, "/c", "echo %TEMP%"], cwd=WIN_CWD, capture_output=True, text=True)
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
        self.hellos = []          # 带配对口令的 hello
        self.update_msg = None    # 下一次 hello 后推给内核的更新
        self.update_results = []
        self.hello_times = []
        self.result_time = 0.0
        self.hold = False         # welcome 后保持连接（测 nano-kernel status 的“已连接”）

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
                self.hellos.append(hello)
                self.hello_times.append(time.time())
                f.write((json.dumps({"t": "welcome", "v": 9}) + "\n").encode())
                f.flush()
                if self.update_msg:
                    msg, self.update_msg = self.update_msg, None
                    f.write((json.dumps(msg) + "\n").encode())
                    f.flush()
                    c.settimeout(30)
                    while True:
                        line = f.readline()
                        if not line:
                            break
                        m = json.loads(line)
                        if m.get("t") == "update_result":
                            self.update_results.append(m)
                            self.result_time = time.time()
                            break
                self.accepted.set()
                if self.hold:
                    c.settimeout(None)
                    f.readline()   # 等内核断开（停止、重启）
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
                          cwd=WIN_CWD, capture_output=True, text=True, errors="replace")


def nk(*args):
    """运行安装目录里的 nano-kernel.exe 管理命令"""
    exe = INST_W + "\\nano-kernel.exe"
    return subprocess.run([CMD, "/d", "/c", exe, *args] if not NATIVE else [exe, *args], cwd=WIN_CWD,
                          capture_output=True, timeout=60)


def nk_out(r):
    return (r.stdout + r.stderr).decode("utf-8", "replace")


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
    """结束这次测试目录下的内核和 run-kernel.ps1（在 WSL 里结束 Popen 不会结束 Windows 那边的进程）"""
    clean = os.path.join(TMP_L, "clean.ps1")
    clean_w = TMP_W + "\\clean.ps1"
    with open(clean, "w", encoding="utf-8-sig", newline="\r\n") as f:
        f.write("Get-CimInstance Win32_Process | Where-Object { ($_.ExecutablePath -like '" + TMP_W + "*') -or "
                "($_.CommandLine -and $_.CommandLine.Contains('" + TMP_W + "\\kernel\\run-kernel.ps1')) } | "
                "ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }\n")
    ps(clean_w)


try:
    r = ps(PKG_PS1, "-Gateway", f"tcp:127.0.0.1:{gw.port}", "-Name", "installpc", "-Yes", "-NoService", "-NoPath", "-Dir", INST_W)
    assert r.returncode == 0 and os.path.exists(CONF), r.stdout + r.stderr
    conf = open(CONF, encoding="utf-8-sig").read()
    assert f"tcp:127.0.0.1:{gw.port}" in conf and "node = installpc" in conf and "exec = " in conf, conf
    assert not os.path.exists(TOKEN)
    print("  ok   安装（离线包，不注册服务）")

    # 手动启动安装包里生成的 run-kernel.ps1；内核请求配对，假网关发口令
    runner = subprocess.Popen([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", RUNNER_W],
                              cwd=WIN_CWD, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    assert wait_file(TOKEN, 30), "没有拿到 paired.token"
    assert gw.paired.wait(10) and gw.accepted.wait(10), "假网关没有收到配对和重连"
    print("  ok   启动后配对并重新连上网关")

    # 网关推更新（同样的程序、同一个版本）：内核把正在运行的 exe 改名、换上新的，以 75 退出，
    # run-kernel.ps1 立即重新拉起；新进程被网关接受后确认，update-pending.json 删掉
    import base64
    ver = gw.hellos[-1].get("version")
    assert ver, gw.hellos[-1]
    files = []
    for n in ("nano-kernel.exe", "nano-exec.exe", "rg.exe"):
        with open(os.path.join(PKG_L, n), "rb") as f:
            files.append({"path": n, "data": base64.b64encode(f.read()).decode()})
    seen = len(gw.hellos)
    gw.update_msg = {"t": "update", "version": ver, "files": files}
    end = time.time() + 60
    while time.time() < end and not (gw.update_results and len(gw.hellos) > seen + 1):
        time.sleep(0.3)
    assert gw.update_results and gw.update_results[0].get("ok") is True, gw.update_results
    assert len(gw.hellos) > seen + 1, "更新后内核没有重新连上（run-kernel.ps1 没把它拉起来？）"
    gap = gw.hello_times[-1] - gw.result_time
    assert gap < 4, f"更新后过了 {gap:.1f} 秒才重新连上：run-kernel.ps1 没认出退出码 75"
    assert os.path.exists(os.path.join(INST_L, "nano-kernel.exe.old")), os.listdir(INST_L)
    end = time.time() + 30
    while time.time() < end and os.path.exists(os.path.join(INST_L, "update-pending.json")):
        time.sleep(0.3)
    assert not os.path.exists(os.path.join(INST_L, "update-pending.json")), "新版本被接受后没有确认"
    print("  ok   网关推来新版本：内核换掉自己、退出码 75、run-kernel.ps1 立即拉起")

    # nano-kernel 管理命令：status、stop（run-kernel.ps1 跟着结束）、start（交给 run-kernel.ps1）
    gw.hold = True
    end = time.time() + 30
    while time.time() < end:
        r = nk("status")
        if r.returncode == 0 and "已连接" in nk_out(r):
            break
        time.sleep(0.5)
    assert r.returncode == 0 and "已连接" in nk_out(r) and "installpc" in nk_out(r), nk_out(r)
    r = stop_r = nk("stop")
    assert r.returncode == 0 and "已停止" in nk_out(r), nk_out(r)
    try:
        assert runner.wait(15) == 0, "run-kernel.ps1 没有在内核正常退出后结束"
    except subprocess.TimeoutExpired:
        logs = ""
        for n in ("kernel.log", "kernel.log.1"):
            if os.path.exists(os.path.join(INST_L, n)):
                logs += f"--- {n}\n" + open(os.path.join(INST_L, n), encoding="utf-8", errors="replace").read()[-1500:]
        tl = subprocess.run([CMD, "/c", "tasklist", "/FI", "IMAGENAME eq nano-kernel.exe"], cwd=WIN_CWD, capture_output=True)
        extra = tl.stdout.decode("gbk", "replace")
        for n in ("kernel.pid", "kernel.status.json", "kernel.stop"):
            pth = os.path.join(INST_L, n)
            extra += f"\n{n}: " + (open(pth, errors="replace").read() if os.path.exists(pth) else "（没有）")
        extra += "\nstop 的输出：" + nk_out(stop_r)
        raise AssertionError("nano-kernel stop 后 run-kernel.ps1 还在拉起内核\n" + logs + extra)
    r = nk("status")
    assert r.returncode == 3 and "没有运行" in nk_out(r), nk_out(r)
    assert not os.path.exists(os.path.join(INST_L, "kernel.stop")), "stop 文件没被处理"
    r = nk("start")
    assert r.returncode == 0 and "已启动" in nk_out(r), nk_out(r)
    r = nk("stop")
    assert r.returncode == 0, nk_out(r)
    gw.hold = False
    print("  ok   nano-kernel status / stop / start")

    old_token = open(TOKEN, encoding="ascii").read().strip()
    cleanup_kernel()
    try:
        runner.wait(5)
    except subprocess.TimeoutExpired:
        runner.kill()

    r = ps(PKG_PS1, "-Gateway", f"tcp:127.0.0.1:{gw.port}", "-Name", "installpc2", "-Yes", "-NoService", "-NoPath", "-Dir", INST_W)
    assert r.returncode == 0, r.stdout + r.stderr
    conf = open(CONF, encoding="utf-8-sig").read()
    assert "node = installpc2" in conf and f"tcp:127.0.0.1:{gw.port}" in conf, conf
    assert os.path.exists(TOKEN) and open(TOKEN, encoding="ascii").read().strip() == old_token, "升级丢了配对口令"
    print("  ok   升级保留配置和 paired.token")

    assert os.path.exists(os.path.join(INST_L, "install-kernel.ps1")), "安装目录里没有留安装脚本"
    r = ps(INST_W + "\\install-kernel.ps1", "-Uninstall", "-Yes", "-NoPath", "-Dir", INST_W)
    assert r.returncode == 0 and not os.path.exists(INST_L), r.stdout + r.stderr
    print("  ok   卸载（用安装目录里留的脚本）")
    passed = 6

    if SERVICE:
        gw.paired.clear()
        gw.accepted.clear()
        r = ps(PKG_PS1, "-Gateway", f"tcp:127.0.0.1:{gw.port}", "-Name", "servicepc", "-Yes", "-Dir", INST_W,
               "-TaskName", TASK)
        assert r.returncode == 0, r.stdout + r.stderr
        assert wait_file(TOKEN, 60), "任务计划程序启动的内核没有拿到 paired.token：" + r.stdout + r.stderr
        assert gw.accepted.wait(30), "任务计划程序启动的内核没有连上假网关"
        q = subprocess.run(["schtasks", "/Query", "/TN", TASK], capture_output=True, text=True, errors="replace")
        assert q.returncode == 0, q.stdout + q.stderr
        upath = subprocess.run([POWERSHELL, "-NoProfile", "-Command", "[Environment]::GetEnvironmentVariable('Path','User')"],
                               capture_output=True, text=True).stdout
        assert INST_W.lower() in upath.lower(), "安装目录没有加进用户 PATH：" + upath
        r = nk("status")
        assert r.returncode == 0 and "Windows 任务计划" in nk_out(r), nk_out(r)
        print("  ok   注册任务计划程序并由它启动内核；加进 PATH；nano-kernel status 认出任务计划")
        r = ps(PKG_PS1, "-Uninstall", "-Yes", "-Dir", INST_W, "-TaskName", TASK)
        assert r.returncode == 0 and not os.path.exists(INST_L), r.stdout + r.stderr
        q = subprocess.run(["schtasks", "/Query", "/TN", TASK], capture_output=True, text=True, errors="replace")
        assert q.returncode != 0, "卸载后任务还在"
        upath = subprocess.run([POWERSHELL, "-NoProfile", "-Command", "[Environment]::GetEnvironmentVariable('Path','User')"],
                               capture_output=True, text=True).stdout
        assert INST_W.lower() not in upath.lower(), "卸载后 PATH 里还有安装目录"
        print("  ok   卸载时删掉任务")
        passed += 2
    print(f"install-win: {passed} passed")
finally:
    if runner:
        try:
            runner.kill()
        except Exception:
            pass
    cleanup_kernel()
    gw.close()
    shutil.rmtree(TMP_L, ignore_errors=True)
