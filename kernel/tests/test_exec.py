#!/usr/bin/env python3
"""直接驱动 nano-exec：五个内置工具的行为，以及沙箱能不能被绕过。

用法：python3 tests/test_exec.py ./nano-exec
"""
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time

EXEC = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else "./nano-exec")
FAILS = []
T = tempfile.mkdtemp(prefix="na-exec-")
HOME = os.path.join(T, "home")
WORK = os.path.join(T, "work")
OTHER = os.path.join(T, "other")
ENV = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": HOME, "LANG": "C.UTF-8",
       "SECRET_TOKEN": "sk-should-not-leak"}
PROBE = json.loads(subprocess.run([EXEC, "--probe"], capture_output=True, text=True, env=ENV).stdout)
SANDBOX = PROBE["landlock"] > 0


def run(tool, args=None, dirs=(WORK,), write=True, ro=(), rw=(), home=False, max_output=65536, timeout=30):
    req = {"tool": tool, "args": args if args is not None else {}, "dirs": list(dirs), "write": write,
           "ro": list(ro), "rw": list(rw), "home": home, "tmp_key": "test", "max_output": max_output, "watch_stdin": True}
    p = subprocess.Popen([EXEC], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
    p.stdin.write((json.dumps(req) + "\n").encode())
    p.stdin.flush()
    out = p.stdout.readline()
    p.stdin.close()
    p.wait(timeout)
    err = p.stderr.read().decode()
    assert "Sanitizer" not in err, err
    assert out, f"没有输出，stderr：{err}"
    return json.loads(out)


def ok(res, *needles):
    assert res["ok"] is True, res
    for n in needles:
        assert n in res["content"], f"结果里没有 {n!r}：{res['content'][:500]}"
    return res["content"]


def bad(res, *needles):
    assert res["ok"] is False, res
    for n in needles:
        assert n in res["content"], f"结果里没有 {n!r}：{res['content'][:500]}"
    return res["content"]


def leftover(tag):
    return subprocess.run(["pgrep", "-f", tag], capture_output=True, text=True).stdout.strip()


def case(name, need_sandbox=False):
    def wrap(fn):
        def go():
            if need_sandbox and not SANDBOX:
                print(f"  skip {name}（本机没有 Landlock）")
                return
            t0 = time.time()
            try:
                fn()
                print(f"  ok   {name} ({time.time() - t0:.1f}s)")
            except Exception as e:  # noqa: BLE001
                FAILS.append(name)
                print(f"  FAIL {name}: {e}")
        return go
    return wrap


# ---- Read ----

@case("Read：行号、offset/limit、超长行")
def t_read():
    c = ok(run("Read", {"path": "lines.txt"}), "     1\tline 1\n", "    50\tline 50\n")
    assert "（共" not in c
    c = ok(run("Read", {"path": "lines.txt", "offset": 10, "limit": 3}), "    10\tline 10", "    12\tline 12",
           "共 50 行，显示了第 10~12 行")
    assert "line 13" not in c
    ok(run("Read", {"path": "long.txt"}), "本行共 5000 字节")
    ok(run("Read", {"path": os.path.join(WORK, "empty.txt")}), "（空文件）")


@case("Read：二进制文件、目录、不存在的文件")
def t_read_misc():
    ok(run("Read", {"path": "bin.dat"}), "二进制文件")
    c = ok(run("Read", {"path": "."}), "是目录", "sub/", "lines.txt")
    assert c.index("bin.dat") < c.index("lines.txt")
    bad(run("Read", {"path": "nope.txt"}), "No such file")


@case("Read：工作目录外的路径一律拒绝（..、绝对路径、~、符号链接）")
def t_read_escape():
    for p in ["../other/secret.txt", os.path.join(OTHER, "secret.txt"), "~/.secret",
              "link_file", "link_dir/secret.txt", "/etc/hostname"]:
        bad(run("Read", {"path": p}), "不在工作目录内")


@case("Read：额外只读路径可以读")
def t_read_ro():
    ok(run("Read", {"path": "~/.gitconfig"}, ro=["~/.gitconfig"]), "name = tester")


@case("Read：ro=/ 时可以读整个文件系统，写仍只限工作目录")
def t_read_all():
    ok(run("Read", {"path": "~/.secret"}, ro=["/"]), "home secret")
    ok(run("Read", {"path": os.path.join(OTHER, "secret.txt")}, ro=["/"]), "top secret")
    # 没有工作目录时，用绝对路径只读
    ok(run("Read", {"path": os.path.join(OTHER, "secret.txt")}, dirs=(), ro=["/"], write=False), "top secret")
    bad(run("Write", {"path": os.path.join(OTHER, "x.txt"), "content": "x"}, ro=["/"]), "不在工作目录内")
    assert not os.path.exists(os.path.join(OTHER, "x.txt"))


# ---- Write / Edit ----

@case("Write：新建（含上级目录）、覆盖保留权限")
def t_write():
    ok(run("Write", {"path": "new/deep/a.txt", "content": "你好\nworld\n"}), "已创建", "2 行")
    assert open(os.path.join(WORK, "new/deep/a.txt")).read() == "你好\nworld\n"
    script = os.path.join(WORK, "run.sh")
    os.chmod(script, 0o755)
    ok(run("Write", {"path": "run.sh", "content": "#!/bin/sh\necho hi\n"}), "已覆盖")
    assert os.stat(script).st_mode & 0o777 == 0o755
    leftovers = [f for f in os.listdir(WORK) if ".nano-" in f]
    assert not leftovers, leftovers


@case("Write：工作目录外、符号链接、只读模式都拒绝")
def t_write_escape():
    bad(run("Write", {"path": "../other/x.txt", "content": "x"}), "不在工作目录内")
    bad(run("Write", {"path": "link_dir/x.txt", "content": "x"}), "不在工作目录内")
    bad(run("Write", {"path": "link_file", "content": "x"}), "不在工作目录内")
    bad(run("Write", {"path": "ro.txt", "content": "x"}, write=False), "只读模式")
    assert not os.path.exists(os.path.join(OTHER, "x.txt"))
    assert open(os.path.join(OTHER, "secret.txt")).read() == "top secret\n"


@case("Edit：唯一替换、找不到、多处匹配、replace_all")
def t_edit():
    path = os.path.join(WORK, "code.py")
    with open(path, "w") as f:
        f.write("def f():\n    return 1\n\ndef g():\n    return 1\n")
    bad(run("Edit", {"path": "code.py", "old_string": "return 1", "new_string": "return 2"}), "出现了 2 次")
    c = ok(run("Edit", {"path": "code.py", "old_string": "def g():\n    return 1",
                        "new_string": "def g():\n    return 3"}), "替换了 1 处", "     5\t    return 3")
    assert open(path).read() == "def f():\n    return 1\n\ndef g():\n    return 3\n", c
    bad(run("Edit", {"path": "code.py", "old_string": "return 9", "new_string": "x"}), "没有找到")
    ok(run("Edit", {"path": "code.py", "old_string": "return", "new_string": "yield", "replace_all": True}),
       "替换了 2 处")
    bad(run("Edit", {"path": "code.py", "old_string": "", "new_string": "x"}), "不能为空")
    bad(run("Edit", {"path": "bin.dat", "old_string": "a", "new_string": "b"}), "二进制")
    bad(run("Edit", {"path": "../other/secret.txt", "old_string": "top", "new_string": "x"}), "不在工作目录内")


# ---- Grep ----

@case("Grep：内容搜索、忽略大小写、只列文件、按 glob 列文件")
def t_grep():
    ok(run("Grep", {"pattern": "line 4[0-2]$"}), "lines.txt:40:line 40", "lines.txt:42:line 42")
    ok(run("Grep", {"pattern": "HELLO", "ignore_case": True, "path": "sub"}), "hello.md:1:")
    c = ok(run("Grep", {"pattern": "world", "files_only": True}), "hello.md")
    assert ":1:" not in c
    c = ok(run("Grep", {"glob": "*.md"}), "hello.md")
    assert "lines.txt" not in c
    ok(run("Grep", {"pattern": "zzz-no-such"}), "没有找到匹配")
    bad(run("Grep", {"pattern": "secret", "path": OTHER}), "不在工作目录内")
    bad(run("Grep", {}), "至少要给一个")


# ---- Shell ----

@case("Shell：输出、退出码、工作目录")
def t_shell():
    ok(run("Shell", {"command": "pwd; echo 中文 >&2"}), WORK, "中文")
    bad(run("Shell", {"command": "echo boom; exit 3"}), "boom", "退出码 3")
    ok(run("Shell", {"command": "true"}), "（无输出）")


@case("Shell：超时连同子进程一起杀掉")
def t_shell_timeout():
    t0 = time.time()
    bad(run("Shell", {"command": "sleep 4242 & wait", "timeout": 1}), "执行超时（1 秒）")
    assert time.time() - t0 < 3, time.time() - t0
    time.sleep(0.2)
    assert not leftover("sleep 4242"), leftover("sleep 4242")


@case("Shell：后台进程在命令结束后被清理")
def t_shell_background():
    t0 = time.time()
    ok(run("Shell", {"command": "sleep 4343 & echo parent done"}), "parent done")
    assert time.time() - t0 < 3.5, time.time() - t0
    time.sleep(0.2)
    assert not leftover("sleep 4343"), leftover("sleep 4343")


@case("Shell：超长输出保留开头和结尾，二进制输出不破坏 JSON")
def t_shell_output():
    c = ok(run("Shell", {"command": "seq 1 100000"}, max_output=4096), "中间省略", "\n100000")
    assert c.startswith("1\n2\n3\n"), c[:40]
    assert len(c.encode()) < 4096 + 200, len(c.encode())
    assert ok(run("Shell", {"command": "printf 'ok\\377\\000end \\344\\270\\255'"})) == "ok��end 中"


@case("取消：stdin 关闭或 SIGTERM 时杀掉命令并退出", need_sandbox=False)
def t_cancel():
    for how in ("stdin", "sigterm"):
        req = {"tool": "Shell", "args": {"command": "sleep 4444"}, "dirs": [WORK], "write": True,
               "tmp_key": "test", "watch_stdin": True}
        p = subprocess.Popen([EXEC], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        p.stdin.write((json.dumps(req) + "\n").encode())
        p.stdin.flush()
        time.sleep(0.5)
        t0 = time.time()
        if how == "stdin":
            p.stdin.close()
        else:
            p.send_signal(signal.SIGTERM)
        p.wait(5)
        assert time.time() - t0 < 1.5, (how, time.time() - t0)
        assert p.stdout.read() == b"", how
        time.sleep(0.2)
        assert not leftover("sleep 4444"), (how, leftover("sleep 4444"))
        if how == "sigterm":
            p.stdin.close()


# ---- 沙箱 ----

@case("沙箱：读不到工作目录外的文件（包括 HOME），写不了 /tmp", need_sandbox=True)
def t_sandbox_fs():
    for cmd in ["cat ~/.secret", "cat ../other/secret.txt", "cat link_file", "ls ~",
                "echo x > ../other/new.txt", "echo x > /tmp/na-exec-escape-test"]:
        c = bad(run("Shell", {"command": cmd}))
        assert "Permission denied" in c or "权限" in c, (cmd, c)
    assert not os.path.exists(os.path.join(OTHER, "new.txt"))
    assert not os.path.exists("/tmp/na-exec-escape-test")
    ok(run("Shell", {"command": "cat /etc/hostname >/dev/null && echo sys-ok"}), "sys-ok")
    ok(run("Shell", {"command": "echo hi > \"$TMPDIR/t\" && cat \"$TMPDIR/t\""}), "hi")
    ok(run("Shell", {"command": "cat ~/.gitconfig"}, ro=["~/.gitconfig"]), "tester")
    c = bad(run("Shell", {"command": "touch x"}, write=False))
    assert "Permission denied" in c or "Read-only" in c, c


@case("沙箱：环境变量里的密钥不会带进来", need_sandbox=False)
def t_sandbox_env():
    c = ok(run("Shell", {"command": "env"}), "TMPDIR=", "HOME=" + HOME)
    assert "sk-should-not-leak" not in c and "SECRET_TOKEN" not in c, c


@case("沙箱：不能连 unix socket（D-Bus、systemd、docker 之类）", need_sandbox=False)
def t_sandbox_unix():
    path = os.path.join(T, "s.sock")
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(path)
    srv.listen(1)
    os.chmod(path, 0o777)
    try:
        code = f"import socket; s = socket.socket(socket.AF_UNIX); s.connect({path!r}); print('CONN' + '-OK')"
        c = bad(run("Shell", {"command": f"python3 -c \"{code}\""}, rw=[T]))
        assert "CONN-OK" not in c and "PermissionError" in c, c
        ok(run("Shell", {"command": "python3 -c \"import socket; a, b = socket.socketpair(); print('pair ok')\""}),
           "pair ok")
    finally:
        srv.close()


@case("沙箱：不能给沙箱外的进程发信号", need_sandbox=True)
def t_sandbox_signal():
    if PROBE["landlock"] < 6:
        print("    （Landlock ABI < 6，跳过）")
        return
    victim = subprocess.Popen(["sleep", "4545"])
    try:
        c = bad(run("Shell", {"command": f"kill -0 {victim.pid}"}))
        assert "not permitted" in c or "权限" in c, c
        assert victim.poll() is None
    finally:
        victim.kill()
        victim.wait()


# ---- 请求本身的问题 ----

@case("请求错误：未知工具、参数不是对象、工作目录没设置或不存在")
def t_request_errors():
    bad(run("Nope"), "未知工具")
    bad(run("Read", args=[1]), "参数应为 JSON 对象")
    bad(run("Shell", {"command": "pwd"}, dirs=()), "还没有设置工作目录")
    bad(run("Read", {"path": "a.txt"}, dirs=()), "还没有设置工作目录")
    bad(run("Read", {"path": "a.txt"}, dirs=[os.path.join(T, "missing")]), "工作目录不存在")


@case("WSL Windows 盘：列目录、读写与目录外隔离", need_sandbox=True)
def t_drvfs():
    drive = next((p for p in ("/mnt/d", "/mnt/c") if os.path.isdir(p) and os.access(p, os.W_OK)), None)
    if drive is None:
        print("       没有可用的 Windows 盘，跳过 DrvFS 实测")
        return
    with tempfile.TemporaryDirectory(prefix="nano-sandbox-", dir=drive) as root:
        work = os.path.join(root, "work")
        os.makedirs(os.path.join(work, "sub"))
        with open(os.path.join(work, "sub", "input.txt"), "w") as f:
            f.write("drvfs-test")
        with open(os.path.join(root, "secret.txt"), "w") as f:
            f.write("outside")
        ok(run("Shell", {"command": "ls -la; ls sub; cat sub/input.txt; echo created > sub/output.txt"}, dirs=[work]), "input.txt", "drvfs-test")
        ok(run("Read", {"path": "sub/output.txt"}, dirs=[work]), "created")
        ok(run("Write", {"path": "write.txt", "content": "written"}, dirs=[work]), "已创建")
        bad(run("Shell", {"command": "cat ../secret.txt"}, dirs=[work]), "Permission denied")
        bad(run("Shell", {"command": "echo denied > write.txt"}, dirs=[work], write=False), "Permission denied")


def setup():
    os.makedirs(os.path.join(WORK, "sub"))
    os.makedirs(OTHER)
    os.makedirs(HOME)
    with open(os.path.join(HOME, ".secret"), "w") as f:
        f.write("home secret\n")
    with open(os.path.join(HOME, ".gitconfig"), "w") as f:
        f.write("[user]\n\tname = tester\n")
    with open(os.path.join(OTHER, "secret.txt"), "w") as f:
        f.write("top secret\n")
    with open(os.path.join(WORK, "lines.txt"), "w") as f:
        f.write("".join(f"line {i}\n" for i in range(1, 51)))
    with open(os.path.join(WORK, "long.txt"), "w") as f:
        f.write("x" * 5000 + "\n")
    open(os.path.join(WORK, "empty.txt"), "w").close()
    with open(os.path.join(WORK, "bin.dat"), "wb") as f:
        f.write(b"\x00\x01binary")
    with open(os.path.join(WORK, "sub", "hello.md"), "w") as f:
        f.write("Hello world\n")
    with open(os.path.join(WORK, "ro.txt"), "w") as f:
        f.write("read only\n")
    with open(os.path.join(WORK, "run.sh"), "w") as f:
        f.write("#!/bin/sh\n")
    os.symlink(os.path.join(OTHER, "secret.txt"), os.path.join(WORK, "link_file"))
    os.symlink(OTHER, os.path.join(WORK, "link_dir"))


@case("提权：rw=/ 能写工作目录外，不带只能读")
def t_elevated_write():
    target = os.path.join(OTHER, "elevated.txt")
    c = bad(run("Write", {"path": target, "content": "x"}, rw=()), "elevated")
    assert not os.path.exists(target)
    ok(run("Write", {"path": target, "content": "x"}, rw=("/",)), "已创建")
    with open(target, encoding="utf-8") as f:
        assert f.read() == "x"
    os.unlink(target)

@case("没有工作目录：Shell 在 HOME，能读，只能写临时目录")
def t_home_mode():
    ok(run("Shell", {"command": "pwd"}, dirs=(), home=True, write=False), HOME)
    ok(run("Shell", {"command": "cat ~/.gitconfig"}, dirs=(), ro=("/",), home=True, write=False), "tester")
    bad(run("Shell", {"command": "echo x > ~/nope"}, dirs=(), home=True, write=False), "Permission denied")
    ok(run("Shell", {"command": "echo hi > \"$TMPDIR/t\" && cat \"$TMPDIR/t\""}, dirs=(), home=True, write=False), "hi")


def main():
    try:
        setup()
        print(f"probe：{PROBE}")
        for t in (t_read, t_read_misc, t_read_escape, t_read_ro, t_read_all, t_write, t_write_escape, t_edit, t_grep,
                  t_shell, t_shell_timeout, t_shell_background, t_shell_output, t_cancel,
                  t_sandbox_fs, t_sandbox_env, t_sandbox_unix, t_sandbox_signal, t_request_errors, t_drvfs, t_elevated_write, t_home_mode):
            t()
        print(f"exec: {len(FAILS)} failures")
        return 1 if FAILS else 0
    finally:
        shutil.rmtree(T, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
