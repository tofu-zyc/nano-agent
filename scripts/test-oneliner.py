#!/usr/bin/env python3
"""一行安装测试：本地起一个假的 Release 下载服务（NANO_DOWNLOAD_URL），跑 scripts/install.sh。

覆盖：查最新版本、下载、SHA256 校验（坏包拒绝）、装内核；装网关并导入另一台机器的导出包
（--import / --old-local-as），装好后网关在后台运行，status 能查到，然后卸载。
用 make release VERSION=test 的包改名成 0.0.0；临时 HOME，systemctl 是假的（走后台进程分支），
不会碰真实的 ~/.nano-agent。在 Windows 上（WSL interop 或原生）另外测 install.ps1。
"""
import functools
import hashlib
import http.server
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
VER = "0.0.0"
ARCH = {"x86_64": "x86_64", "amd64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64"}[os.uname().machine]
FAILS = []


def case(name):
    def wrap(fn):
        t0 = time.time()
        try:
            fn()
            print(f"  ok   {name} ({time.time() - t0:.1f}s)")
        except Exception as e:  # noqa: BLE001
            FAILS.append(name)
            print(f"  FAIL {name}：{e}")
        return fn
    return wrap


def sha256(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def repack(src, kind, osname, arch, ext, out_dir):
    """把 VERSION=test 的包改名成 VER 版本"""
    name = f"nano-{kind}-{VER}-{osname}-{arch}"
    work = tempfile.mkdtemp(dir=TMP)
    if ext == "tar.gz":
        with tarfile.open(src) as t:
            t.extractall(work, filter="data")
        old = os.path.join(work, os.listdir(work)[0])
        os.rename(old, os.path.join(work, name))
        with open(os.path.join(work, name, "VERSION"), "w") as f:
            f.write(VER + "\n")
        with tarfile.open(os.path.join(out_dir, name + ".tar.gz"), "w:gz") as t:
            t.add(os.path.join(work, name), arcname=name)
    else:
        import zipfile
        with zipfile.ZipFile(src) as z:
            z.extractall(work)
        old = os.path.join(work, os.listdir(work)[0])
        os.rename(old, os.path.join(work, name))
        with open(os.path.join(work, name, "VERSION"), "w") as f:
            f.write(VER + "\n")
        subprocess.run([sys.executable, os.path.join(ROOT, "scripts", "zipdir.py"), os.path.join(work, name),
                        os.path.join(out_dir, name + ".zip")], check=True)
    return name


class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


if not os.path.exists(os.path.join(ROOT, f"dist/nano-gateway-test-linux-{ARCH}.tar.gz")):
    subprocess.run(["make", "release", "VERSION=test"], cwd=ROOT, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

TMP = tempfile.mkdtemp(prefix="nano-oneliner-")
SITE = os.path.join(TMP, "site")
REL = os.path.join(SITE, "download", "v" + VER)
os.makedirs(REL)
os.makedirs(os.path.join(SITE, "latest", "download"))
assets = []
for kind in ("kernel", "gateway"):
    assets.append(repack(os.path.join(ROOT, f"dist/nano-{kind}-test-linux-{ARCH}.tar.gz"), kind, "linux", ARCH,
                         "tar.gz", REL) + ".tar.gz")
WIN_ZIP = os.path.join(ROOT, "dist/nano-kernel-test-windows-x86_64.zip")
if os.path.exists(WIN_ZIP):
    assets.append(repack(WIN_ZIP, "kernel", "windows", "x86_64", "zip", REL) + ".zip")
for s in ("install.sh", "install.ps1"):
    shutil.copy2(os.path.join(ROOT, "scripts", s), os.path.join(REL, s))
with open(os.path.join(REL, "VERSION"), "w") as f:
    f.write(VER + "\n")
shutil.copy2(os.path.join(REL, "VERSION"), os.path.join(SITE, "latest", "download", "VERSION"))
with open(os.path.join(REL, "SHA256SUMS"), "w") as f:
    for a in assets + ["install.sh", "install.ps1", "VERSION"]:
        f.write(f"{sha256(os.path.join(REL, a))}  {a}\n")

srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Quiet, directory=SITE))
threading.Thread(target=srv.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{srv.server_address[1]}"

HOME = os.path.join(TMP, "home")
FAKEBIN = os.path.join(TMP, "fakebin")
os.makedirs(HOME)
os.makedirs(FAKEBIN)
with open(os.path.join(FAKEBIN, "systemctl"), "w") as f:
    f.write("#!/bin/sh\nexit 1\n")
os.chmod(os.path.join(FAKEBIN, "systemctl"), 0o755)
LOCALBIN = os.path.join(HOME, ".local", "bin")
os.makedirs(LOCALBIN)
ENV = dict(os.environ, HOME=HOME, PATH=FAKEBIN + ":" + LOCALBIN + ":" + os.environ["PATH"], NANO_DOWNLOAD_URL=BASE,
           SHELL="/bin/bash")
ENV.pop("NANO_HOME", None)
NANO = os.path.join(HOME, ".nano-agent")


def oneliner(*args, env=ENV):
    # 和 curl … | sh -s -- 一样：脚本从标准输入读
    script = open(os.path.join(ROOT, "scripts", "install.sh"), "rb").read()
    return subprocess.run(["sh", "-s", "--", *args], input=script, env=env, capture_output=True, timeout=300)


def out(r):
    return (r.stdout + r.stderr).decode("utf-8", "replace")


try:
    @case("一行安装内核：查最新版本、下载、校验、安装")
    def _():
        r = oneliner("--kernel", "--gateway", "unix:" + os.path.join(TMP, "none.sock"), "--name", "onepc",
                     "--yes", "--no-service")
        assert r.returncode == 0, out(r)
        kdir = os.path.join(NANO, "kernel")
        assert os.path.exists(os.path.join(kdir, "nano-kernel")) and "校验通过" in out(r), out(r)
        conf = open(os.path.join(kdir, "kernel.conf")).read()
        assert "node = onepc" in conf, conf
        r = subprocess.run([os.path.join(kdir, "install-kernel.sh") if os.path.exists(os.path.join(kdir, "install-kernel.sh"))
                            else os.path.join(ROOT, "scripts", "install-kernel.sh"), "--uninstall", "--yes"],
                           env=ENV, capture_output=True)
        assert r.returncode == 0 and not os.path.exists(kdir), out(r)

    @case("一行安装：包被改过时校验失败，不安装")
    def _():
        bad = os.path.join(REL, assets[0])
        good = open(bad, "rb").read()
        try:
            with open(bad, "ab") as f:
                f.write(b"tampered")
            r = oneliner("--kernel", "--gateway", "unix:/x", "--yes", "--no-service")
            assert r.returncode != 0 and "校验失败" in out(r), out(r)
            assert not os.path.exists(os.path.join(NANO, "kernel")), "校验失败还装了"
        finally:
            with open(bad, "wb") as f:
                f.write(good)

    @case("一行安装网关并导入另一台机器的导出包")
    def _():
        # 另一台机器：源码布局的网关配置，导出
        old = os.path.join(TMP, "oldpc")
        os.makedirs(os.path.join(old, "data"))
        with open(os.path.join(old, "gateway.toml"), "w") as f:
            f.write('[kernel]\nlisten = "unix:' + os.path.join(old, "k.sock") + '"\ntoken = "tok"\nlocal = false\n'
                    '[web]\nlisten = "off"\n[llm]\nprovider = "mock"\n')
        with open(os.path.join(old, "data", "workspaces.json"), "w") as f:
            json.dump([{"id": "w1", "host": "本机", "path": "/mnt/d/proj"}], f)
        gw_bin = os.path.join(ROOT, "gateway", "nano-gateway")
        exp = os.path.join(TMP, "export.tar.gz")
        r = subprocess.run([gw_bin, "export", "-c", os.path.join(old, "gateway.toml"), "-o", exp],
                           env=dict(ENV, NANO_HOME=old), capture_output=True)
        assert r.returncode == 0, out(r)

        r = oneliner("--import", exp, "--old-local-as", "oldwsl", "--yes")
        assert r.returncode == 0, out(r)
        assert "已导入" in out(r) and "oldwsl" in out(r), out(r)
        # 命令加进 PATH：shell 启动文件里有，~/.local/bin 在 PATH 里时还有链接（当前终端马上能用）
        for rc in (".profile", ".bashrc"):
            text = open(os.path.join(HOME, rc)).read()
            assert text.count("# nano-agent PATH") == 1 and os.path.join(NANO, "bin") in text, (rc, text)
        assert os.path.realpath(os.path.join(LOCALBIN, "nano-gateway")) == os.path.join(NANO, "bin", "nano-gateway")
        ws = json.load(open(os.path.join(NANO, "data", "workspaces.json")))
        assert ws[0]["host"] == "oldwsl", ws
        gw = os.path.join(NANO, "bin", "nano-gateway")
        end = time.time() + 20
        while True:
            st = subprocess.run([gw, "status"], env=ENV, capture_output=True)
            if st.returncode == 0 or time.time() > end:
                break
            time.sleep(0.5)
        assert st.returncode == 0 and "运行中" in out(st) and "oldwsl" in out(st), out(st)

    @case("网关卸载干净")
    def _():
        script = os.path.join(TMP, "gw-uninstall")
        with tarfile.open(os.path.join(REL, f"nano-gateway-{VER}-linux-{ARCH}.tar.gz")) as t:
            t.extractall(script, filter="data")
        r = subprocess.run(["bash", os.path.join(script, f"nano-gateway-{VER}-linux-{ARCH}", "install-gateway.sh"),
                            "--uninstall", "--yes"], env=ENV, capture_output=True)
        assert r.returncode == 0 and not os.path.exists(NANO), out(r)
        for rc in (".profile", ".bashrc"):
            assert "nano-agent" not in open(os.path.join(HOME, rc)).read(), rc
        assert not os.path.lexists(os.path.join(LOCALBIN, "nano-gateway"))

    if os.path.exists(WIN_ZIP) and (os.name == "nt" or os.path.exists("/mnt/c/Windows/System32/cmd.exe")):
        @case("Windows 一行安装（install.ps1）：下载、校验、安装、卸载")
        def _():
            ps = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
            cmd = "/mnt/c/Windows/System32/cmd.exe"
            temp = subprocess.run([cmd, "/c", "echo %TEMP%"], cwd="/mnt/c", capture_output=True, text=True)
            temp_w = temp.stdout.strip().splitlines()[-1].strip()
            tmp_l = tempfile.mkdtemp(prefix="nano-oneliner-win-",
                                     dir=subprocess.check_output(["wslpath", "-u", temp_w], text=True).strip())
            tmp_w = subprocess.check_output(["wslpath", "-w", tmp_l], text=True).strip()
            try:
                inst = tmp_w + "\\kernel"
                boot = (f"$env:NANO_DOWNLOAD_URL='{BASE}'; "
                        f"& ([scriptblock]::Create((irm {BASE}/download/v{VER}/install.ps1))) "
                        f"-Gateway tcp:127.0.0.1:9 -Name winone -Dir '{inst}' -Yes -NoService")
                r = subprocess.run([ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", boot],
                                   cwd="/mnt/c", capture_output=True, timeout=300)
                assert r.returncode == 0 and "SHA256 OK" in out(r), out(r)
                conf = open(os.path.join(tmp_l, "kernel", "kernel.conf"), encoding="utf-8-sig").read()
                assert "node = winone" in conf, conf
                boot = (f"$env:NANO_DOWNLOAD_URL='{BASE}'; "
                        f"& ([scriptblock]::Create((irm {BASE}/download/v{VER}/install.ps1))) -Dir '{inst}' -Yes -Uninstall")
                r = subprocess.run([ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", boot],
                                   cwd="/mnt/c", capture_output=True, timeout=300)
                assert r.returncode == 0 and not os.path.exists(os.path.join(tmp_l, "kernel")), out(r)
            finally:
                shutil.rmtree(tmp_l, ignore_errors=True)
finally:
    gw = os.path.join(NANO, "bin", "nano-gateway")
    if os.path.exists(gw):
        subprocess.run([gw, "stop"], env=ENV, capture_output=True)
    srv.shutdown()
    shutil.rmtree(TMP, ignore_errors=True)

print(f"oneliner: {len(FAILS)} failures")
sys.exit(1 if FAILS else 0)
