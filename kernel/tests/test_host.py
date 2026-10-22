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


def start_run(gw, sid, mode="auto", dirs=(WORK,), frm="tg:user"):
    gw.send(t="input", sid=sid, mode=mode, dirs=list(dirs), ro=[], rw=[], **{"from": frm})
    gw.expect("model", sid=sid)


def call(gw, sid, *calls):
    gw.send(t="turn", sid=sid, reason="tool_use",
            calls=[{"id": c[0], "name": c[1], "args": c[2]} for c in calls])


def finish(gw, sid):
    gw.expect("model", sid=sid)
    gw.send(t="turn", sid=sid, reason="final")
    gw.expect("done", sid=sid, status="ok")


def start_gateway_run(gw, sid, mode="auto", landlock=7):
    gw.send(t="input", sid=sid, mode=mode, dirs=["~/proj"], ro=[], rw=[], exec="gateway", landlock=landlock,
            **{"from": "tg:user"})
    gw.expect("model", sid=sid)


def expect_all(gw, n, sid):
    got = {}
    for _ in range(n):
        m = gw.recv()
        assert m.get("sid") == sid, m
        got[(m["t"], m.get("id"))] = m
    return got


def leftover(tag):
    return subprocess.run(["pgrep", "-f", tag], capture_output=True, text=True).stdout.strip()


SKILLS = os.path.join(T, "skills")


@case("技能：Skill 返回说明，附带文件在沙箱里只读可见")
def t_skill(gw):
    start_run(gw, "sk")
    call(gw, "sk", ("c1", "Skill", {"name": "demo"}))
    gw.expect("result", sid="sk", id="c1", ok=True,
              content=lambda s: s.startswith("技能 demo\n技能目录：") and "附带文件：data.txt、scripts/run.sh" in s
              and s.rstrip().endswith("照着做就行。"))
    gw.expect("model", sid="sk")
    call(gw, "sk", ("c2", "Shell", {"command": f"cat {SKILLS}/demo/data.txt"}),
         ("c3", "Write", {"path": f"{SKILLS}/demo/data.txt", "content": "x"}),
         ("c4", "Skill", {"name": "nope"}))
    got = {}
    for _ in range(3):
        r = gw.expect("result", sid="sk")
        got[r["id"]] = r
    assert got["c2"]["ok"] and "技能数据" in got["c2"]["content"], got["c2"]
    assert not got["c3"]["ok"], got["c3"]
    assert not got["c4"]["ok"] and "没有这个技能" in got["c4"]["content"], got["c4"]
    finish(gw, "sk")


@case("技能热更新：下一轮加载新增技能，删除后撤下 Skill 工具")
def t_skill_reload(gw):
    demo = os.path.join(SKILLS, "demo", "SKILL.md")
    saved = open(demo).read()
    os.unlink(demo)
    gw.send(t="input", sid="reload", mode="auto", dirs=[WORK], **{"from": "tg:user"})
    msg = gw.expect("tools", skills=[])
    assert "Skill" not in [t["name"] for t in msg["tools"]], msg
    finish(gw, "reload")

    with open(demo, "w") as f:
        f.write(saved)
    gw.send(t="input", sid="reload2", mode="auto", dirs=[WORK], **{"from": "tg:user"})
    msg = gw.expect("tools")
    assert msg["skills"][0]["name"] == "demo", msg
    assert "Skill" in [t["name"] for t in msg["tools"]], msg
    finish(gw, "reload2")


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
    gw.send(t="welcome", v=3)
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
    assert "dirs" not in w and "ro" not in w, w
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


@case("异机模式：技能文件交给网关传到执行的机器上")
def t_gateway_skill(gw):
    start_gateway_run(gw, "g4")
    call(gw, "g4", ("c1", "Skill", {"name": "demo"}))
    m = gw.expect("skill_files", sid="g4", id="c1", skill="demo")
    files = {f["path"]: f for f in m["files"]}
    assert set(files) == {"SKILL.md", "data.txt", "scripts/run.sh"}, files.keys()
    assert base64.b64decode(files["data.txt"]["data"]).decode() == "技能数据\n", files["data.txt"]
    gw.send(t="exec_result", sid="g4", id="c1", ok=True, content="~/.cache/nano-agent/skills/pc/demo")
    gw.expect("result", sid="g4", id="c1", ok=True,
              content=lambda s: "技能目录：~/.cache/nano-agent/skills/pc/demo（" in s and "照着做就行" in s)
    gw.expect("model", sid="g4")
    call(gw, "g4", ("c2", "Skill", {"name": "nope"}))
    gw.expect("result", sid="g4", id="c2", ok=False)
    finish(gw, "g4")


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
        os.makedirs(os.path.join(SKILLS, "demo", "scripts"))
        with open(os.path.join(SKILLS, "demo", "SKILL.md"), "w") as f:
            f.write("---\nname: demo\ndescription: >\n  示例技能，\n  测试用\n---\n\n# 示例\n照着做就行。\n")
        with open(os.path.join(SKILLS, "demo", "data.txt"), "w") as f:
            f.write("技能数据\n")
        with open(os.path.join(SKILLS, "demo", "scripts", "run.sh"), "w") as f:
            f.write("echo hi\n")
        os.makedirs(os.path.join(SKILLS, "broken"))  # 没有 SKILL.md 的目录不算技能
        sock = os.path.join(T, "gw.sock")
        conf = os.path.join(T, "kernel.conf")
        with open(conf, "w") as f:
            f.write(f"gateway = unix:{sock}\ntoken = t0k\nexec = {EXEC}\nskills = {SKILLS}\n"
                    f"max_output = 4096\napproval_timeout = 2\n")

        gw = Gateway(sock)
        log = open(os.path.join(T, "kernel.log"), "w")
        env = dict(os.environ, HOME=os.path.join(T, "home"))
        proc = subprocess.Popen([KERNEL, "-c", conf], stderr=log, env=env)
        try:
            gw.accept()
            hello = gw.expect("hello")
            names = [t["name"] for t in hello["tools"]]
            assert hello["token"] == "t0k" and hello["v"] == 3, hello
            assert names == ["Edit", "Grep", "Read", "Shell", "Skill", "Write"], names
            skill = next(t for t in hello["tools"] if t["name"] == "Skill")
            assert "- demo：示例技能， 测试用" in skill["description"], skill["description"]
            assert skill["input_schema"]["properties"]["name"]["enum"] == ["demo"], skill
            assert hello["skills"] == [{"name": "demo", "description": "示例技能， 测试用"}], hello["skills"]
            assert all("input_schema" in t and t["description"] for t in hello["tools"])
            assert "landlock" in hello["sandbox"], hello
            gw.send(t="welcome", v=3)
            print(f"hello ok：{names}，sandbox={hello['sandbox']}")
            for t in (t_ls, t_skill, t_skill_reload, t_roundtrip, t_parallel, t_timeout, t_output, t_modes, t_approve, t_approve_timeout,
                      t_cancel, t_background, t_guard, t_bad_args, t_gateway_exec, t_gateway_nosandbox, t_gateway_cancel,
                      t_gateway_skill, t_reconnect):
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


if __name__ == "__main__":
    sys.exit(main())
