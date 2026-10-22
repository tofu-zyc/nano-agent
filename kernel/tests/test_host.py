#!/usr/bin/env python3
"""用一个假网关驱动真实的 nano-kernel 进程（它再启动真实的 nano-exec），测试宿主层：
协议收发、权限模式、审批、超时、取消、断线重连、进程清理；
以及异机模式：工作目录不在这台机器上时，工具交给网关执行。

用法：python3 tests/test_host.py ./nano-kernel ./nano-exec
"""
import base64
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time

KERNEL = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else "./nano-kernel")
EXEC = os.path.abspath(sys.argv[2] if len(sys.argv) > 2 else "./nano-exec")
FAILS = []
T = tempfile.mkdtemp(prefix="na-host-")
WORK = os.path.join(T, "work")


class Gateway:
    def __init__(self, path):
        self.srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.srv.bind(path)
        self.srv.listen(1)
        self.srv.settimeout(10)
        self.conn = None
        self.buf = b""

    def accept(self):
        self.conn, _ = self.srv.accept()
        self.conn.settimeout(10)
        self.buf = b""

    def send(self, **msg):
        self.conn.sendall(json.dumps(msg, ensure_ascii=False).encode() + b"\n")

    def recv(self, timeout=10):
        deadline = time.time() + timeout
        while b"\n" not in self.buf:
            left = deadline - time.time()
            if left <= 0:
                raise TimeoutError("等待内核消息超时")
            self.conn.settimeout(left)
            chunk = self.conn.recv(65536)
            if not chunk:
                raise ConnectionError("内核关闭了连接")
            self.buf += chunk
        line, self.buf = self.buf.split(b"\n", 1)
        msg = json.loads(line)
        if msg.get("t") == "ping":
            self.send(t="pong")
            return self.recv(timeout)
        return msg

    def expect(self, t, timeout=10, **fields):
        msg = self.recv(timeout)
        ok = msg.get("t") == t and all(
            (v(msg.get(k)) if callable(v) else msg.get(k) == v) for k, v in fields.items())
        if not ok:
            raise AssertionError(f"期望 {t} {fields}，收到 {msg}")
        return msg


def case(name):
    def wrap(fn):
        def run(gw):
            t0 = time.time()
            try:
                fn(gw)
                print(f"  ok   {name} ({time.time() - t0:.1f}s)")
            except Exception as e:  # noqa: BLE001 —— 测试里要把所有失败都记下来
                FAILS.append(name)
                print(f"  FAIL {name}: {e}")
        run.__name__ = fn.__name__
        return run
    return wrap


def local_ws(dirs=(WORK,)):
    return {"name": "本机:" + dirs[0], "dirs": list(dirs), "ro": [], "rw": [], "local": True}


def box_ws(landlock=7):
    return {"name": "box:~/proj", "dirs": ["~/proj"], "ro": [], "rw": [], "local": False, "landlock": landlock}


def start_run(gw, sid, mode="auto", dirs=(WORK,), frm="tg:user", workspaces=None):
    gw.send(t="input", sid=sid, mode=mode, workspaces=workspaces or [local_ws(dirs)], **{"from": frm})
    gw.expect("model", sid=sid)


def call(gw, sid, *calls):
    gw.send(t="turn", sid=sid, reason="tool_use",
            calls=[{"id": c[0], "name": c[1], "args": c[2]} for c in calls])


def finish(gw, sid):
    gw.expect("model", sid=sid)
    gw.send(t="turn", sid=sid, reason="final")
    gw.expect("done", sid=sid, status="ok")


def start_gateway_run(gw, sid, mode="auto", landlock=7):
    start_run(gw, sid, mode=mode, workspaces=[box_ws(landlock)])


def expect_all(gw, n, sid):
    got = {}
    for _ in range(n):
        m = gw.recv()
        assert m.get("sid") == sid, m
        got[(m["t"], m.get("id"))] = m
    return got


def leftover(tag):
    return subprocess.run(["pgrep", "-f", tag], capture_output=True, text=True).stdout.strip()


@case("技能在网关上：Skill 交给网关，不带工作目录")
def t_skill(gw):
    start_run(gw, "sk")
    call(gw, "sk", ("c1", "Skill", {"name": "demo"}))
    m = gw.expect("exec", sid="sk", id="c1", name="Skill")
    assert m["args"] == {"name": "demo"} and "workspace" not in m, m
    gw.send(t="exec_result", sid="sk", id="c1", ok=True, content="技能 demo：照着做就行")
    gw.expect("result", sid="sk", id="c1", ok=True, content="技能 demo：照着做就行")
    finish(gw, "sk")


@case("多个工作目录：按 workspace 参数决定在哪台机器上执行")
def t_multi_ws(gw):
    start_run(gw, "mw", workspaces=[local_ws(), box_ws(landlock=0)])
    call(gw, "mw", ("c1", "Read", {"path": "a.txt"}),
         ("c2", "Write", {"path": "m.txt", "content": "本机", "workspace": "本机:" + WORK}),
         ("c3", "Read", {"path": "r.txt", "workspace": "box"}),
         ("c4", "Read", {"path": "r.txt", "workspace": 2}),
         ("c5", "Read", {"path": "x", "workspace": "nope"}),
         ("c6", "Shell", {"command": "ls", "workspace": "box:~/proj"}))
    got = expect_all(gw, 6, "mw")   # c2 在本机执行，结果也在这些消息里
    assert not got[("result", "c1")]["ok"] and "用 workspace 参数指明" in got[("result", "c1")]["content"], got
    assert "box:~/proj" in got[("result", "c1")]["content"], got
    assert not got[("result", "c5")]["ok"] and "没有工作目录" in got[("result", "c5")]["content"], got
    for c in ("c3", "c4"):
        e = got[("exec", c)]
        assert e["workspace"] == "box:~/proj" and e["args"] == {"path": "r.txt"}, e   # workspace 参数不交给执行器
    assert ("approve_req", "c6") in got, got   # box 没有沙箱：Shell 要审批
    assert got[("result", "c2")]["ok"] and open(os.path.join(WORK, "m.txt")).read() == "本机", got
    gw.send(t="exec_result", sid="mw", id="c3", ok=True, content="r")
    gw.send(t="exec_result", sid="mw", id="c4", ok=True, content="r")
    gw.send(t="approve", sid="mw", id="c6", ok=False, **{"from": "tg:admin"})
    got = expect_all(gw, 3, "mw")
    assert not got[("result", "c6")]["ok"], got
    finish(gw, "mw")


@case("代网关执行：网关请这台机器执行工具、放技能文件")
def t_host_run(gw):
    with open(os.path.join(WORK, "h.txt"), "w") as f:
        f.write("代执行\n")
    gw.send(t="run", id="r1", req={"tool": "Read", "args": {"path": "h.txt"}, "dirs": [WORK], "ro": [], "rw": [],
                                    "write": False, "tmp_key": "s"})
    gw.expect("run_result", id="r1", ok=True, content=lambda c: "代执行" in c)
    gw.send(t="run", id="r2", req={"tool": "Read", "args": {"path": "kernel.conf"}, "dirs": [T], "write": False})
    gw.expect("run_result", id="r2", ok=False, content=lambda c: "包含 nano-agent 自己" in c)
    gw.send(t="run", id="r3", req={"tool": "Shell", "args": {"command": "sleep 4747"}, "dirs": [WORK], "write": True})
    time.sleep(0.4)
    gw.send(t="run_cancel", id="r3")
    time.sleep(0.5)
    assert not leftover("sleep 4747"), leftover("sleep 4747")
    data = base64.b64encode("echo 技能\n".encode()).decode()
    gw.send(t="skill_put", id="p1", skill="demo",
            files=[{"path": "SKILL.md", "data": base64.b64encode(b"# demo").decode()},
                   {"path": "scripts/run.sh", "data": data, "exec": True}])
    r = gw.expect("run_result", id="p1", ok=True)
    d = os.path.join(T, "home", ".nano-agent", "skills", "demo")
    assert r["content"] == d and os.access(os.path.join(d, "scripts", "run.sh"), os.X_OK), r
    gw.send(t="skill_put", id="p2", skill="demo", files=[{"path": "../escape", "data": data}])
    gw.expect("run_result", id="p2", ok=False, content=lambda c: "路径不能用" in c)
    assert os.path.exists(os.path.join(d, "SKILL.md")), "失败时不动原来的文件"
    gw.send(t="skill_put", id="p3", skill="../x", files=[])
    gw.expect("run_result", id="p3", ok=False)


@case("工具往返：Write 写文件，Read 读回来")
def t_roundtrip(gw):
    start_run(gw, "s1")
    call(gw, "s1", ("c1", "Write", {"path": "a.txt", "content": "你好\n"}))
    gw.expect("result", sid="s1", id="c1", ok=True, content=lambda s: "已创建" in s)
    gw.expect("model", sid="s1")
    call(gw, "s1", ("c2", "Read", {"path": "a.txt"}))
    gw.expect("result", sid="s1", id="c2", ok=True, content="     1\t你好\n")
    finish(gw, "s1")


@case("并行调用：全部有结果后才请求模型")
def t_parallel(gw):
    start_run(gw, "s2")
    call(gw, "s2", ("a", "Shell", {"command": "echo A"}), ("b", "Shell", {"command": "echo boom >&2; exit 3"}),
         ("c", "Nope", {}))
    got = {}
    for _ in range(3):
        r = gw.expect("result", sid="s2")
        got[r["id"]] = r
    assert got["a"]["ok"] is True and got["a"]["content"] == "A\n", got["a"]
    assert got["b"]["ok"] is False and "退出码 3" in got["b"]["content"] and "boom" in got["b"]["content"], got["b"]
    assert got["c"]["ok"] is False and "未知工具" in got["c"]["content"]
    finish(gw, "s2")


@case("超时：执行器自己结束命令，连同子进程")
def t_timeout(gw):
    start_run(gw, "s3")
    t0 = time.time()
    call(gw, "s3", ("c1", "Shell", {"command": "sleep 4242 & wait", "timeout": 1}))
    r = gw.expect("result", sid="s3", id="c1", ok=False, timeout=5)
    assert "执行超时（1 秒）" in r["content"], r
    assert time.time() - t0 < 3, time.time() - t0
    time.sleep(0.2)
    assert not leftover("sleep 4242"), leftover("sleep 4242")
    finish(gw, "s3")


@case("超长和二进制输出：截断、替换非法字节，JSON 仍然合法")
def t_output(gw):
    start_run(gw, "s4")
    call(gw, "s4", ("c1", "Shell", {"command": "seq 1 100000"}),
         ("c2", "Shell", {"command": "printf 'ok\\377\\000end \\344\\270\\255'"}))
    got = {}
    for _ in range(2):
        r = gw.expect("result", sid="s4")
        got[r["id"]] = r["content"]
    assert "中间省略" in got["c1"] and got["c1"].endswith("100000\n"), got["c1"][-100:]
    assert len(got["c1"].encode()) < 4096 + 200, len(got["c1"].encode())
    assert got["c2"] == "ok��end 中", repr(got["c2"])
    finish(gw, "s4")


@case("权限模式：none 全拒绝，readonly 只能读")
def t_modes(gw):
    start_run(gw, "s5", mode="none")
    call(gw, "s5", ("c1", "Read", {"path": "a.txt"}))
    gw.expect("result", sid="s5", id="c1", ok=False, content="当前身份没有使用工具的权限")
    finish(gw, "s5")
    start_run(gw, "s5", mode="readonly")
    call(gw, "s5", ("c1", "Read", {"path": "a.txt"}), ("c2", "Write", {"path": "b.txt", "content": "x"}))
    got = {}
    for _ in range(2):
        r = gw.expect("result", sid="s5")
        got[r["id"]] = r
    assert got["c1"]["ok"] is True, got["c1"]
    assert got["c2"]["ok"] is False and "只读模式" in got["c2"]["content"], got["c2"]
    assert not os.path.exists(os.path.join(WORK, "b.txt"))
    finish(gw, "s5")


@case("审批模式：Shell 先审批，批准后执行；拒绝的给模型错误结果")
def t_approve(gw):
    start_run(gw, "s6", mode="ask", frm="tg:admin")
    call(gw, "s6", ("c1", "Shell", {"command": "echo approved-ran"}))
    req = gw.expect("approve_req", sid="s6", id="c1", name="Shell")
    assert req["args"] == {"command": "echo approved-ran"} and req["from"] == "tg:admin", req
    gw.send(t="approve", sid="s6", id="c1", ok=True, **{"from": "tg:admin"})
    gw.expect("notice", sid="s6", text=lambda s: "已批准" in s)
    gw.expect("result", sid="s6", id="c1", ok=True, content="approved-ran\n")
    gw.expect("model", sid="s6")
    call(gw, "s6", ("c2", "Write", {"path": "denied.txt", "content": "x"}))
    gw.expect("approve_req", sid="s6", id="c2", name="Write")
    gw.send(t="approve", sid="s6", id="c2", ok=False, **{"from": "tg:admin"})
    gw.expect("result", sid="s6", id="c2", ok=False, content="管理员拒绝执行")
    assert not os.path.exists(os.path.join(WORK, "denied.txt"))
    finish(gw, "s6")


@case("审批超时：按拒绝处理")
def t_approve_timeout(gw):
    start_run(gw, "s7", mode="ask")
    call(gw, "s7", ("c1", "Shell", {"command": "true"}))
    gw.expect("approve_req", sid="s7")
    gw.expect("result", sid="s7", id="c1", ok=False, timeout=6, content=lambda s: "审批超时" in s)
    finish(gw, "s7")


@case("取消：执行器收到 SIGTERM，清理命令后退出")
def t_cancel(gw):
    start_run(gw, "s8")
    call(gw, "s8", ("c1", "Shell", {"command": "sleep 4242"}))
    time.sleep(0.5)
    gw.send(t="cancel", sid="s8")
    gw.expect("result", sid="s8", id="c1", ok=False, content="已取消")
    gw.expect("done", sid="s8", status="cancelled")
    time.sleep(0.5)
    assert not leftover("sleep 4242"), f"取消后进程还在：{leftover('sleep 4242')}"


@case("命令留下的后台进程会被清理")
def t_background(gw):
    start_run(gw, "s9")
    t0 = time.time()
    call(gw, "s9", ("c1", "Shell", {"command": "sleep 4343 & echo parent done"}))
    gw.expect("result", sid="s9", id="c1", ok=True, timeout=6, content=lambda s: "parent done" in s)
    assert time.time() - t0 < 4, time.time() - t0
    time.sleep(0.2)
    assert not leftover("sleep 4343"), leftover("sleep 4343")
    finish(gw, "s9")


@case("工作目录不能包含 nano-agent 自己")
def t_guard(gw):
    start_run(gw, "s10", dirs=[T])
    call(gw, "s10", ("c1", "Read", {"path": "kernel.conf"}))
    gw.expect("result", sid="s10", id="c1", ok=False, content=lambda s: "包含 nano-agent 自己" in s)
    finish(gw, "s10")


@case("非法参数和错误结局")
def t_bad_args(gw):
    start_run(gw, "s11")
    gw.send(t="turn", sid="s11", reason="tool_use", calls=[{"id": "c1", "name": "Read", "args": "{not json"}])
    gw.expect("result", sid="s11", id="c1", ok=False, content=lambda s: "不是合法的 JSON" in s)
    gw.expect("model", sid="s11")
    gw.send(t="turn", sid="s11", reason="error", detail="upstream 500")
    gw.expect("done", sid="s11", status="error", detail="upstream 500")


@case("断线：终止执行中的工具，重连后重新 hello")
def t_reconnect(gw):
    start_run(gw, "s12")
    call(gw, "s12", ("c1", "Shell", {"command": "sleep 4242"}))
    time.sleep(0.5)
    gw.conn.close()
    gw.accept()
    hello = gw.expect("hello", timeout=10)
    assert hello["token"] == "t0k"
    gw.send(t="welcome", v=5)
    time.sleep(0.5)
    assert not leftover("sleep 4242"), f"断线后工具还在跑：{leftover('sleep 4242')}"
    gw.send(t="turn", sid="s12", reason="final")   # 旧 run 已丢弃，内核应忽略
    start_run(gw, "s13")
    gw.send(t="turn", sid="s13", reason="final")
    gw.expect("done", sid="s13", status="ok")


@case("列目录：只列子目录，包含内核自己的目录标出来")
def t_ls(gw):
    os.makedirs(os.path.join(WORK, "sub", "deep"), exist_ok=True)
    os.makedirs(os.path.join(WORK, ".hidden"), exist_ok=True)
    gw.send(t="ls", id="l1", path=WORK)
    r = gw.expect("ls_result", id="l1", ok=True)
    names = [e["name"] for e in r["entries"]]
    assert names == [".hidden", "sub"], names   # a.txt 之类的文件不列
    assert r["path"] == os.path.realpath(WORK) and not r["blocked"], r
    gw.send(t="ls", id="l2", path=T)   # T 里有 kernel.conf：T 本身不能当工作目录，work 可以
    r = gw.expect("ls_result", id="l2", ok=True)
    assert r["blocked"] is True, r
    assert {"name": "work"} in r["entries"], r["entries"]
    gw.send(t="ls", id="l3", path="~")
    r = gw.expect("ls_result", id="l3", ok=True)
    assert r["path"] == os.path.realpath(os.path.join(T, "home")) and r["home"] == os.path.join(T, "home"), r
    gw.send(t="ls", id="l4", path="/no/such/dir")
    gw.expect("ls_result", id="l4", ok=False, detail=lambda d: "No such file" in d)
    gw.send(t="ls", id="l5", path="relative")
    gw.expect("ls_result", id="l5", ok=False, detail=lambda d: "绝对路径" in d)


@case("异机模式：工具交给网关执行，结果交回模型，约束不由内核给")
def t_gateway_exec(gw):
    start_gateway_run(gw, "g1")
    call(gw, "g1", ("c1", "Write", {"path": "r.txt", "content": "x"}), ("c2", "Read", {"path": "r.txt"}))
    got = expect_all(gw, 2, "g1")
    w = got[("exec", "c1")]
    assert w["name"] == "Write" and w["args"] == {"path": "r.txt", "content": "x"} and w["timeout_ms"] > 0, w
    assert "dirs" not in w and "ro" not in w and w["workspace"] == "box:~/proj", w
    gw.send(t="exec_result", sid="g1", id="c2", ok=True, content="1\tx")
    gw.send(t="exec_result", sid="g1", id="c1", ok=False, content="写不了")
    got = expect_all(gw, 2, "g1")
    assert got[("result", "c2")]["ok"] and got[("result", "c2")]["content"] == "1\tx", got
    assert not got[("result", "c1")]["ok"] and got[("result", "c1")]["content"] == "写不了", got
    gw.send(t="exec_result", sid="g1", id="c1", ok=True, content="迟到的结果")   # 没有在等的调用，忽略
    finish(gw, "g1")
    assert not os.path.exists(os.path.join(WORK, "r.txt")), "异机模式不该在本机执行"


@case("异机模式：执行的机器没有沙箱时，自动模式的 Shell 也要审批")
def t_gateway_nosandbox(gw):
    start_gateway_run(gw, "g2", landlock=0)
    call(gw, "g2", ("c1", "Shell", {"command": "ls"}), ("c2", "Read", {"path": "a"}))
    got = expect_all(gw, 2, "g2")
    assert ("approve_req", "c1") in got and ("exec", "c2") in got, got
    gw.send(t="approve", sid="g2", id="c1", ok=True, **{"from": "tg:admin"})
    gw.expect("notice", sid="g2")
    gw.expect("exec", sid="g2", id="c1")
    gw.send(t="exec_result", sid="g2", id="c1", ok=True, content="a")
    gw.send(t="exec_result", sid="g2", id="c2", ok=True, content="b")
    expect_all(gw, 2, "g2")
    finish(gw, "g2")


@case("异机模式：停止时请网关取消执行")
def t_gateway_cancel(gw):
    start_gateway_run(gw, "g3")
    call(gw, "g3", ("c1", "Shell", {"command": "sleep 100"}))
    gw.expect("exec", sid="g3", id="c1")
    gw.send(t="cancel", sid="g3")
    got = expect_all(gw, 3, "g3")
    assert ("exec_cancel", "c1") in got and got[("result", "c1")]["content"] == "已取消", got
    assert got[("done", None)]["status"] == "cancelled", got


def old_ssh_config():
    """kernel.conf 里的 remote = / ssh = 已经不用了，要给出明白的提示"""
    conf = os.path.join(T, "old.conf")
    with open(conf, "w") as f:
        f.write(f"gateway = unix:{T}/none.sock\ntoken = x\nexec = {EXEC}\nremote = box box\n")
    r = subprocess.run([KERNEL, "-t", "-c", conf], capture_output=True, text=True)
    if r.returncode != 0 and "网页「设备」" in r.stderr:
        print("  ok   旧的 remote = 配置给出提示")
    else:
        FAILS.append("old remote config")
        print(f"  FAIL 旧的 remote = 配置：{r.returncode} {r.stderr}")


def main():
    try:
        os.makedirs(WORK)
        os.makedirs(os.path.join(T, "home"))
        sock = os.path.join(T, "gw.sock")
        conf = os.path.join(T, "kernel.conf")
        with open(conf, "w") as f:
            f.write(f"gateway = unix:{sock}\ntoken = t0k\nexec = {EXEC}\nskills = skills\n"
                    f"max_output = 4096\napproval_timeout = 2\n")

        gw = Gateway(sock)
        log = open(os.path.join(T, "kernel.log"), "w")
        env = dict(os.environ, HOME=os.path.join(T, "home"))
        proc = subprocess.Popen([KERNEL, "-c", conf], stderr=log, env=env)
        try:
            gw.accept()
            hello = gw.expect("hello")
            names = [t["name"] for t in hello["tools"]]
            assert hello["token"] == "t0k" and hello["v"] == 9, hello
            assert hello["os"] in ("linux", "macos", "windows") and hello["arch"], hello
            assert names == ["Edit", "Grep", "Read", "Shell", "Write"], names   # Skill 由网关提供
            assert all("input_schema" in t and t["description"] for t in hello["tools"])
            assert all("workspace" in t["input_schema"]["properties"] for t in hello["tools"]), hello["tools"]
            assert "landlock" in hello["sandbox"], hello
            gw.send(t="welcome", v=5)
            print(f"hello ok：{names}，sandbox={hello['sandbox']}")
            for t in (t_ls, t_skill, t_multi_ws, t_host_run, t_roundtrip, t_parallel, t_timeout, t_output, t_modes, t_approve,
                      t_approve_timeout, t_cancel, t_background, t_guard, t_bad_args, t_gateway_exec, t_gateway_nosandbox,
                      t_gateway_cancel, t_reconnect):
                t(gw)

            # 内核退出时，正在跑的命令也要被清理
            start_run(gw, "s14")
            call(gw, "s14", ("c1", "Shell", {"command": "sleep 4646"}))
            time.sleep(0.5)
        finally:
            proc.terminate()
            try:
                proc.wait(5)
            except subprocess.TimeoutExpired:
                proc.kill()
            log.close()
        time.sleep(0.3)
        old_ssh_config()
        if leftover("sleep 4646"):
            FAILS.append("exit cleanup")
            print(f"  FAIL 内核退出后命令还在：{leftover('sleep 4646')}")
            subprocess.run(["pkill", "-f", "sleep 4646"])
        else:
            print("  ok   内核退出时清理正在执行的命令")
        update_tests(T)
        kernel_log = open(os.path.join(T, "kernel.log"), errors="replace").read()
        if "Sanitizer" in kernel_log:
            FAILS.append("sanitizer")
            print("内核日志里有 Sanitizer 报告：")
        elif proc.returncode != 0:
            FAILS.append("exit code")
            print(f"内核退出码 {proc.returncode}")
        if FAILS:
            print(kernel_log[-3000:])
        print(f"host: {len(FAILS)} failures")
        return 1 if FAILS else 0
    finally:
        shutil.rmtree(T, ignore_errors=True)


def update_tests(T):
    """内核自我更新：换上新程序后重启并确认；新版本连不上网关时退回；启动次数超了时退回。
    用单独的内核实例，内核和执行器放在同一个目录（安装布局）。"""
    import base64
    U = os.path.join(T, "upd")
    os.makedirs(os.path.join(U, "home"))
    kbin, xbin = os.path.join(U, "nano-kernel"), os.path.join(U, "nano-exec")
    shutil.copy2(KERNEL, kbin)
    shutil.copy2(EXEC, xbin)
    sock = os.path.join(U, "gw.sock")
    conf = os.path.join(U, "kernel.conf")
    with open(conf, "w") as f:
        f.write(f"gateway = unix:{sock}\ntoken = t0k\nexec = {xbin}\n")
    gw = Gateway(sock)
    log = open(os.path.join(U, "kernel.log"), "w")
    env = dict(os.environ, HOME=os.path.join(U, "home"), NA_UPDATE_CONFIRM_MS="2000")
    files = [{"path": n, "data": base64.b64encode(open(p, "rb").read()).decode()}
             for n, p in (("nano-kernel", kbin), ("nano-exec", xbin))]
    pending, failed = os.path.join(U, "update-pending.json"), os.path.join(U, "update-failed")

    def check(name, fn):
        t0 = time.time()
        try:
            fn()
            print(f"  ok   {name} ({time.time() - t0:.1f}s)")
        except Exception as e:  # noqa: BLE001
            FAILS.append(name)
            print(f"  FAIL {name}：{e}")
            log.flush()
            print(open(os.path.join(U, "kernel.log"), errors="replace").read()[-1500:])

    def wait_gone(p, timeout=5):
        end = time.time() + timeout
        while os.path.exists(p) and time.time() < end:
            time.sleep(0.05)
        return not os.path.exists(p)

    proc = subprocess.Popen([kbin, "-c", conf], stderr=log, env=env)
    try:
        gw.accept()
        hello = gw.expect("hello")
        ver = hello["version"]
        assert ver and "update_failed" not in hello, hello
        gw.send(t="welcome", v=9)

        def ok_update():
            gw.send(t="update", version=ver, files=files)
            r = gw.expect("update_result")
            assert r["ok"] is True and r["version"] == ver, r
            gw.accept()                       # exec 后同一个进程重新连上
            h = gw.expect("hello")
            assert h["version"] == ver and proc.poll() is None, (h, proc.poll())
            assert os.path.exists(kbin + ".old") and os.path.exists(pending)
            if os.path.exists(f"/proc/{proc.pid}/exe"):   # 跑的是换上来的新文件，不是改名后的旧文件
                assert os.readlink(f"/proc/{proc.pid}/exe") == kbin, os.readlink(f"/proc/{proc.pid}/exe")
            gw.send(t="welcome", v=9)
            assert wait_gone(pending), "被网关接受后 update-pending.json 还在"
        check("自我更新：换上新程序、重启、被接受后确认", ok_update)

        def bad_name():
            gw.send(t="update", version=ver, files=[{"path": "../evil", "data": "AAAA"}] + files)
            r = gw.expect("update_result")
            assert r["ok"] is False and "不认识" in r["detail"], r
            assert not os.path.exists(os.path.join(T, "evil")) and proc.poll() is None
        check("自我更新：不认识的文件名直接拒绝", bad_name)

        def rollback_unconfirmed():
            gw.send(t="update", version=ver, files=files)
            assert gw.expect("update_result")["ok"] is True
            gw.accept()
            gw.expect("hello")
            gw.conn.close()                   # 不接受新版本：超时后退回
            gw.accept()
            h = gw.expect("hello", timeout=15)
            if "update_failed" not in h:      # 退回前的重连
                gw.conn.close()
                gw.accept()
                h = gw.expect("hello", timeout=15)
            assert h.get("update_failed") == ver, h
            assert os.path.exists(kbin + ".bad") and not os.path.exists(pending), os.listdir(U)
            gw.send(t="welcome", v=9)
        check("自我更新：新版本没被网关接受就退回旧版本", rollback_unconfirmed)
    finally:
        proc.terminate()
        try:
            proc.wait(5)
        except subprocess.TimeoutExpired:
            proc.kill()

    def rollback_tries():
        shutil.copy2(kbin, kbin + ".old")
        with open(pending, "w") as f:
            json.dump({"from": "old", "to": ver, "tries": 3}, f)
        p = subprocess.Popen([kbin, "-c", conf], stderr=log, env=env)
        try:
            gw.accept()
            h = gw.expect("hello")
            assert h.get("update_failed") == ver and not os.path.exists(pending), h
        finally:
            p.terminate()
            p.wait(5)
    check("自我更新：启动次数超了就退回", rollback_tries)
    log.close()
    text = open(os.path.join(U, "kernel.log"), errors="replace").read()
    if "Sanitizer" in text:
        FAILS.append("update sanitizer")
        print(text[-3000:])


if __name__ == "__main__":
    sys.exit(main())
