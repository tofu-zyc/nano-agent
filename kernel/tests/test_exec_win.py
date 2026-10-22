#!/usr/bin/env python3
"""驱动 Windows 版 nano-exec.exe：在 WSL 里通过 interop，或者直接在 Windows 上（CI）。

用法：python3 tests/test_exec_win.py build/nano-exec.exe [rg.exe]
测试目录放在 Windows 的临时目录下（WSL 里用 wslpath -w 转换），USERPROFILE 也指向临时目录，
不碰用户自己的文件。需要 Windows 的 powershell.exe。
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

EXEC_SRC = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else "./build/nano-exec.exe")
RG_SRC = os.path.abspath(sys.argv[2] if len(sys.argv) > 2 else "../build/tools/rg.exe")
NATIVE = os.name == "nt"   # 直接在 Windows 上跑（GitHub Actions 的 Windows runner）
if NATIVE:
    CMD = os.environ.get("ComSpec", r"C:\Windows\System32\cmd.exe")
    TASKLIST = "tasklist.exe"
    WIN_CWD = None
else:
    CMD = "/mnt/c/Windows/System32/cmd.exe"
    TASKLIST = "/mnt/c/Windows/System32/tasklist.exe"
    WIN_CWD = "/mnt/c"
FAILS = []


def wslpath(flag, p):
    if NATIVE:
        return os.path.abspath(p)
    return subprocess.check_output(["wslpath", flag, p], text=True).strip()


def windows_temp():
    if NATIVE:
        return os.path.abspath(tempfile.gettempdir())
    out = subprocess.run([CMD, "/c", "echo %TEMP%"], cwd=WIN_CWD, capture_output=True, text=True)
    if out.returncode != 0 or not out.stdout.strip():
        raise RuntimeError("拿不到 Windows 的 TEMP：" + out.stderr)
    return out.stdout.strip().splitlines()[-1].strip()


TEMP_WIN = windows_temp()
TEMP_LINUX = wslpath("-u", TEMP_WIN)
ROOT_LINUX = tempfile.mkdtemp(prefix="nano-exec-win-", dir=TEMP_LINUX)
ROOT_WIN = wslpath("-w", ROOT_LINUX)
EXEC_LINUX = os.path.join(ROOT_LINUX, "nano-exec.exe")
EXEC_WIN = ROOT_WIN + "\\nano-exec.exe"
HOME_LINUX = os.path.join(ROOT_LINUX, "home")
HOME_WIN = ROOT_WIN + "\\home"
WORK_LINUX = os.path.join(ROOT_LINUX, "工作 目录")
WORK_WIN = ROOT_WIN + "\\工作 目录"
OTHER_LINUX = os.path.join(ROOT_LINUX, "other")
OTHER_WIN = ROOT_WIN + "\\other"
PATH_WIN = ";".join([
    EXEC_WIN.rsplit("\\", 1)[0],
    r"C:\Windows\System32",
    r"C:\Windows",
    r"C:\Windows\System32\Wbem",
    r"C:\Windows\System32\WindowsPowerShell\v1.0",
    r"C:\Windows\System32\OpenSSH",
])
ENV = {
    "USERPROFILE": HOME_WIN,
    "HOME": HOME_WIN,
    "TEMP": TEMP_WIN,
    "TMP": TEMP_WIN,
    "PATH": PATH_WIN,
    "SystemRoot": r"C:\Windows",
    "windir": r"C:\Windows",
    "ComSpec": r"C:\Windows\System32\cmd.exe",
    "PATHEXT": ".COM;.EXE;.BAT;.CMD",
    "LANG": "C.UTF-8",
    "SECRET_TOKEN": "sk-should-not-leak",
}


def q(s):
    return subprocess.list2cmdline([s])


RUN_CMD_LINUX = os.path.join(ROOT_LINUX, "run.cmd")
RUN_CMD_WIN = ROOT_WIN + "\\run.cmd"


def write_run_cmd():
    lines = ["@echo off", "cd /d " + q(ROOT_WIN)]
    for k, v in ENV.items():
        lines.append(f'set "{k}={v}"')
    lines.append(q(EXEC_WIN) + " %*")
    with open(RUN_CMD_LINUX, "w", encoding="ascii", newline="\r\n") as f:
        f.write("\n".join(lines) + "\n")


def win_cmd(extra=()):
    """WSL interop 会把 USERPROFILE 换回 Windows 用户目录，所以经 cmd 批处理显式设成临时目录。"""
    return [CMD, "/d", "/c", RUN_CMD_WIN] + list(extra)



def setup():
    shutil.copy2(EXEC_SRC, EXEC_LINUX)
    if os.path.exists(RG_SRC):
        shutil.copy2(RG_SRC, os.path.join(ROOT_LINUX, "rg.exe"))
    write_run_cmd()
    os.makedirs(os.path.join(WORK_LINUX, "sub"))
    os.makedirs(OTHER_LINUX)
    os.makedirs(HOME_LINUX)
    with open(os.path.join(OTHER_LINUX, "secret.txt"), "w", encoding="utf-8", newline="") as f:
        f.write("top secret\r\n")
    with open(os.path.join(HOME_LINUX, ".secret"), "w", encoding="utf-8") as f:
        f.write("home secret\n")
    with open(os.path.join(WORK_LINUX, "中文.txt"), "w", encoding="utf-8", newline="") as f:
        f.write("第一行\n第二行\n")
    with open(os.path.join(WORK_LINUX, "sub", "hello.md"), "w", encoding="utf-8") as f:
        f.write("Hello world\n中文 hello\n")
    with open(os.path.join(WORK_LINUX, "crlf.txt"), "w", encoding="utf-8", newline="") as f:
        f.write("a\r\nb\r\n")


def run(tool, args=None, dirs=(WORK_WIN,), write=True, ro=(), rw=(), home=False,
        max_output=65536, timeout=40):
    req = {"tool": tool, "args": args if args is not None else {}, "dirs": list(dirs),
           "write": write, "ro": list(ro), "rw": list(rw), "home": home, "tmp_key": "test",
           "max_output": max_output, "watch_stdin": True}
    p = subprocess.Popen(win_cmd(), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.PIPE, cwd=WIN_CWD)
    p.stdin.write((json.dumps(req, ensure_ascii=False) + "\n").encode("utf-8"))
    p.stdin.flush()
    out = p.stdout.readline()
    p.stdin.close()
    p.wait(timeout)
    err = p.stderr.read().decode("utf-8", "replace")
    assert "Sanitizer" not in err, err
    assert out, f"没有输出，stderr：{err}"
    text = out.decode("utf-8", "replace")
    try:
        return json.loads(text)
    except Exception as e:  # noqa: BLE001
        raise AssertionError(f"执行器输出不是 JSON（{e}）：{text[:500]!r}，stderr：{err[:500]!r}")


def ok(res, *needles):
    assert res["ok"] is True, res
    for n in needles:
        assert n in res["content"], f"结果里没有 {n!r}：{res['content'][:800]}"
    return res["content"]


def bad(res, *needles):
    assert res["ok"] is False, res
    for n in needles:
        assert n in res["content"], f"结果里没有 {n!r}：{res['content'][:800]}"
    return res["content"]


def pid_alive(pid):
    out = subprocess.run([TASKLIST, "/FI", f"PID eq {pid}", "/NH"], cwd=WIN_CWD, capture_output=True)
    return str(pid).encode() in out.stdout


def case(name):
    def wrap(fn):
        def go():
            t0 = time.time()
            try:
                fn()
                print(f"  ok   {name} ({time.time() - t0:.1f}s)")
            except Exception as e:  # noqa: BLE001
                FAILS.append(name)
                print(f"  FAIL {name}: {e}")
        return go
    return wrap


PROBE = None


@case("probe：报告 windows / arch / 无沙箱 / rg")
def t_probe():
    global PROBE
    p = subprocess.run(win_cmd(["--probe"]), capture_output=True, text=True, cwd=WIN_CWD)
    PROBE = json.loads(p.stdout)
    assert PROBE["os"] == "windows", PROBE
    assert PROBE["landlock"] == 0, PROBE
    assert PROBE["arch"] in ("x86_64", "arm64"), PROBE
    assert PROBE["rg"] is True, PROBE


@case("Read：中文路径、中文内容、行号")
def t_read():
    c = ok(run("Read", {"path": "中文.txt"}), "     1\t第一行", "     2\t第二行")
    ok(run("Read", {"path": os.path.join(WORK_WIN, "中文.txt")}), "第一行")
    ok(run("Read", {"path": "sub"}), "hello.md", "是目录")


@case("Write：新建用 LF，覆盖保留 CRLF")
def t_write():
    ok(run("Write", {"path": "new/deep/新建.txt", "content": "你好\n世界\n"}), "已创建")
    with open(os.path.join(WORK_LINUX, "new", "deep", "新建.txt"), "rb") as f:
        assert f.read() == "你好\n世界\n".encode(), "新文件应该用 LF"
    ok(run("Write", {"path": "crlf.txt", "content": "x\ny\n"}), "已覆盖")
    with open(os.path.join(WORK_LINUX, "crlf.txt"), "rb") as f:
        assert f.read() == b"x\r\ny\r\n", "覆盖时应沿用 CRLF"


@case("Edit：CRLF 文件自动匹配，保留 CRLF")
def t_edit_crlf():
    c = ok(run("Edit", {"path": "crlf.txt", "old_string": "x\ny", "new_string": "X\nY"}), "替换了 1 处")
    with open(os.path.join(WORK_LINUX, "crlf.txt"), "rb") as f:
        data = f.read()
    assert data == b"X\r\nY\r\n", data
    assert "X" in c


@case("Read / Write：工作目录外一律拒绝（..、绝对路径、~）")
def t_escape():
    bad(run("Read", {"path": "../other/secret.txt"}), "不在工作目录内")
    bad(run("Read", {"path": os.path.join(OTHER_WIN, "secret.txt")}), "不在工作目录内")
    bad(run("Read", {"path": "~/.secret"}), "不在工作目录内")
    bad(run("Write", {"path": os.path.join(OTHER_WIN, "x.txt"), "content": "x"}), "不在工作目录内")
    assert not os.path.exists(os.path.join(OTHER_LINUX, "x.txt"))


@case("Read：ro=/ 可以读所有盘，写仍只限工作目录")
def t_read_all():
    ok(run("Read", {"path": os.path.join(OTHER_WIN, "secret.txt")}, ro=["/"]), "top secret")
    ok(run("Read", {"path": "~/.secret"}, ro=["/"]), "home secret")
    bad(run("Write", {"path": os.path.join(OTHER_WIN, "x.txt"), "content": "x"}, ro=["/"]), "不在工作目录内")
    assert not os.path.exists(os.path.join(OTHER_LINUX, "x.txt"))


@case("Shell：PowerShell 中文输出、退出码、工作目录")
def t_shell():
    c = ok(run("Shell", {"command": "Write-Output '中文输出'; (Get-Location).Path"}), "中文输出", WORK_WIN)
    bad(run("Shell", {"command": "Write-Output boom; exit 3"}), "boom", "退出码 3")


@case("Shell：环境变量里的密钥不会带进来")
def t_env():
    c = ok(run("Shell", {"command": "$env:TEMP; $env:USERPROFILE; Get-ChildItem Env: | Out-String"}),
           "TEMP")
    assert "sk-should-not-leak" not in c and "SECRET_TOKEN" not in c, c
    assert HOME_WIN.lower() in c.lower(), c


@case("Shell：超时连同子进程一起杀掉")
def t_timeout():
    marker = os.path.join(HOME_LINUX, "nano-child.pid")
    if os.path.exists(marker):
        os.unlink(marker)
    cmd = ("$p = Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60' "
           "-PassThru -WindowStyle Hidden; Set-Content -Encoding ASCII -Path \"$env:USERPROFILE\\nano-child.pid\" -Value $p.Id; "
           "Start-Sleep -Seconds 60")
    t0 = time.time()
    bad(run("Shell", {"command": cmd, "timeout": 2}), "执行超时（2 秒）")
    assert time.time() - t0 < 8, time.time() - t0
    time.sleep(0.5)
    assert os.path.exists(marker), "子进程 PID 没写出来"
    raw = open(marker, "rb").read()
    print("    marker:", raw[:80])
    pid = raw.decode("ascii", "ignore").strip()
    assert pid.isdigit(), f"PID 文件内容不对：{raw[:80]!r}"
    assert not pid_alive(pid), f"子进程 {pid} 还在"


@case("取消：stdin 关闭时杀掉命令并退出")
def t_cancel():
    req = {"tool": "Shell", "args": {"command": "Start-Sleep -Seconds 30"}, "dirs": [WORK_WIN],
           "write": True, "tmp_key": "test", "watch_stdin": True}
    p = subprocess.Popen(win_cmd(), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.PIPE, cwd=WIN_CWD)
    p.stdin.write((json.dumps(req) + "\n").encode())
    p.stdin.flush()
    time.sleep(0.8)
    t0 = time.time()
    p.stdin.close()
    p.wait(5)
    assert time.time() - t0 < 3, time.time() - t0
    assert p.stdout.read() == b""


@case("Grep：自带 rg、中文、glob、files_only")
def t_grep():
    ok(run("Grep", {"pattern": "第二行"}), "中文.txt:2:")
    ok(run("Grep", {"pattern": "中文"}), "hello.md:2:")
    ok(run("Grep", {"pattern": "HELLO", "ignore_case": True, "path": "sub"}), "hello.md:1:")
    c = ok(run("Grep", {"pattern": "hello", "files_only": True}), "hello.md")
    assert ":1:" not in c
    ok(run("Grep", {"glob": "*.md"}), "hello.md")
    ok(run("Grep", {"pattern": "zzz-no-such"}), "没有找到匹配")


@case("--ls：根目录盘符、C:\\、中文目录")
def t_ls():
    p = subprocess.run(win_cmd(["--ls", "/"]), capture_output=True, text=True, cwd=WIN_CWD)
    roots = json.loads(p.stdout)
    assert roots["ok"] and roots["path"] == "", roots
    names = [e["name"] for e in roots["entries"]]
    assert any(n.startswith("C:") for n in names), names
    p = subprocess.run(win_cmd(["--ls", "C:\\"]), capture_output=True, text=True, cwd=WIN_CWD)
    croot = json.loads(p.stdout)
    assert croot["ok"] and croot["path"] == "C:\\", croot
    p = subprocess.run(win_cmd(["--ls", WORK_WIN]), capture_output=True, text=True, cwd=WIN_CWD)
    w = json.loads(p.stdout)
    assert w["ok"] and w["path"] == WORK_WIN, (w, WORK_WIN)
    assert any(e["name"] == "sub" for e in w["entries"]), w


@case("请求错误：未知工具、参数不是对象、工作目录不存在")
def t_bad_request():
    bad(run("Nope"), "未知工具")
    bad(run("Read", args=[1]), "参数应为 JSON 对象")
    bad(run("Shell", {"command": "pwd"}, dirs=()), "还没有设置工作目录")
    bad(run("Read", {"path": "a.txt"}, dirs=[os.path.join(ROOT_WIN, "missing")]), "工作目录不存在")


def main():
    try:
        setup()
        print(f"windows temp：{ROOT_WIN}")
        for t in (t_probe, t_read, t_write, t_edit_crlf, t_escape, t_read_all, t_shell, t_env,
                  t_timeout, t_cancel, t_grep, t_ls, t_bad_request):
            t()
        print(f"exec-win: {len(FAILS)} failures")
        return 1 if FAILS else 0
    finally:
        shutil.rmtree(ROOT_LINUX, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
