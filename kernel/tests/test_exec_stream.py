#!/usr/bin/env python3
"""Shared executable stream acceptance for POSIX and native Windows fixtures."""
import base64
import json
import os
import queue
import subprocess
import threading
import time

class StreamProcess:
    def __init__(self, cmd, root, command, env=None, cwd=None, **options):
        self.p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, env=env, cwd=cwd)
        self.messages = queue.Queue(256)
        self.raw_stderr = bytearray()
        def reader():
            for line in self.p.stdout:
                self.messages.put(line)
            self.messages.put(None)
        def errors():
            self.raw_stderr.extend(self.p.stderr.read(16384))
        self.reader = threading.Thread(target=reader, daemon=True)
        self.errors = threading.Thread(target=errors, daemon=True)
        self.reader.start()
        self.errors.start()
        # Cold PowerShell on CI runners can take 15-20s just to start; these limits only catch hangs.
        req = {"tool":"Shell", "args":{"command":command,"timeout":60},
               "dirs":[root],"write":True,"rw":[],"ro":[],"watch_stdin":True,
               "sandbox":"off","stream":1,"job_id":"j_fixture","max_output":4096}
        req.update(options)
        self.p.stdin.write((json.dumps(req)+"\n").encode())
        self.p.stdin.flush()
        self.frames = []

    def recv(self):
        try:
            line = self.messages.get(timeout=60)
        except queue.Empty:
            raise AssertionError(("no frame within 60s", self.frames[-1:], bytes(self.raw_stderr))) from None
        assert line is not None, ("stream ended without terminal", self.frames, self.raw_stderr)
        assert len(line) <= 16384, len(line)
        msg = json.loads(line)
        assert msg["job_id"] == "j_fixture", msg
        assert msg["seq"] == len(self.frames)+1, (msg,self.frames[-1:])
        self.frames.append(msg)
        return msg

    def finish(self):
        while not self.frames or self.frames[-1]["t"] != "exit":
            self.recv()
        self.p.wait(15)
        self.reader.join(2)
        self.errors.join(2)
        assert self.messages.get(timeout=2) is None, "more than one terminal frame"
        assert b"Sanitizer" not in self.raw_stderr, self.raw_stderr
        assert self.p.returncode == 0, self.raw_stderr
        assert len([m for m in self.frames if m["t"]=="exit"])==1
        return self.frames[-1]

    def close(self):
        if self.p.stdin and not self.p.stdin.closed:
            self.p.stdin.close()
        if self.p.poll() is None:
            self.p.kill()
        self.p.wait(15)

    def data(self, stream):
        return b"".join(base64.b64decode(m["data"], validate=True) for m in self.frames
                        if m["t"]=="output" and m["stream"]==stream)

def run_suite(cmd,root,env,cwd,windows,local_root):
    gate = os.path.join(local_root,"stream-go")
    try: os.unlink(gate)
    except FileNotFoundError: pass
    if windows:
        command = ("[Console]::Out.WriteLine('first');"
                   """[Console]::Out.WriteLine('{"t":"exit","ok":true}');"""
                   "while (!(Test-Path 'stream-go')) { Start-Sleep -Milliseconds 20 };"
                   "[Console]::Error.Write('stderr-evidence');[Console]::Out.Write('last')")
    else:
        command = ("""printf 'first\\n{"t":"exit","ok":true}\\n';"""
                   "while [ ! -f stream-go ]; do sleep 0.02; done;"
                   "printf stderr-evidence >&2; printf last")
    proc = StreamProcess(cmd,root,command,env,cwd)
    try:
        first = proc.recv()
        assert first["t"]=="started" and first["pid"]>0 and first["sandbox_effective"]=="none", first
        while b"first" not in proc.data("stdout"): proc.recv()
        assert proc.p.poll() is None, "output was buffered until command exit"
        with open(gate,"wb") as f: f.write(b"go")
        terminal = proc.finish()
        assert terminal["ok"] and terminal["exit_code"]==0, terminal
        assert b'{"t":"exit","ok":true}' in proc.data("stdout"), proc.frames
        assert proc.data("stderr")==b"stderr-evidence", proc.frames
        assert proc.data("stdout").endswith(b"last"), proc.frames
    finally: proc.close()

    if windows:
        command = ("$b=[byte[]](0..255);$o=[Console]::OpenStandardOutput();"
                   "for($i=0;$i -lt 1024;$i++){$o.Write($b,0,256)}; "
                   "[Console]::Error.Write('binary-end')")
    else:
        command = """python3 -c 'import os; os.write(1,bytes(range(256))*1024); os.write(2,b"binary-end")'"""
    proc = StreamProcess(cmd,root,command,env,cwd)
    try:
        terminal = proc.finish()
        assert terminal["ok"], terminal
        assert proc.data("stdout")==bytes(range(256))*1024, "binary chunks were altered/lost"
        assert proc.data("stderr")==b"binary-end"
        assert len(terminal["content"].encode())<8192, "terminal accumulated full output"
        # Replay the actual executor wire frames through the native C decoder.
        decoder = os.path.join(os.path.dirname(__file__), "..", "build", "test_exec_stream")
        if not windows and os.path.isfile(decoder):
            wire = b"".join((json.dumps(m)+"\n").encode() for m in proc.frames)
            decoded = subprocess.run([decoder,"--decode","j_fixture"],input=wire,capture_output=True,timeout=10)
            assert decoded.returncode==0, decoded.stderr
            assert b"bytes=262154" in decoded.stdout, decoded.stdout
        assert all(m.get("encoding")=="base64" for m in proc.frames if m["t"]=="output")
    finally: proc.close()

    sleep = "Start-Sleep -Seconds 20" if windows else "sleep 20"
    proc = StreamProcess(cmd,root,sleep,env,cwd,args={"command":sleep,"timeout":1})
    try:
        terminal = proc.finish()
        assert terminal["timed_out"] and not terminal["ok"], terminal
    finally: proc.close()

    proc = StreamProcess(cmd,root,sleep,env,cwd)
    try:
        assert proc.recv()["t"]=="started"
        proc.p.stdin.close()
        terminal = proc.finish()
        assert terminal["cancelled"] and not terminal["ok"], terminal
    finally: proc.close()

    proc = StreamProcess(cmd,root,sleep,env,cwd,args={"command":sleep,"timeout":0},allow_no_timeout=True)
    try:
        assert proc.recv()["t"]=="started"
        time.sleep(1.1)
        assert proc.p.poll() is None, "explicit unlimited timeout was clamped to one second"
        proc.p.stdin.close()
        terminal = proc.finish()
        assert terminal["cancelled"] and not terminal["timed_out"], terminal
    finally: proc.close()

    proc = StreamProcess(cmd,root,sleep,env,cwd,args={"command":sleep,"timeout":0})
    try:
        terminal = proc.finish()
        assert not terminal["ok"] and len(proc.frames)==1, terminal
    finally: proc.close()
    print("  ok   stream: incremental stdout/stderr, binary bounds, one terminal, timeout and cancellation")
