#!/usr/bin/env python3
"""用一个假网关驱动真实的 nano-kernel 进程，测试宿主层：
协议收发、子进程执行、超时强杀、输出截断、审批、取消、断线重连。

用法：python3 tests/test_host.py ./nano-kernel
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
FAILS = []


def tool(root, name, script, **policy):
    d = os.path.join(root, name)
    os.makedirs(d)
    spec = {"name": name, "description": f"test tool {name}",
            "input_schema": {"type": "object", "properties": {}}}
    spec.update(policy)
    with open(os.path.join(d, "tool.json"), "w") as f:
        json.dump(spec, f)
    with open(os.path.join(d, "run"), "w") as f:
        f.write("#!/bin/sh\n" + script + "\n")
    os.chmod(os.path.join(d, "run"), 0o755)


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


def start_run(gw, sid, frm="tg:user"):
    gw.send(t="input", sid=sid, **{"from": frm})
    gw.expect("model", sid=sid)


@case("工具往返：参数走 stdin，结果回网关")
def t_echo(gw):
    start_run(gw, "s1")
    gw.send(t="turn", sid="s1", reason="tool_use",
            calls=[{"id": "c1", "name": "echo", "args": {"x": 1, "s": "中文"}}])
    r = gw.expect("result", sid="s1", id="c1", ok=True)
    assert json.loads(r["content"]) == {"x": 1, "s": "中文"}, r
    assert r["content"].count("\n") == 0
    gw.expect("model", sid="s1")
    gw.send(t="turn", sid="s1", reason="final")
    gw.expect("done", sid="s1", status="ok")


@case("并行调用：全部有结果后才请求模型")
def t_parallel(gw):
    start_run(gw, "s2")
    gw.send(t="turn", sid="s2", reason="tool_use", calls=[
        {"id": "a", "name": "echo", "args": {}},
        {"id": "b", "name": "fail", "args": {}},
        {"id": "c", "name": "nope", "args": {}},
    ])
    got = {}
    for _ in range(3):
        r = gw.expect("result", sid="s2")
        got[r["id"]] = r
    assert got["a"]["ok"] is True
    assert got["b"]["ok"] is False and "退出码 3" in got["b"]["content"] and "boom" in got["b"]["content"], got["b"]
    assert got["c"]["ok"] is False and "未知工具" in got["c"]["content"]
    gw.expect("model", sid="s2")
    gw.send(t="turn", sid="s2", reason="final")
    gw.expect("done", sid="s2", status="ok")


@case("超时：强杀整个进程组")
def t_timeout(gw):
    start_run(gw, "s3")
    t0 = time.time()
    gw.send(t="turn", sid="s3", reason="tool_use", calls=[{"id": "c1", "name": "slow", "args": {}}])
    r = gw.expect("result", sid="s3", id="c1", ok=False, timeout=5)
    assert "执行超时" in r["content"], r
    assert 0.8 < time.time() - t0 < 3, time.time() - t0
    gw.expect("model", sid="s3")
    time.sleep(0.3)
    left = subprocess.run(["pgrep", "-f", "sleep 4242"], capture_output=True, text=True).stdout
    assert not left.strip(), f"孙进程没被杀掉：{left}"
    gw.send(t="turn", sid="s3", reason="final")
    gw.expect("done", sid="s3", status="ok")


@case("超长输出：截断并注明")
def t_big(gw):
    start_run(gw, "s4")
    gw.send(t="turn", sid="s4", reason="tool_use", calls=[{"id": "c1", "name": "big", "args": {}}])
    r = gw.expect("result", sid="s4", id="c1", ok=True)
    assert "已截断" in r["content"], r["content"][-200:]
    assert len(r["content"].encode()) < 4096 + 200, len(r["content"].encode())
    r["content"].encode("utf-8")  # 截断不能切坏 UTF-8
    gw.expect("model", sid="s4")
    gw.send(t="turn", sid="s4", reason="final")
    gw.expect("done", sid="s4")


@case("二进制输出：非法 UTF-8 和 \\0 被替换，JSON 仍然合法")
def t_binary(gw):
    start_run(gw, "s4b")
    gw.send(t="turn", sid="s4b", reason="tool_use", calls=[{"id": "c1", "name": "binary", "args": {}}])
    r = gw.expect("result", sid="s4b", id="c1", ok=True)
    assert r["content"] == "ok\ufffd\ufffdend 中", repr(r["content"])
    gw.expect("model", sid="s4b")
    gw.send(t="turn", sid="s4b", reason="final")
    gw.expect("done", sid="s4b")


@case("权限：非管理员调用管理员工具直接得到错误结果")
def t_perm(gw):
    start_run(gw, "s5", "tg:user")
    gw.send(t="turn", sid="s5", reason="tool_use", calls=[{"id": "c1", "name": "admin_tool", "args": {}}])
    r = gw.expect("result", sid="s5", id="c1", ok=False)
    assert "权限不足" in r["content"], r
    gw.expect("model", sid="s5")
    gw.send(t="turn", sid="s5", reason="final")
    gw.expect("done", sid="s5")


@case("审批：非管理员审批无效，管理员批准后执行")
def t_approve(gw):
    start_run(gw, "s6", "tg:admin")
    gw.send(t="turn", sid="s6", reason="tool_use",
            calls=[{"id": "c1", "name": "guarded", "args": {"cmd": "x"}}])
    req = gw.expect("approve_req", sid="s6", id="c1", name="guarded")
    assert req["args"] == {"cmd": "x"} and req["from"] == "tg:admin", req
    gw.send(t="approve", sid="s6", id="c1", ok=True, **{"from": "tg:user"})
    gw.expect("notice", sid="s6", text=lambda s: "不是管理员" in s)
    gw.send(t="approve", sid="s6", id="c1", ok=True, **{"from": "tg:admin"})
    gw.expect("notice", sid="s6", text=lambda s: "已批准" in s)
    gw.expect("result", sid="s6", id="c1", ok=True, content=lambda s: "guarded ran" in s)
    gw.expect("model", sid="s6")
    gw.send(t="turn", sid="s6", reason="final")
    gw.expect("done", sid="s6")


@case("审批超时：按拒绝处理")
def t_approve_timeout(gw):
    start_run(gw, "s7", "tg:admin")
    gw.send(t="turn", sid="s7", reason="tool_use", calls=[{"id": "c1", "name": "guarded", "args": {}}])
    gw.expect("approve_req", sid="s7")
    r = gw.expect("result", sid="s7", id="c1", ok=False, timeout=6)
    assert "审批超时" in r["content"], r
    gw.expect("model", sid="s7")
    gw.send(t="turn", sid="s7", reason="final")
    gw.expect("done", sid="s7")


@case("取消：终止正在执行的工具并结束 run")
def t_cancel(gw):
    start_run(gw, "s8")
    gw.send(t="turn", sid="s8", reason="tool_use", calls=[{"id": "c1", "name": "slow", "args": {}}])
    time.sleep(0.3)
    gw.send(t="cancel", sid="s8")
    r = gw.expect("result", sid="s8", id="c1", ok=False)
    assert "已取消" in r["content"], r
    gw.expect("done", sid="s8", status="cancelled")
    time.sleep(0.3)
    left = subprocess.run(["pgrep", "-f", "sleep 4242"], capture_output=True, text=True).stdout
    assert not left.strip(), f"取消后进程还在：{left}"


@case("后台孙进程占着管道：退出后最多再等 2 秒")
def t_orphan(gw):
    start_run(gw, "s9")
    t0 = time.time()
    gw.send(t="turn", sid="s9", reason="tool_use", calls=[{"id": "c1", "name": "orphan", "args": {}}])
    r = gw.expect("result", sid="s9", id="c1", ok=True, timeout=6)
    assert "parent done" in r["content"], r
    assert time.time() - t0 < 4, time.time() - t0
    gw.expect("model", sid="s9")
    gw.send(t="turn", sid="s9", reason="final")
    gw.expect("done", sid="s9")
    subprocess.run(["pkill", "-f", "sleep 4343"])


@case("非法参数和错误结局")
def t_bad_args(gw):
    start_run(gw, "s10")
    gw.send(t="turn", sid="s10", reason="tool_use",
            calls=[{"id": "c1", "name": "echo", "args": "{not json"}])
    r = gw.expect("result", sid="s10", id="c1", ok=False)
    assert "不是合法的 JSON" in r["content"], r
    gw.expect("model", sid="s10")
    gw.send(t="turn", sid="s10", reason="error", detail="upstream 500")
    gw.expect("done", sid="s10", status="error", detail="upstream 500")


@case("断线：杀掉工具，重连后重新 hello")
def t_reconnect(gw):
    start_run(gw, "s11")
    gw.send(t="turn", sid="s11", reason="tool_use", calls=[{"id": "c1", "name": "slow", "args": {}}])
    time.sleep(0.3)
    gw.conn.close()
    gw.accept()
    hello = gw.expect("hello", timeout=10)
    assert hello["token"] == "t0k"
    gw.send(t="welcome", v=1)
    time.sleep(0.3)
    left = subprocess.run(["pgrep", "-f", "sleep 4242"], capture_output=True, text=True).stdout
    assert not left.strip(), f"断线后工具还在跑：{left}"
    gw.send(t="turn", sid="s11", reason="final")   # 旧 run 已丢弃，内核应忽略
    start_run(gw, "s12")
    gw.send(t="turn", sid="s12", reason="final")
    gw.expect("done", sid="s12", status="ok")


def main():
    tmp = tempfile.mkdtemp(prefix="na-host-")
    try:
        tools = os.path.join(tmp, "tools")
        os.makedirs(tools)
        tool(tools, "echo", "cat", allow="all", timeout=5)
        tool(tools, "fail", "echo boom >&2; exit 3", allow="all", timeout=5)
        tool(tools, "slow", "sleep 4242 & wait", allow="all", timeout=1)
        tool(tools, "big", "i=0; while [ $i -lt 5000 ]; do echo '一二三四五六七八九十'; i=$((i+1)); done",
             allow="all", timeout=5)
        tool(tools, "admin_tool", "echo hi", allow="admin", timeout=5)
        tool(tools, "binary", "printf 'ok\\377\\000end \\344\\270\\255'", allow="all", timeout=5)
        tool(tools, "guarded", "echo guarded ran", allow="admin", approval=True, timeout=5)
        tool(tools, "orphan", "sleep 4343 & echo parent done", allow="all", timeout=10)
        os.makedirs(os.path.join(tools, "broken"))   # 没有 tool.json，应被跳过

        sock = os.path.join(tmp, "gw.sock")
        conf = os.path.join(tmp, "kernel.conf")
        with open(conf, "w") as f:
            f.write(f"gateway = unix:{sock}\ntoken = t0k\nadmin = tg:admin\n"
                    f"tools_dir = tools\nmax_output = 4096\napproval_timeout = 2\n")

        gw = Gateway(sock)
        log = open(os.path.join(tmp, "kernel.log"), "w")
        proc = subprocess.Popen([KERNEL, "-c", conf], stderr=log)
        try:
            gw.accept()
            hello = gw.expect("hello")
            names = [t["name"] for t in hello["tools"]]
            assert hello["token"] == "t0k" and hello["v"] == 1
            assert names == sorted(names) and "broken" not in names and len(names) == 8, names
            assert all("input_schema" in t for t in hello["tools"])
            gw.send(t="welcome", v=1)
            print(f"hello ok：{names}")
            for t in (t_echo, t_parallel, t_timeout, t_big, t_binary, t_perm, t_approve,
                      t_approve_timeout, t_cancel, t_orphan, t_bad_args, t_reconnect):
                t(gw)
        finally:
            proc.terminate()
            try:
                proc.wait(5)
            except subprocess.TimeoutExpired:
                proc.kill()
            log.close()
        kernel_log = open(os.path.join(tmp, "kernel.log")).read()
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
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
