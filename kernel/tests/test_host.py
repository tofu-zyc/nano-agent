#!/usr/bin/env python3
"""用一个假网关驱动真实的 nano-kernel 进程（它再启动真实的 nano-exec），测试宿主层：
协议收发、权限模式、审批、超时、取消、断线重连、进程清理；
以及远程执行：用一个假的 ssh 把命令放到本机的另一个“服务器主目录”里执行。

用法：python3 tests/test_host.py ./nano-kernel ./nano-exec
"""
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


def start_remote_run(gw, sid, node, dirs, mode="auto"):
    gw.send(t="input", sid=sid, mode=mode, node=node, dirs=list(dirs), ro=[], rw=[], **{"from": "tg:user"})
    gw.expect("model", sid=sid)


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


# 假的 ssh：跳过选项，把命令交给 sh 在“服务器”的主目录（$REMOTE_ROOT/目标）里执行。目标 down 连不上
FAKE_SSH = r"""#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2 ;;
    -T) shift ;;
    --) shift; break ;;
    *) break ;;
  esac
done
target=$1; shift
if [ "$target" = down ]; then echo "ssh: connect to host down port 22: Connection refused" >&2; exit 255; fi
export HOME="$REMOTE_ROOT/$target"
mkdir -p "$HOME" && cd "$HOME" && exec sh -c "$*"
"""


def remote_phase():
    """配了三台服务器的内核：box 自动上传执行器，down 连不上，fixed 指定的执行器不存在"""
    root = os.path.join(T, "remote")
    box = os.path.join(root, "box")
    os.makedirs(os.path.join(box, "proj"))
    ssh = os.path.join(T, "fakessh")
    with open(ssh, "w") as f:
        f.write(FAKE_SSH)
    os.chmod(ssh, 0o755)
    sock = os.path.join(T, "gw2.sock")
    conf = os.path.join(T, "kernel2.conf")
    with open(conf, "w") as f:
        f.write(f"gateway = unix:{sock}\ntoken = t0k\nexec = {EXEC}\nssh = {ssh}\n"
                f"skills = {SKILLS}\nremote = box box\nremote = down down\nremote = fixed box /nonexistent/nano-exec\n")
    gw = Gateway(sock)
    log = open(os.path.join(T, "kernel2.log"), "w")
    env = dict(os.environ, HOME=os.path.join(T, "home"), REMOTE_ROOT=root, XDG_RUNTIME_DIR=os.path.join(T, "run"))
    os.makedirs(os.path.join(T, "run"), mode=0o700)
    proc = subprocess.Popen([KERNEL, "-c", conf], stderr=log, env=env)
    try:
        gw.accept()
        hello = gw.expect("hello")
        names = [r["name"] for r in hello["remotes"]]
        assert names == ["box", "down", "fixed"] and not any(r["ok"] for r in hello["remotes"]), hello["remotes"]
        gw.send(t="welcome", v=3)
        run_remote_cases(gw, box)
    finally:
        proc.terminate()
        try:
            proc.wait(5)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()
    klog = open(os.path.join(T, "kernel2.log"), errors="replace").read()
    if "Sanitizer" in klog or proc.returncode != 0:
        FAILS.append("remote kernel")
        print(f"  FAIL 远程测试的内核异常（退出码 {proc.returncode}）")
    if FAILS:
        print(klog[-3000:])


def run_remote_cases(gw, box):
    @case("远程：自动上传执行器；连不上、执行器不存在的报告不可用")
    def t_prepare(gw):
        state = {}
        deadline = time.time() + 20
        while time.time() < deadline and not (state.get("box", {}).get("ok") and "down" in state and "fixed" in state):
            m = gw.recv(20)
            if m.get("t") == "remotes":
                for r in m["remotes"]:
                    if r["ok"] or "detail" in r and ("连不上" in r["detail"] or "不存在" in r["detail"]):
                        state[r["name"]] = r
        assert state["box"]["ok"] and state["box"]["landlock"] >= 0, state
        assert "连不上" in state["down"]["detail"], state
        assert "/nonexistent/nano-exec" in state["fixed"]["detail"], state
        exe = os.listdir(os.path.join(box, ".cache", "nano-agent"))
        assert any(n.startswith("nano-exec-") for n in exe), exe

    @case("远程：在服务器上读写文件")
    def t_remote_tools(gw):
        start_remote_run(gw, "r1", "box", ["~/proj"])
        call(gw, "r1", ("c1", "Write", {"path": "r.txt", "content": "remote\n"}))
        gw.expect("result", sid="r1", id="c1", ok=True)
        assert open(os.path.join(box, "proj", "r.txt")).read() == "remote\n"
        gw.expect("model", sid="r1")
        call(gw, "r1", ("c2", "Shell", {"command": "pwd"}))
        gw.expect("result", sid="r1", id="c2", ok=True, content=lambda s: s.strip().endswith("/box/proj"))
        finish(gw, "r1")

    @case("远程：工作目录包含服务器上的执行器时，执行器自己拒绝")
    def t_remote_guard(gw):
        start_remote_run(gw, "r2", "box", ["~"])
        call(gw, "r2", ("c1", "Write", {"path": "x.txt", "content": "x"}))
        gw.expect("result", sid="r2", id="c1", ok=False, content=lambda s: "包含执行器自己" in s)
        finish(gw, "r2")

    @case("远程技能：上传附带文件，在服务器上读取、运行，不能改写")
    def t_remote_skill(gw):
        start_remote_run(gw, "rs", "box", ["~/proj"])
        call(gw, "rs", ("s1", "Skill", {"name": "demo"}))
        result = gw.expect("result", sid="rs", id="s1", ok=True)
        path = result["content"].split("技能目录：", 1)[1].split("（", 1)[0]
        assert path.startswith("~/.cache/nano-agent/skills/"), path
        gw.expect("model", sid="rs")
        call(gw, "rs", ("s2", "Read", {"path": path + "/data.txt"}),
             ("s3", "Shell", {"command": "sh " + path + "/scripts/run.sh"}),
             ("s4", "Write", {"path": path + "/data.txt", "content": "x"}))
        got = {r["id"]: r for r in (gw.expect("result", sid="rs") for _ in range(3))}
        assert got["s2"]["ok"] and "技能数据" in got["s2"]["content"], got
        assert got["s3"]["ok"] and "hi" in got["s3"]["content"], got
        assert not got["s4"]["ok"], got
        finish(gw, "rs")

    @case("远程：列服务器上的目录，包含执行器的标出来")
    def t_remote_ls(gw):
        gw.send(t="ls", id="L1", node="box", path="~")
        r = gw.expect("ls_result", id="L1", ok=True)
        ents = {e["name"]: e.get("blocked", False) for e in r["entries"]}
        assert ents.get("proj") is False and ents.get(".cache") is True, ents
        gw.send(t="ls", id="L2", node="down", path="~")
        gw.expect("ls_result", id="L2", ok=False, detail=lambda s: "用不了" in s)

    @case("远程：服务器不可用、不认识时，工具调用失败并说明原因")
    def t_remote_unavailable(gw):
        start_remote_run(gw, "r3", "down", ["~"])
        call(gw, "r3", ("c1", "Read", {"path": "a"}))
        gw.expect("result", sid="r3", id="c1", ok=False, content=lambda s: "down 现在用不了" in s)
        finish(gw, "r3")
        start_remote_run(gw, "r4", "nope", ["~"])
        call(gw, "r4", ("c1", "Read", {"path": "a"}))
        gw.expect("result", sid="r4", id="c1", ok=False, content=lambda s: "没有配置服务器 nope" in s)
        finish(gw, "r4")

    @case("远程：取消时服务器上的命令也被杀掉")
    def t_remote_cancel(gw):
        start_remote_run(gw, "r5", "box", ["~/proj"])
        call(gw, "r5", ("c1", "Shell", {"command": "sleep 4747"}))
        time.sleep(0.8)
        assert leftover("sleep 4747"), "命令没有跑起来"
        gw.send(t="cancel", sid="r5")
        gw.expect("result", sid="r5", id="c1", ok=False, content="已取消")
        gw.expect("done", sid="r5", status="cancelled")
        for _ in range(30):
            if not leftover("sleep 4747"):
                break
            time.sleep(0.1)
        assert not leftover("sleep 4747"), leftover("sleep 4747")

    for t in (t_prepare, t_remote_tools, t_remote_guard, t_remote_skill, t_remote_ls, t_remote_unavailable, t_remote_cancel):
        t(gw)


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
                      t_cancel, t_background, t_guard, t_bad_args, t_reconnect):
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
        remote_phase()
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
