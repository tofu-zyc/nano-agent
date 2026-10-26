#!/usr/bin/env python3
"""Linux 真桌面驱动验证：独立输入框 → 截图 → 中文输入 → 点击确定 → 核对进程输出。

用 --isolated-x11 在私有 Xvfb/DBus 会话里验证，不触碰用户现有窗口。
也可在已有图形会话直接运行。任何截图/输入失败都会失败，不用 skip 代替通过。
需要 Python Pillow、GI/GTK3；隔离桌面需要 Xvfb、xauth、DBus，可搭配 Openbox、xcompmgr。
"""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import select
import shutil
import struct
import subprocess
import sys
import tempfile
import time

from PIL import Image
import io


class MCP:
    def __init__(self, driver, env, log):
        self.proc = subprocess.Popen([str(driver), "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=log, env=env)
        self.seq, self.buffer = 0, b""
        initialized = self.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                "clientInfo": {"name": "nano-agent-desktop-test", "version": "1"}})
        self.version = initialized.get('serverInfo', {}).get('version', '')
        self.proc.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
        self.proc.stdin.flush()

    def rpc(self, method, params):
        self.seq += 1
        self.proc.stdin.write((json.dumps({"jsonrpc": "2.0", "id": self.seq, "method": method, "params": params}) + "\n").encode())
        self.proc.stdin.flush()
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            if b"\n" not in self.buffer:
                if not select.select([self.proc.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                    break
                chunk = os.read(self.proc.stdout.fileno(), 1 << 20)
                if not chunk:
                    raise EOFError("Driver disconnected")
                self.buffer += chunk
                continue
            line, self.buffer = self.buffer.split(b"\n", 1)
            msg = json.loads(line)
            if msg.get("id") == self.seq:
                if "error" in msg:
                    raise AssertionError(msg["error"])
                return msg["result"]
        raise TimeoutError(method)

    def call(self, name, args):
        result = self.rpc("tools/call", {"name": name, "arguments": args})
        if result.get("isError"):
            raise AssertionError(f"{name}: {result}")
        return result

    def close(self):
        self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.terminate()
            self.proc.wait(timeout=5)


def stop(proc):
    if proc is not None and proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--driver", type=Path, required=True)
    parser.add_argument("--isolated-x11", action="store_true")
    parser.add_argument("--window-manager", type=Path, help="隔离 X11 的窗口管理器（openbox 或 twm）")
    parser.add_argument("--compositor", type=Path, help="隔离 X11 的合成器（xcompmgr，供透明光标窗口使用）")
    parser.add_argument("--allow-desktop-capture", action="store_true", help="允许在窗口截图不可用时捕获整个测试桌面")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--gateway", type=Path, help="经真实 nano-agent 网关和内核执行自主桌面任务")
    parser.add_argument("--kernel", type=Path)
    parser.add_argument("--executor", type=Path)
    opts = parser.parse_args()
    root = opts.output or Path(tempfile.mkdtemp(prefix="nano-desktop-e2e-"))
    root = root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    (root / 'report.json').write_text(json.dumps({'ok': False, 'status': 'running'}))
    env = os.environ.copy()
    if opts.isolated_x11:
        for command in ("Xvfb", "xauth", "dbus-run-session"):
            if not shutil.which(command):
                raise SystemExit("缺少 " + command)
        display = next(n for n in range(100, 300) if not Path(f"/tmp/.X{n}-lock").exists() and not Path(f"/tmp/.X11-unix/X{n}").exists())
        authority = root / "Xauthority"
        authority.touch(mode=0o600)
        subprocess.run(["xauth", "-f", str(authority), "add", f":{display}", "MIT-MAGIC-COOKIE-1", secrets.token_hex(16)], check=True)
        env.update(DISPLAY=f":{display}", XAUTHORITY=str(authority), XDG_SESSION_TYPE="x11", GDK_BACKEND="x11")
        for key in ("WAYLAND_DISPLAY", "CUA_DRIVER_RS_ENABLE_WAYLAND", "DBUS_SESSION_BUS_ADDRESS"):
            env.pop(key, None)
        with (root / "xvfb.log").open("w") as log:
            xserver = subprocess.Popen(["Xvfb", f":{display}", "-screen", "0", "1280x800x24", "-nolisten", "tcp", "-auth", str(authority)],
                                       cwd=root, stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 5
                while not Path(f"/tmp/.X11-unix/X{display}").exists():
                    if xserver.poll() is not None or time.monotonic() > deadline:
                        raise AssertionError("Xvfb did not start")
                    time.sleep(.05)
                manager = None
                compositor = None
                wm_bin = shutil.which("openbox") or shutil.which("twm")
                wm = opts.window_manager or (Path(wm_bin) if wm_bin else None)
                with (root / "session.log").open("w") as session_log:
                    try:
                        if wm:
                            if wm.name == "openbox":
                                rc = root / "openbox.xml"
                                rc.write_text('<openbox_config xmlns="http://openbox.org/3.4/rc"><theme><name>Clearlooks</name></theme></openbox_config>')
                                command = [str(wm), "--sm-disable", "--config-file", str(rc)]
                            else:
                                rc = root / "twmrc"
                                rc.write_text('RandomPlacement\nTitleFont "fixed"\nMenuFont "fixed"\nIconFont "fixed"\n')
                                command = [str(wm), "-f", str(rc)]
                            manager = subprocess.Popen(command, env=env, cwd=root, stdout=session_log, stderr=session_log)
                            time.sleep(.4)
                        comp = opts.compositor or (Path(shutil.which("xcompmgr")) if shutil.which("xcompmgr") else None)
                        if comp:
                            compositor = subprocess.Popen([str(comp)], env=env, cwd=root, stdout=session_log, stderr=session_log)
                            time.sleep(.2)
                        child = [sys.executable, str(Path(__file__).resolve()), '--driver', str(opts.driver.resolve()), '--output', str(root), '--allow-desktop-capture']
                        if opts.gateway:
                            if not opts.kernel or not opts.executor: parser.error('--gateway 需要 --kernel 和 --executor')
                            child += ['--gateway', str(opts.gateway.resolve()), '--kernel', str(opts.kernel.resolve()), '--executor', str(opts.executor.resolve())]
                        result = subprocess.run(['dbus-run-session', '--'] + child, env=env, stderr=session_log)
                    finally:
                        stop(compositor)
                        stop(manager)
                if result.returncode:
                    print((root / "session.log").read_text()[-1800:], file=sys.stderr)
                return result.returncode
            finally:
                stop(xserver)
    fixture = root / "fixture.py"
    fixture.write_text('''import gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk
window = Gtk.Window(title="Nano桌面操作验证")
window.set_default_size(440, 180)
box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16, margin=20)
box.add(Gtk.Label(label="独立验证窗口"))
entry = Gtk.Entry()
box.add(entry)
button = Gtk.Button(label="确定")
button.connect("clicked", lambda *_: (print(entry.get_text(), flush=True), Gtk.main_quit()))
box.add(button)
window.add(box)
window.connect("destroy", Gtk.main_quit)
window.show_all()
Gtk.main()
''')
    env.update(GDK_BACKEND="x11", GSK_RENDERER="cairo", LIBGL_ALWAYS_SOFTWARE="1")
    driver, ui, output_file = None, None, None
    session = "nano-test-" + secrets.token_hex(5)
    checks = []
    with (root / "driver.log").open("w") as log, (root / "app.log").open("w") as app_log:
        try:
            driver = MCP(opts.driver.resolve(), env, log)
            schemas = {t['name']: t['inputSchema'] for t in driver.rpc('tools/list', {}).get('tools', [])}
            driver.call("start_session", {"session": session})
            output_path = root / 'application-output.txt'
            output_file = output_path.open('w') if opts.gateway else None
            ui = subprocess.Popen([sys.executable, str(fixture)],
                                  env=env, stdout=output_file if output_file else subprocess.PIPE, stderr=app_log, text=True)
            deadline, windows = time.monotonic() + 10, []
            while time.monotonic() < deadline:
                rows = driver.call("list_windows", {"pid": ui.pid, "on_screen_only": True}).get("structuredContent", {}).get("windows", [])
                windows = [w for w in rows if w.get("width", 0) > 20 and w.get("height", 0) > 20]
                if windows:
                    break
                time.sleep(.2)
            if not windows:
                raise AssertionError("Driver did not find the test window")
            args = {"pid": ui.pid, "window_id": windows[0]["window_id"], "session": session}
            tree_args = {"tree_format": "both"} if "tree_format" in schemas.get('get_window_state', {}).get('properties', {}) else {}
            time.sleep(.5)
            if shutil.which("xwininfo"):
                info = subprocess.run(["xwininfo", "-id", str(args["window_id"])], env=env, capture_output=True, text=True, timeout=5)
                (root / "x-window-info.txt").write_text(info.stdout + info.stderr)
            state = driver.call("get_window_state", dict(args, **tree_args, include_screenshot=True))
            (root / "window-state.json").write_text(json.dumps(state, ensure_ascii=False))
            images = [c for c in state.get("content", []) if c.get("type") == "image"]
            if not images:
                if not opts.allow_desktop_capture:
                    raise AssertionError("窗口截图不可用；可用 --isolated-x11 在独立测试桌面验证桌面截图")
                driver.call("set_config", {"session": session, "capture_scope": "desktop"})
                desktop = driver.call("get_desktop_state", {"session": session})
                (root / "desktop-state.json").write_text(json.dumps(desktop))
                images = [c for c in desktop.get("content", []) if c.get("type") == "image"]
                if not images:
                    raise AssertionError("No screenshot returned: " + json.dumps(desktop)[:1200])
            png = base64.b64decode(images[0]["data"])
            assert png[:8] == b"\x89PNG\r\n\x1a\n" and min(struct.unpack(">II", png[16:24])) > 10
            (root / "window.png").write_bytes(png)
            picture = Image.open(io.BytesIO(png)).convert("RGB")
            assert any(low != high for low, high in picture.getextrema()), "截图是纯色，不能证明界面已渲染"
            checks.append("发现独立测试窗口并取得真实 PNG 截图")
            elements = state["structuredContent"]["elements"]
            entry = next(e for e in elements if e["role"] in ("text box", "entry", "text"))
            expected = "中文验证：你好 nano 42"
            if opts.gateway:
                combined = subprocess.run([sys.executable, str(Path(__file__).with_name('e2e-autonomy.py')),
                    '--gateway', str(opts.gateway), '--kernel', str(opts.kernel), '--executor', str(opts.executor),
                    '--desktop-driver', str(opts.driver), '--desktop-pid', str(ui.pid), '--desktop-window', str(args['window_id']),
                    '--desktop-output', str(output_path), '--report', str(root/'gateway-report.json')], env=env)
                assert combined.returncode == 0, '自主桌面组合验证失败'
                ui.wait(timeout=5)
                output_file.close()
                assert ui.returncode == 0 and output_path.read_text().strip() == expected
                (root / 'report.json').write_text(json.dumps({'ok': True, 'driver_version': driver.version,
                    'mode': 'autonomous-gateway', 'application_output': output_path.read_text().strip(),
                    'checks': checks + ['真网关自主任务经 MCP 操作并验收']}, ensure_ascii=False, indent=2))
                print('ok 真实应用输出已核对，桌面动作由自主任务经网关完成', flush=True)
                return 0
            input_method = 'set_value' if 'set_value' in schemas else 'type_text'
            typed = driver.call(input_method, dict(args, element_token=entry["element_token"],
                                **({'value': expected} if input_method == 'set_value' else {'text': expected}), delivery_mode="foreground"))
            (root / "typing-result.json").write_text(json.dumps(typed, ensure_ascii=False))
            time.sleep(.7)
            state = driver.call("get_window_state", dict(args, **tree_args, include_screenshot=False))
            (root / "window-after.json").write_text(json.dumps(state, ensure_ascii=False))
            button = next(e for e in state["structuredContent"]["elements"]
                          if e["role"] == "button" and ("确定" in e.get("label", "") or "OK" in e.get("label", "")))
            driver.call("click", dict(args, element_token=button["element_token"]))
            output, _ = ui.communicate(timeout=10)
            assert ui.returncode == 0 and expected == output.strip(), f"真实应用输出与预期不符：{output!r}，exit={ui.returncode}"
            checks.append("通过驱动输入中文并点击确定，应用进程输出核对成功")
            for check in checks:
                print("ok", check, flush=True)
            (root / "report.json").write_text(json.dumps({"ok": True, "driver_version": driver.version, "input_method": input_method,
                "checks": checks, "application_output": output.strip()}, ensure_ascii=False, indent=2))
            print("验证产物", root, flush=True)
            return 0
        finally:
            stop(ui)
            if output_file is not None and not output_file.closed: output_file.close()
            if driver is not None:
                try:
                    driver.call("end_session", {"session": session})
                finally:
                    driver.close()


if __name__ == "__main__":
    raise SystemExit(main())
