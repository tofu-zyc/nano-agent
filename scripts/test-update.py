#!/usr/bin/env python3
"""更新和卸载的端到端测试：临时 HOME、本地假 Release（NANO_DOWNLOAD_URL）、假 systemctl（走后台进程模式）。

1. 一行安装网关 0.0.1（导入一个网页关闭、mock 模型的配置）。
2. nano-gateway update --check / update：换成 0.0.2；自带的人格改过的放成 .new，没改过的技能直接更新。
3. 0.0.3 是启动必失败的测试构建：update 后自动退回 0.0.2。
4. 内核 0.0.1 连上网关 0.0.2：网关自动推送，内核换成 0.0.2。
5. 网关和内核装在一起：nano-gateway uninstall 只卸网关；nano-kernel uninstall 再卸内核，PATH 行去掉，不留进程。
不会碰真实的 ~/.nano-agent。
"""
import functools
import hashlib
import http.server
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
ARCH = {"x86_64": "x86_64", "amd64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64"}[os.uname().machine]
FAILS = []
TMP = tempfile.mkdtemp(prefix="nano-update-")


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


def out(r):
    return ((r.stdout or b"") + (r.stderr or b"")).decode("utf-8", "replace")


def sha256(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


# ---- 编出三个版本（0.0.3 启动必失败） ----
BUILD = os.path.join(TMP, "build")


def build(ver, fail=False):
    d = os.path.join(BUILD, ver)
    os.makedirs(d)
    ld = f"-X nano-agent/gateway/internal/version.Version={ver}" + (" -X main.testFailStart=1" if fail else "")
    subprocess.run(["go", "build", "-ldflags", ld, "-o", os.path.join(d, "nano-gateway"), "./cmd/nano-gateway"],
                   cwd=os.path.join(ROOT, "gateway"), check=True)
    subprocess.run(["make", "-s", "-C", "kernel", "nano-kernel", "nano-exec", f"VERSION={ver}"], cwd=ROOT, check=True,
                   stdout=subprocess.DEVNULL)
    for b in ("nano-kernel", "nano-exec"):
        shutil.copy2(os.path.join(ROOT, "kernel", b), os.path.join(d, b))
    return d


SITE = os.path.join(TMP, "site")
os.makedirs(os.path.join(SITE, "latest", "download"))


def publish(ver, bins, persona_extra="", skill_extra=""):
    rel = os.path.join(SITE, "download", "v" + ver)
    os.makedirs(rel)
    work = os.path.join(TMP, "pkg-" + ver)
    g = os.path.join(work, f"nano-gateway-{ver}-linux-{ARCH}")
    os.makedirs(os.path.join(g, "lib"))
    shutil.copy2(os.path.join(bins, "nano-gateway"), g)
    for b in ("nano-kernel", "nano-exec"):
        shutil.copy2(os.path.join(bins, b), os.path.join(g, "lib", b))
    shutil.copytree(os.path.join(ROOT, "personas"), os.path.join(g, "personas"))
    shutil.copytree(os.path.join(ROOT, "skills"), os.path.join(g, "skills"))
    if persona_extra:
        with open(os.path.join(g, "personas", "default.md"), "a") as f:
            f.write(persona_extra)
    if skill_extra:
        with open(os.path.join(g, "skills", "model-setup", "SKILL.md"), "a") as f:
            f.write(skill_extra)
    for s in ("install-gateway.sh",):
        shutil.copy2(os.path.join(ROOT, "scripts", s), g)
    shutil.copy2(os.path.join(ROOT, "gateway.example.toml"), g)
    k = os.path.join(work, f"nano-kernel-{ver}-linux-{ARCH}")
    os.makedirs(k)
    for b in ("nano-kernel", "nano-exec"):
        shutil.copy2(os.path.join(bins, b), k)
    shutil.copy2(os.path.join(ROOT, "scripts", "install-kernel.sh"), k)
    assets = []
    for d in (g, k):
        with open(os.path.join(d, "VERSION"), "w") as f:
            f.write(ver + "\n")
        name = os.path.basename(d) + ".tar.gz"
        with tarfile.open(os.path.join(rel, name), "w:gz") as t:
            t.add(d, arcname=os.path.basename(d))
        assets.append(name)
    shutil.copy2(os.path.join(ROOT, "scripts", "install.sh"), rel)
    with open(os.path.join(rel, "VERSION"), "w") as f:
        f.write(ver + "\n")
    with open(os.path.join(rel, "SHA256SUMS"), "w") as f:
        for a in assets + ["install.sh", "VERSION"]:
            f.write(f"{sha256(os.path.join(rel, a))}  {a}\n")


def set_latest(ver):
    with open(os.path.join(SITE, "latest", "download", "VERSION"), "w") as f:
        f.write(ver + "\n")
    shutil.copy2(os.path.join(ROOT, "scripts", "install.sh"), os.path.join(SITE, "latest", "download", "install.sh"))


class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


HOME = os.path.join(TMP, "home")
NANO = os.path.join(HOME, ".nano-agent")
FAKEBIN = os.path.join(TMP, "fakebin")
LOCALBIN = os.path.join(HOME, ".local", "bin")
srv = None


def gw(*args, timeout=180):
    return subprocess.run([os.path.join(NANO, "bin", "nano-gateway"), *args], env=ENV, capture_output=True, timeout=timeout)


def status():
    r = gw("status")
    return out(r) if r.returncode == 0 else ""


def wait(cond, timeout, what):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.5)
    raise AssertionError("等不到：" + what)


def leftover():
    found = []
    for pid in os.listdir("/proc"):
        if pid.isdigit():
            try:
                if os.readlink(f"/proc/{pid}/exe").startswith(TMP):
                    found.append(int(pid))
            except OSError:
                pass
    return found


try:
    b1, b2, b3 = build("0.0.1"), build("0.0.2"), build("0.0.3", fail=True)
    publish("0.0.1", b1)
    publish("0.0.2", b2, persona_extra="\n新版本的人格补充\n", skill_extra="\n新版本的技能说明\n")
    publish("0.0.3", b3)
    set_latest("0.0.1")
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Quiet, directory=SITE))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    os.makedirs(LOCALBIN)
    os.makedirs(FAKEBIN)
    with open(os.path.join(FAKEBIN, "systemctl"), "w") as f:
        f.write("#!/bin/sh\nexit 1\n")
    os.chmod(os.path.join(FAKEBIN, "systemctl"), 0o755)
    ENV = dict(os.environ, HOME=HOME, SHELL="/bin/bash", NANO_DOWNLOAD_URL=f"http://127.0.0.1:{srv.server_address[1]}",
               PATH=FAKEBIN + ":" + LOCALBIN + ":" + os.environ["PATH"])
    ENV.pop("NANO_HOME", None)
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        DEVPORT = s.getsockname()[1]

    @case("一行安装网关 0.0.1 并导入配置")
    def _():
        old = os.path.join(TMP, "old")
        os.makedirs(old)
        with open(os.path.join(old, "gateway.toml"), "w") as f:
            f.write(f'[kernel]\nlisten = "unix:{old}/k.sock"\ntoken = "tok"\ndevice_listen = "127.0.0.1:{DEVPORT}"\n'
                    '[web]\nlisten = "off"\n[llm]\nprovider = "mock"\n[update]\ncheck = false\n')
        exp = os.path.join(TMP, "export.tar.gz")
        r = subprocess.run([os.path.join(b1, "nano-gateway"), "export", "-c", os.path.join(old, "gateway.toml"), "-o", exp],
                           env=dict(ENV, NANO_HOME=old), capture_output=True)
        assert r.returncode == 0, out(r)
        script = open(os.path.join(ROOT, "scripts", "install.sh"), "rb").read()
        r = subprocess.run(["sh", "-s", "--", "--import", exp, "--yes"], input=script, env=ENV, capture_output=True, timeout=300)
        assert r.returncode == 0, out(r)
        wait(lambda: "版本 0.0.1" in status(), 30, "网关 0.0.1 运行")
        assert os.path.exists(os.path.join(NANO, ".shipped.sha256")), "安装时没有记下自带文件的清单"

    @case("update --check 和 update：换成 0.0.2，改过的人格放成 .new")
    def _():
        with open(os.path.join(NANO, "personas", "default.md"), "a") as f:
            f.write("\n我自己加的话\n")
        set_latest("0.0.2")
        r = gw("update", "--check")
        assert r.returncode == 0 and "0.0.2" in out(r) and "可以更新" in out(r), out(r)
        r = gw("update", "--yes")
        assert r.returncode == 0 and "已更新到 0.0.2" in out(r), out(r)
        assert "版本 0.0.2" in status(), status()
        k = subprocess.run([os.path.join(NANO, "lib", "nano-kernel"), "--version"], capture_output=True, text=True).stdout
        assert k.strip() == "0.0.2", k
        p = open(os.path.join(NANO, "personas", "default.md")).read()
        assert "我自己加的话" in p and "新版本的人格补充" not in p, p
        assert "新版本的人格补充" in open(os.path.join(NANO, "personas", "default.md.new")).read()
        assert "新版本的技能说明" in open(os.path.join(NANO, "skills", "model-setup", "SKILL.md")).read()
        assert os.listdir(os.path.join(NANO, "backup")), "没有备份"
        assert not os.path.exists(os.path.join(NANO, "update.json"))

    @case("新版本起不来：自动退回 0.0.2")
    def _():
        set_latest("0.0.3")
        r = gw("update", "--yes", timeout=240)
        assert r.returncode != 0 and "已退回 0.0.2" in out(r), out(r)
        wait(lambda: "版本 0.0.2" in status(), 30, "退回后网关 0.0.2 运行")
        wait(lambda: not os.path.exists(os.path.join(NANO, "update.json")), 15, "update.json 处理掉")
        set_latest("0.0.2")

    @case("内核 0.0.1 连上网关 0.0.2：自动换成 0.0.2")
    def _():
        script = open(os.path.join(ROOT, "scripts", "install.sh"), "rb").read()
        r = subprocess.run(["sh", "-s", "--", "--kernel", "--version", "0.0.1", "--gateway", f"tcp:127.0.0.1:{DEVPORT}",
                            "--name", "kpc", "--yes", "--no-service"], input=script, env=ENV, capture_output=True, timeout=300)
        assert r.returncode == 0, out(r)
        kdir = os.path.join(NANO, "kernel")
        assert os.path.exists(os.path.join(kdir, "install-kernel.sh")), "内核目录里没有留安装脚本"
        with open(os.path.join(kdir, "kernel.conf"), "a") as f:
            f.write("token = tok\n")
        r = subprocess.run([os.path.join(NANO, "bin", "nano-kernel"), "start"], env=ENV, capture_output=True)
        assert r.returncode == 0, out(r)
        ver = lambda: subprocess.run([os.path.join(kdir, "nano-kernel"), "--version"], capture_output=True, text=True).stdout.strip()
        wait(lambda: ver() == "0.0.2", 60, "内核换成 0.0.2：" + open(os.path.join(kdir, "kernel.log")).read()[-800:])
        wait(lambda: "kpc（linux 0.0.2）" in status(), 30, "网关看到内核 0.0.2：" + status())
        r = subprocess.run([os.path.join(NANO, "bin", "nano-kernel"), "version"], env=ENV, capture_output=True, text=True)
        assert r.stdout.strip() == "0.0.2", r.stdout + r.stderr

    @case("网关和内核在一起：nano-gateway uninstall 只卸网关")
    def _():
        r = gw("uninstall", "--yes")
        assert r.returncode == 0, out(r)
        assert os.path.exists(os.path.join(NANO, "kernel", "nano-kernel")), "内核被删了"
        for p in ("gateway.toml", "data", "secret.key", "lib", "bin/nano-gateway"):
            assert not os.path.exists(os.path.join(NANO, p)), p + " 还在"
        assert "# nano-agent PATH" in open(os.path.join(HOME, ".bashrc")).read(), "内核还在，PATH 行不该删"
        time.sleep(1)
        left = [p for p in leftover() if "nano-gateway" in os.readlink(f"/proc/{p}/exe")]
        assert not left, f"网关进程还在：{left}"

    @case("nano-kernel uninstall：卸内核，PATH 行和链接都去掉，不留进程")
    def _():
        r = subprocess.run([os.path.join(NANO, "bin", "nano-kernel"), "uninstall", "--yes"], env=ENV, capture_output=True)
        assert r.returncode == 0, out(r)
        assert not os.path.exists(os.path.join(NANO, "kernel")) and not os.path.exists(os.path.join(NANO, "bin")), \
            os.listdir(NANO) if os.path.exists(NANO) else ""
        for rc in (".profile", ".bashrc"):
            assert "nano-agent" not in open(os.path.join(HOME, rc)).read(), rc
        assert not os.listdir(LOCALBIN), os.listdir(LOCALBIN)
        time.sleep(1)
        assert not leftover(), f"还有进程：{leftover()}"
finally:
    for p in leftover():
        try:
            os.kill(p, signal.SIGKILL)
        except OSError:
            pass
    if srv:
        srv.shutdown()
    shutil.rmtree(TMP, ignore_errors=True)
    subprocess.run(["make", "-s", "-C", "kernel"], cwd=ROOT, stdout=subprocess.DEVNULL)   # 换回平时的版本号

print(f"update: {len(FAILS)} failures")
sys.exit(1 if FAILS else 0)
