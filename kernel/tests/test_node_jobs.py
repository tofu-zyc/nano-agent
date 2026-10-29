#!/usr/bin/env python3
"""Real C node/executor Job paths against a deterministic lease coordinator."""
import base64
import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

KERNEL = str(Path(sys.argv[1]).resolve())
EXEC = str(Path(sys.argv[2]).resolve())
WINDOWS = KERNEL.lower().endswith(".exe")


def native(path):
    if WINDOWS and os.name != "nt":
        return subprocess.check_output(["wslpath", "-w", str(path)], text=True).strip()
    return str(path)


def shell(command):
    if not WINDOWS:
        return command
    command = re.sub(r"while \[ ! -f '([^']+)' \]; do sleep 0.02; done",
                     lambda m: "while(!(Test-Path -LiteralPath '" + native(m[1]) + "')) { Start-Sleep -Milliseconds 20 }", command)
    command = command.replace("printf once >> launches", "[IO.File]::AppendAllText((Join-Path (Get-Location) 'launches'),'once')")
    command = re.sub(r"echo started >> ([a-z-]+)", r"Add-Content -LiteralPath '\1' -Value 'started'", command)
    command = command.replace("echo BAD > must-not-run", "Set-Content -LiteralPath 'must-not-run' -Value 'BAD'")
    return re.sub(r"printf ([a-z-]+)", r"[Console]::Out.Write('\1')", command)


def temp_root():
    if not WINDOWS or os.name == "nt":
        return None
    win = subprocess.check_output(["/mnt/c/Windows/System32/cmd.exe", "/d", "/c", "echo %TEMP%"],
                                  cwd="/mnt/c", text=True).strip().splitlines()[-1]
    return subprocess.check_output(["wslpath", "-u", win], text=True).strip()


class Node:
    def __init__(self, root, executor=EXEC):
        self.root = Path(root)
        self.server = socket.socket()
        self.server.bind(("127.0.0.1", 0))
        self.server.listen(1)
        self.server.settimeout(8)
        port = self.server.getsockname()[1]
        self.conf = self.root / "node.conf"
        self.conf.write_text(f"gateway = tcp:127.0.0.1:{port}\ntoken = fixture\nnode = job-test\n"
                             f"exec = {native(executor)}\nsandbox = off\nmax_execs = 2\nmax_parallel_shells = 1\n")
        self.log = (self.root / "node.log").open("w")
        self.process = None
        self.buffer = b""
        self.history = []
        self.leases = {}
        self.reject_prepare = False
        self.defer_commit = False
        self.deferred = None
        self.conn = None
        self.work = self.root / "work"
        self.work.mkdir()
        self.launch()

    def launch(self):
        env = dict(os.environ, HOME=str(self.root), NANO_HOME=str(self.root / "home"))
        self.process = subprocess.Popen([KERNEL, "-c", native(self.conf)], stderr=self.log, env=env)
        self.connect()

    def connect(self):
        self.conn, _ = self.server.accept()
        self.buffer = b""
        hello = self.raw()
        assert hello["t"] == "hello" and hello["features"]["job_manager"] == 1, hello
        self.epoch = hello["node_epoch"]
        self.send(t="welcome", v=11, features={"machine_leases": 1, "lease_detach": 1, "job_manager": 1})
        self.wait(lambda m: m["t"] == "jobs_sync")
        self.wait(lambda m: m["t"] == "lease_sync", past=True)

    def send(self, **message):
        self.conn.sendall(json.dumps(message, ensure_ascii=False).encode() + b"\n")

    def raw(self, timeout=8):
        deadline = time.monotonic() + timeout
        while b"\n" not in self.buffer:
            self.conn.settimeout(max(0.001, deadline-time.monotonic()))
            chunk = self.conn.recv(65536)
            if not chunk:
                raise AssertionError("node connection closed; " + (self.root / "node.log").read_text()[-5000:])
            self.buffer += chunk
        line, self.buffer = self.buffer.split(b"\n", 1)
        message = json.loads(line)
        self.history.append(message)
        return message

    def next(self, timeout=8):
        message = self.raw(timeout)
        kind = message["t"]
        if kind == "ping":
            self.send(t="pong")
        elif kind == "lease_sync":
            active=message.get("leases",[])
            receipts=message.get("lease_receipts",[])
            assert len(active)<=256 and len(receipts)<=256, message
            assert not ({item["lease_id"] for item in active} & {item["lease_id"] for item in receipts}), message
            for item in receipts:
                assert item["terminal_receipt"] and item["job_id"] and item["state"] in (
                    "succeeded","failed","cancelled","timed_out","interrupted"), item
            self.send(t="lease_synced", node_epoch=self.epoch, ok=True)
        elif kind == "lease_acquire":
            self.leases[message["lease_id"]] = message["lease"]
            self.send(t="lease_grant", node_epoch=self.epoch, lease_id=message["lease_id"])
        elif kind.startswith("lease_detach_"):
            phase = kind.removeprefix("lease_detach_")
            assert message["lease_id"] in self.leases, ("prepare before lease admission", message)
            reply = dict(t="lease_detach_result", node_epoch=self.epoch, lease_id=message["lease_id"],
                         request_id=message["request_id"], phase=phase, ok=not self.reject_prepare or phase != "prepare",
                         detail="fixture capacity full" if self.reject_prepare else "")
            if self.defer_commit and phase == "commit":
                self.deferred = reply
            else:
                self.send(**reply)
        return message

    def wait(self, predicate, timeout=8, past=False):
        if past:
            for message in reversed(self.history):
                if predicate(message):
                    return message
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            message = self.next(deadline-time.monotonic())
            if predicate(message):
                return message
        raise AssertionError("no matching response: " + repr(self.history[-12:]))

    def run(self, ident, command, execution="foreground", **args):
        origin = dict(agent="test-agent", sid="tg:owner", run_id="r-"+ident, turn_id=1,
                      call_id="c-"+ident, created_by="tg:owner")
        self.send(t="run", id=ident, req=dict(tool="Shell", args=dict(command=shell(command), execution=execution, **args),
                  origin=origin, dirs=[native(self.work)], write=True, sandbox="off"))

    def result(self, ident, **kw):
        return self.wait(lambda m: m["t"] == "run_result" and m["id"] == ident, past=True, **kw)

    def control(self, ident, action, job_id=None, **kw):
        self.send(t="job_control", request_id=ident, action=action, job_id=job_id, **kw)
        return self.wait(lambda m: m["t"] == "job_state" and m.get("request_id") == ident)

    def terminal(self, job_id):
        return self.wait(lambda m: m["t"] == "job_state" and m.get("job_id") == job_id and m.get("kind") == "terminal")["job"]

    def stop(self):
        (self.root / "node.stop").touch()
        self.process.wait(8)

    def close(self):
        if self.process:
            self.stop()
            assert self.process.returncode == 0, (self.root / "node.log").read_text()[-6000:]
            self.process = None
        if self.conn:
            self.conn.close()
        self.server.close()
        self.log.close()


def check():
    with tempfile.TemporaryDirectory(prefix="na-jobs-host-", dir=temp_root()) as root:
        node = Node(root)
        try:
            # Foreground is durable and still returns exactly one ordinary terminal.
            node.run("fore", "printf foreground", timeout=9)
            fore = node.result("fore")
            assert fore["ok"] and fore["content"] == "foreground", fore
            fore_id = fore["job_id"]
            assert node.control("status-fore", "status", fore_id)["job"]["status"] == "succeeded"

            # Explicit background returns while machine slot/resources are occupied.
            gate = node.work / "release"
            node.run("block", f"while [ ! -f '{gate}' ]; do sleep 0.02; done; printf blocker")
            node.wait(lambda m: m["t"] == "run_started" and m["id"] == "block")
            node.run("queued", "printf once >> launches; printf queued-done", execution="background")
            detached = node.result("queued")
            receipt = json.loads(detached["content"])
            assert detached["ok"] and receipt["status"] == "backgrounded" and receipt["state"] == "queued", detached
            queued_id = receipt["job_id"]
            assert not (node.work / "launches").exists()
            gate.touch()
            assert node.result("block")["ok"]
            terminal = node.terminal(queued_id)
            assert terminal["status"] == "succeeded" and (node.work / "launches").read_text() == "once", terminal
            output = node.control("logs", "output", queued_id, cursor=0, max_bytes=65536)
            chunks = output["data"]["chunks"]
            assert b"".join(base64.b64decode(c["data"]) for c in chunks) == b"queued-done", output
            assert output["data"]["next_cursor"] == 11, output

            # Auto yield detaches the existing PID, never starts a replacement.
            auto_gate = node.work / "auto-release"
            node.run("auto", f"echo started >> auto-launches; while [ ! -f '{auto_gate}' ]; do sleep 0.02; done; printf auto-done",
                     execution="auto", yield_after_ms=100)
            auto = node.result("auto")
            auto_id = auto["job_id"]
            deadline = time.monotonic() + 8
            while True:
                before = node.control("auto-status", "status", auto_id)["job"]
                if before.get("pid") and before["status"] == "running":
                    break
                assert time.monotonic() < deadline, before
                time.sleep(0.01)
            pid = before["pid"]
            assert before["background"] and before["status"] == "running", before
            # Foreground cancel after handoff is harmless.
            node.send(t="run_cancel", id="auto")
            assert node.control("auto-after", "status", auto_id)["job"]["pid"] == pid
            auto_gate.touch()
            assert node.terminal(auto_id)["status"] == "succeeded"
            assert (node.work / "auto-launches").read_text().splitlines() == ["started"]

            # Capacity rejection retains the original foreground operation.
            node.reject_prepare = True
            denied_gate = node.work / "denied-release"
            node.run("denied", f"while [ ! -f '{denied_gate}' ]; do sleep 0.02; done; printf kept", execution="auto", yield_after_ms=100)
            denied = node.wait(lambda m: m["t"] == "job_state" and m.get("kind") == "detach_denied")["job"]
            assert not denied["background"]
            assert not any(m["t"] == "run_result" and m["id"] == "denied" for m in node.history)
            denied_gate.touch()
            assert node.result("denied")["content"] == "kept"
            node.reject_prepare = False

            # Exit racing durable handoff still gives only the background receipt.
            node.defer_commit = True
            race_gate = node.work / "race-release"
            node.run("race", f"while [ ! -f '{race_gate}' ]; do sleep 0.02; done; printf race-done",
                     execution="auto", yield_after_ms=100)
            node.wait(lambda m: m["t"] == "lease_detach_commit")
            race_gate.touch()
            race_id = node.deferred["request_id"]
            node.terminal(race_id)
            assert not any(m["t"] == "run_result" and m["id"] == "race" for m in node.history)
            node.send(**node.deferred)
            assert json.loads(node.result("race")["content"])["status"] == "backgrounded"
            node.defer_commit = False

            # Detached jobs survive Gateway disconnect and reconcile the same PID.
            reconnect_gate = node.work / "reconnect-release"
            node.run("reconnect", f"echo started >> reconnect-launches; while [ ! -f '{reconnect_gate}' ]; do sleep 0.02; done; printf reconnect-done",
                     execution="auto", yield_after_ms=100)
            reconnect_id = node.result("reconnect")["job_id"]
            deadline = time.monotonic() + 8
            while True:
                prior = node.control("before-disconnect", "status", reconnect_id)["job"]
                if prior.get("pid") and prior["status"] == "running":
                    break
                assert time.monotonic() < deadline, prior
                time.sleep(0.01)
            node.conn.close()
            node.connect()
            restored = node.control("after-reconnect", "status", reconnect_id)["job"]
            assert restored["pid"] == prior["pid"] and restored["background"], restored
            reconnect_gate.touch()
            assert node.terminal(reconnect_id)["status"] == "succeeded"
            assert (node.work / "reconnect-launches").read_text().splitlines() == ["started"]

            # Cancel a queued job without running its command.
            cancel_gate = node.work / "cancel-release"
            node.run("cancel-block", f"while [ ! -f '{cancel_gate}' ]; do sleep 0.02; done")
            node.wait(lambda m: m["t"] == "run_started" and m["id"] == "cancel-block")
            node.run("cancel-queued", "echo BAD > must-not-run", execution="background")
            cancel_id = node.result("cancel-queued")["job_id"]
            assert node.control("cancel", "cancel", cancel_id)["ok"]
            assert node.terminal(cancel_id)["status"] == "cancelled"
            cancel_gate.touch()
            node.result("cancel-block")
            assert not (node.work / "must-not-run").exists()
            # Explicit control detaches an already-running foreground process.
            manual_gate = node.work / "manual-release"
            node.run("manual", f"while [ ! -f '{manual_gate}' ]; do sleep 0.02; done; printf manual-done")
            manual = node.wait(lambda m: m["t"] == "run_started" and m["id"] == "manual")["job_id"]
            assert node.control("manual-detach", "detach", manual)["ok"]
            assert json.loads(node.result("manual")["content"])["status"] == "backgrounded"
            assert node.control("manual-detach-repeat", "detach", manual)["ok"]
            manual_gate.touch()
            node.terminal(manual)
            # Running background cancellation closes the real process before terminal.
            stop_gate = node.work / "never-stop-release"
            node.run("stop-running", f"while [ ! -f '{stop_gate}' ]; do sleep 0.02; done",
                     execution="background")
            stopping = node.result("stop-running")["job_id"]
            deadline = time.monotonic() + 8
            while not node.control("stop-status", "status", stopping)["job"].get("pid"):
                assert time.monotonic() < deadline
                time.sleep(0.01)
            assert node.control("stop-running-control", "cancel", stopping)["ok"]
            assert node.terminal(stopping)["status"] == "cancelled"
            # Native model/tool loop: detached call + sibling must close the whole
            # accepted batch before another model turn; completing the run leaves
            # the same background Job alive.
            native_gate = node.work / "native-release"
            node.send(t="input", sid="native", run_id="native-run", owner_agent="test-agent", mode="auto",
                      sandbox_mode="off", workspaces=[dict(name="job-test:"+native(node.work),
                      dirs=[native(node.work)], ro=[], rw=[], local=True)], **{"from":"tg:owner"})
            node.wait(lambda m: m["t"] == "model" and m.get("sid") == "native")
            node.send(t="turn", sid="native", run_id="native-run", turn_id=1, reason="tool_use", calls=[
                dict(id="background", name="Shell", args=dict(command=shell(
                    f"while [ ! -f '{native_gate}' ]; do sleep 0.02; done; printf native-done"),
                    execution="background")),
                dict(id="sibling", name="Read", args=dict(path="launches"))])
            native_results = {}
            while len(native_results) < 2:
                result = node.wait(lambda m: m["t"] == "result" and m.get("sid") == "native")
                native_results[result["id"]] = result
            native_id = json.loads(native_results["background"]["content"])["job_id"]
            assert native_results["sibling"]["ok"] and native_results["sibling"]["content"].rstrip().endswith("once")
            node.wait(lambda m: m["t"] == "model" and m.get("sid") == "native")
            node.send(t="turn", sid="native", run_id="native-run", turn_id=2, reason="final")
            node.wait(lambda m: m["t"] == "done" and m.get("sid") == "native")
            assert node.control("native-still-live", "status", native_id)["job"]["background"]
            native_gate.touch()
            assert node.terminal(native_id)["status"] == "succeeded"

            # Keep one real process and one queued background request at shutdown.
            # Recovery must expose uncertainty/interruption rather than replay either.
            restart_gate = node.work / "restart-never-release"
            node.run("restart-live", f"echo started >> restart-launches; while [ ! -f '{restart_gate}' ]; do sleep 0.02; done",
                     execution="background")
            restart_live = node.result("restart-live")["job_id"]
            deadline = time.monotonic() + 8
            while not node.control("restart-pid", "status", restart_live)["job"].get("pid"):
                assert time.monotonic() < deadline
                time.sleep(0.01)
            node.run("restart-queued", "echo BAD > must-not-run-after-restart", execution="background")
            restart_queued = node.result("restart-queued")["job_id"]
            # Repeated terminal event ack is persisted; a real node restart never replays.
            node.send(t="events_ack", job_id=queued_id, event_seq=1)
            node.wait(lambda m: m["t"] == "job_state" and m.get("kind") == "ack" and m.get("job_id") == queued_id)
            node.stop()
            assert node.process.returncode == 0
            node.conn.close()
            old_epoch = node.epoch
            node.launch()
            assert node.epoch != old_epoch
            restored = node.control("rebuild", "status", queued_id)["job"]
            assert restored["status"] == "succeeded" and restored["notification_state"] == "delivered" and restored["output_tail"] == "queued-done", restored
            assert (node.work / "launches").read_text() == "once"
            live = node.control("rebuild-live", "status", restart_live)["job"]
            queued = node.control("rebuild-queued", "status", restart_queued)["job"]
            assert live["status"] == "unknown" and queued["status"] == "interrupted", (live,queued)
            assert live["last_event_seq"] == queued["last_event_seq"] == 1
            inventory=node.wait(lambda m: m["t"]=="lease_sync" and m["node_epoch"]==node.epoch,past=True)
            held=[item for item in inventory["leases"] if item.get("job_id")==restart_live]
            completed=[item for item in inventory["lease_receipts"] if item.get("job_id")==restart_queued]
            assert len(held)==len(completed)==1 and held[0]["state"]=="unknown" and held[0]["background"]
            assert completed[0]["state"]=="interrupted" and completed[0]["terminal_receipt"]
            assert (node.work / "restart-launches").read_text().splitlines() == ["started"]
            assert not (node.work / "must-not-run-after-restart").exists()
            for ident in ["fore", "queued", "block", "auto", "denied", "race", "reconnect", "cancel-block", "cancel-queued", "manual", "stop-running", "restart-live", "restart-queued"]:
                results = [m for m in node.history if m["t"] == "run_result" and m["id"] == ident]
                assert len(results) == 1, (ident, results)
            print("node jobs: 13 host requests + native 2-call batch; queued/auto/handoff race/cancel/reconnect/rebuild/one result passed")
        except BaseException:
            print("JOB NODE HISTORY", json.dumps(node.history[-30:], ensure_ascii=False, indent=2), file=sys.stderr)
            print((node.root / "node.log").read_text()[-8000:], file=sys.stderr)
            raise
        finally:
            node.close()


def invalid_protocol():
    if WINDOWS:
        return  # Pure decoder fixtures run on both platforms; this host fixture uses a POSIX shebang.
    for mode in ["duplicate", "truncated", "missing"]:
        with tempfile.TemporaryDirectory(prefix="na-invalid-job-") as root:
            executor = Path(root) / "fixture-exec"
            executor.write_text("""#!/usr/bin/env python3
import json, sys
if '--probe' in sys.argv:
    print(json.dumps(dict(landlock=0,rg=False,job_stream=1)))
    sys.exit()
req=json.loads(sys.stdin.readline())
job_id=req['job_id']
mode=req['args']['command']
start=dict(t='started',seq=1,job_id=job_id,pid=1,sandbox_effective='none')
end=dict(t='exit',seq=2,job_id=job_id,ok=True,content='untrusted success',
         exit_code=0,signal=0,timed_out=False,cancelled=False)
print(json.dumps(start),flush=True)
if mode!='missing': print(json.dumps(end),flush=True)
if mode=='duplicate': print(json.dumps(end),flush=True)
if mode=='truncated': print('{',end='',flush=True)
""")
            executor.chmod(0o700)
            node = Node(root, executor)
            try:
                node.run("invalid-"+mode, mode)
                result = node.result("invalid-"+mode)
                assert not result["ok"], result
                state = node.control("inspect-invalid", "status", result["job_id"])["job"]
                assert state["status"] == "unknown", state
                assert state.get("result",{}).get("ok") is not True, state
            finally:
                node.close()
    print("node jobs: duplicate/truncated/missing terminal cannot publish successful completion")


if __name__ == "__main__":
    check()
    invalid_protocol()
