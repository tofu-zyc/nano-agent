#!/usr/bin/env python3
"""Windows 内核端到端测试：WSL 里的真实网关 + 网关自己启动的本机内核 + Windows 内核 winpc。

网关监听 TCP，Windows 内核从 127.0.0.1 连进来；它必须被当成另一台电脑（不是本机）。
用命令行聊天（mock 模型）和 !call 直接调工具，覆盖工作目录规范化、远程执行、
Windows 上自己的执行器、PowerShell、CRLF Edit、Grep、技能文件。
"""
import json
import glob
import os
import queue
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
TASKKILL = "/mnt/c/Windows/System32/taskkill.exe"


def wslpath(flag, p):
    return subprocess.check_output(["wslpath", flag, p], text=True).strip()


def win_temp():
    out = subprocess.run([CMD, "/c", "echo %TEMP%"], cwd="/mnt/c", capture_output=True, text=True)
    return out.stdout.strip().splitlines()[-1].strip()


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


TMP = tempfile.mkdtemp(prefix="nano-e2e-win-")
WIN_ROOT_L = tempfile.mkdtemp(prefix="nano-e2e-win-", dir=wslpath("-u", win_temp()))
WIN_ROOT = wslpath("-w", WIN_ROOT_L)
WIN_WORK_L = os.path.join(WIN_ROOT_L, "工作 目录")
WIN_WORK = WIN_ROOT + "\\工作 目录"
WIN_HOME_L = os.path.join(WIN_ROOT_L, "home")
WIN_HOME = WIN_ROOT + "\\home"
WIN_NANO_HOME = WIN_HOME + "\\自定义 空间-\U0001f680"
PORT = free_port()

for name in ("nano-kernel.exe", "nano-exec.exe"):
    shutil.copy2(os.path.join(ROOT, "kernel/build", name), os.path.join(WIN_ROOT_L, name))
shutil.copy2(os.path.join(ROOT, "build/tools/rg.exe"), os.path.join(WIN_ROOT_L, "rg.exe"))
for d in (WIN_WORK_L, WIN_HOME_L, os.path.join(TMP, "work"), os.path.join(TMP, "home"),
          os.path.join(TMP, "data"), os.path.join(TMP, "run"), os.path.join(TMP, "skills/demo"),
          os.path.join(TMP, "kernel")):
    os.makedirs(d, exist_ok=True)
shutil.copy2(os.path.join(ROOT, "kernel/nano-exec"), os.path.join(TMP, "kernel/nano-exec"))
shutil.copy2(os.path.join(ROOT, "kernel/nano-kernel"), os.path.join(TMP, "kernel/nano-kernel"))
with open(os.path.join(WIN_WORK_L, "hello.txt"), "w", encoding="utf-8", newline="") as f:
    f.write("win-e2e 你好\n")
with open(os.path.join(WIN_WORK_L, "crlf.txt"), "wb") as f:
    f.write(b"a\r\nb\r\n")
with open(os.path.join(TMP, "work/hello.txt"), "w", encoding="utf-8") as f:
    f.write("linux-e2e\n")
with open(os.path.join(TMP, "skills/demo/SKILL.md"), "w", encoding="utf-8") as f:
    f.write("---\nname: demo\ndescription: Windows 端到端技能\n---\n读取 data.txt。\n")
with open(os.path.join(TMP, "skills/demo/data.txt"), "w", encoding="utf-8") as f:
    f.write("skill-e2e-data\n")

gateway_conf = os.path.join(TMP, "gateway.toml")
with open(gateway_conf, "w", encoding="utf-8") as f:
    f.write(f"""[kernel]
listen = "tcp:127.0.0.1:{PORT}"
token = "e2e"
local = true
[skills]
dir = "{TMP}/skills"
[llm]
provider = "mock"
max_history_tokens = 4000
[persona]
dir = "{TMP}/personas"
[session]
data_dir = "{TMP}/data"
[workspace]
dirs = ["{TMP}/work"]
mode = "auto"
[cli]
enabled = true
[web]
listen = "off"
""")

win_conf = os.path.join(WIN_ROOT_L, "kernel.conf")
with open(win_conf, "w", encoding="utf-8", newline="\n") as f:
    f.write(f"gateway = tcp:127.0.0.1:{PORT}\ntoken = e2e\nnode = winpc\nexec = {WIN_ROOT}\\nano-exec.exe\napproval_timeout = 5\n")

ps1 = os.path.join(WIN_ROOT_L, "run-kernel.ps1")
ps1_win = WIN_ROOT + "\\run-kernel.ps1"
pidfile = os.path.join(WIN_ROOT_L, "kernel.pid")
klog = os.path.join(WIN_ROOT_L, "kernel.log")
with open(ps1, "w", encoding="utf-8-sig", newline="\r\n") as f:
    f.write("$ErrorActionPreference = 'Stop'\n")
    f.write(f"$env:USERPROFILE = '{WIN_HOME}'\n$env:HOME = '{WIN_HOME}'\n")
    f.write(f"$env:NANO_HOME = '{WIN_NANO_HOME}'\n")
    f.write(f"$env:TEMP = '{WIN_ROOT}'\n$env:TMP = '{WIN_ROOT}'\n")
    f.write("$env:PATH = '" + WIN_ROOT + r";C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0" + "'\n")
    f.write("$env:SystemRoot = 'C:\\Windows'\n$env:ComSpec = 'C:\\Windows\\System32\\cmd.exe'\n")
    f.write("$env:PATHEXT = '.COM;.EXE;.BAT;.CMD'\n")
    f.write("$p = Start-Process -FilePath '" + WIN_ROOT + "\\nano-kernel.exe' -ArgumentList @('-c','" +
            WIN_ROOT + "\\kernel.conf') -WindowStyle Hidden -PassThru -RedirectStandardError '" + klog + "'\n")
    f.write("$p.Id | Set-Content -Encoding ASCII -Path '" + pidfile + "'\n")
    f.write("$p.WaitForExit()\n")

clean_ps1 = os.path.join(WIN_ROOT_L, "cleanup-kernel.ps1")
clean_win = WIN_ROOT + "\\cleanup-kernel.ps1"
with open(clean_ps1, "w", encoding="utf-8-sig", newline="\r\n") as f:
    f.write("Get-Process nano-kernel -ErrorAction SilentlyContinue | "
            "Where-Object { $_.Path -eq '" + WIN_ROOT + "\\nano-kernel.exe' } | Stop-Process -Force\n")

env = dict(os.environ, HOME=os.path.join(TMP, "home"), XDG_RUNTIME_DIR=os.path.join(TMP, "run"))
gateway = subprocess.Popen([os.path.join(ROOT, "gateway/nano-gateway"), "-c", gateway_conf],
                           stdin=subprocess.DEVNULL, stdout=open(os.path.join(TMP, "gateway.out"), "w"),
                           stderr=open(os.path.join(TMP, "gateway.log"), "w"), env=env)
wink = subprocess.Popen([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1_win],
                        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, cwd="/mnt/c")

chat = None
lines = queue.Queue()
passed = 0
turn_finished = threading.Event()
turn_finished.set()


def reader(proc):
    for line in proc.stdout:
        if "次模型调用" in line and "用时" in line:
            turn_finished.set()
        lines.put(line.rstrip("\n"))


def expect(desc, want, timeout=15):
    global passed
    end = time.time() + timeout
    seen = []
    while time.time() < end:
        try:
            line = lines.get(timeout=0.2)
        except queue.Empty:
            continue
        seen.append(line)
        if want in line:
            print(f"  ok   {desc}")
            passed += 1
            return
    raise AssertionError(f"{desc}：没等到 {want!r}，最近输出：{seen[-8:]}")


def send(s):
    # 工具进度出现不等于模型轮次已经完成；本夹具测试顺序操作，
    # 不让下一条 !call 偶然变成插话（插话本身另有专项测试）。
    if not s.startswith(("/approve", "/deny", "/stop")):
        assert turn_finished.wait(20), "上一轮没有正常结束"
    if s.startswith("!call "):
        turn_finished.clear()
    chat.stdin.write(s + "\n")
    chat.stdin.flush()


def wait_gateway():
    log = os.path.join(TMP, "gateway.log")
    end = time.time() + 25
    while time.time() < end:
        try:
            t = open(log, encoding="utf-8", errors="replace").read()
        except FileNotFoundError:
            t = ""
        if t.count("内核已连接") >= 2:
            return
        time.sleep(0.2)
    print("WINK", wink.poll())
    try:
        print("WINKERR", wink.stderr.read(4000))
    except Exception:
        pass
    print("PIDFILE", os.path.exists(pidfile), "KLOG", os.path.exists(klog))
    print("WINLOG", open(klog, encoding="utf-8", errors="replace").read()[-3000:] if os.path.exists(klog) else "(none)")
    raise AssertionError("两个内核没有都连上网关：\n" + open(log, encoding="utf-8", errors="replace").read()[-3000:])


def taskkill(pid):
    subprocess.run([TASKKILL, "/PID", str(pid), "/T", "/F"], cwd="/mnt/c", capture_output=True)


try:
    wait_gateway()
    chat = subprocess.Popen([os.path.join(ROOT, "gateway/nano-gateway"), "chat", "-c", gateway_conf],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True, bufsize=1, env=env)
    threading.Thread(target=reader, args=(chat,), daemon=True).start()
    expect("终端连上网关", "已连上网关")

    # 添加 Windows 工作目录：网关用那台机器 ls 返回的规范路径保存
    send("/dir add winpc:" + WIN_WORK)
    expect("添加 Windows 工作目录", "挂上了 winpc:" + WIN_WORK, 20)
    send("/dir")
    expect("列表里是 Windows 路径", "winpc:" + WIN_WORK)

    # 远程执行：这一轮由本机内核跑，Windows 工作目录交给 winpc 的内核执行（run）
    send("!call Read " + json.dumps({"path": "hello.txt", "workspace": "winpc:" + WIN_WORK}, ensure_ascii=False))
    expect("远程执行 Windows 上的工具", "win-e2e 你好", 20)

    # 只挂 Windows 目录：这一轮由 winpc 自己跑，工具在本机执行
    send("/session new win")
    expect("新建 Windows 会话", "已切换到会话")
    send("/dir none")
    expect("取下默认目录", "工作目录全部取下了")
    send("/dir 2")
    expect("挂上 Windows 目录", "挂上了 winpc:" + WIN_WORK)
    send("!call Read " + json.dumps({"path": "hello.txt"}, ensure_ascii=False))
    expect("Windows 内核自己执行 Read", "win-e2e 你好", 20)
    send('!call Shell {"command":"Write-Output \'powershell-e2e\'; (Get-Location).Path"}')
    expect("没有沙箱时 Shell 要审批", "需要审批 #1", 20)
    send("/approve 1")
    expect("Windows 内核跑 PowerShell", "powershell-e2e", 20)
    expect("PowerShell 的工作目录", WIN_WORK, 20)

    # CRLF Edit；路径用 C:/ 写法也要能规范化
    send('!call Edit {"path":"crlf.txt","old_string":"a\\nb","new_string":"A\\nB"}')
    expect("CRLF Edit", "替换了 1 处", 20)
    with open(os.path.join(WIN_WORK_L, "crlf.txt"), "rb") as f:
        data = f.read()
    assert data == b"A\r\nB\r\n", data

    send('!call Write {"path":"' + WIN_WORK.replace("\\", "/") + '/win.txt","content":"win-e2e\\n"}')
    expect("Windows 上用 / 写文件", "已创建", 20)
    send("!call Grep " + json.dumps({"pattern": "win-e2e"}, ensure_ascii=False))
    expect("Windows 上用自带 rg", "win-e2e", 20)

    # 技能文件放到独立、按内容版本分隔的缓存；读取实际路径
    send("!call Skill " + json.dumps({"name": "demo"}, ensure_ascii=False))
    expect("加载技能", "读取 data.txt", 20)
    cache_files = glob.glob(os.path.join(WIN_HOME_L, "自定义 空间-\U0001f680", "skill-cache", "demo-*", "data.txt"))
    assert len(cache_files) == 1, cache_files
    send("!call Read " + json.dumps({"path": wslpath("-w", cache_files[0])}, ensure_ascii=False))
    expect("技能文件放到 Windows", "skill-e2e-data", 20)

    # 工作目录里的文件是网关保存的规范形式（不是网关按 Linux 规则拼的）
    send("/dir")
    expect("规范路径还在列表里", "winpc:" + WIN_WORK)
    print(f"e2e-win: {passed} passed")
finally:
    if chat:
        chat.terminate()
        try:
            chat.wait(3)
        except subprocess.TimeoutExpired:
            chat.kill()
    for p in (wink, gateway):
        p.terminate()
    for p in (wink, gateway):
        try:
            p.wait(5)
        except subprocess.TimeoutExpired:
            p.kill()
    try:
        pid = int(open(pidfile, encoding="ascii").read().strip())
        taskkill(pid)
    except Exception:
        pass
    # 兜底：按精确路径杀掉这个测试目录里的内核（不碰用户自己的内核）
    if os.path.exists(clean_ps1):
        subprocess.run([POWERSHELL, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", clean_win],
                       cwd="/mnt/c", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if os.path.exists(klog):
        err = open(klog, encoding="utf-8", errors="replace").read()
        if "ERROR" in err or "panic" in err:
            print("Windows 内核日志：\n" + err[-2000:])
    shutil.rmtree(TMP, ignore_errors=True)
    shutil.rmtree(WIN_ROOT_L, ignore_errors=True)
