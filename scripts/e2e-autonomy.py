#!/usr/bin/env python3
"""真网关 + 真 C 内核/执行器：自主日程、审批、检查点重启续做及失败验收。

模型用本机确定性的 OpenAI 兼容服务替身；配置、数据和产物全部在临时目录。
可在 Linux 目标机上用 --gateway/--kernel/--executor 指定待验证的程序。
"""
import argparse
import base64
import http.cookiejar
import http.server
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.error


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Model(http.server.BaseHTTPRequestHandler):
    phase = "checkpoint"
    work = None
    outside = None
    writes = 0
    requests = []
    seq = 0
    desktop = None
    image_requests = 0

    def log_message(self, *_):
        pass

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        type(self).requests.append(req)
        messages = req["messages"]
        if any(isinstance(m.get('content'), list) and any(p.get('type') == 'image_url' for p in m['content']) for m in messages):
            type(self).image_requests += 1
        # 工具截图会单独作为多模态用户消息追加，任务说明仍是前面的文字用户消息。
        start = max(i for i, m in enumerate(messages) if m["role"] == "user" and isinstance(m.get('content'), str))
        user = messages[start]["content"]
        later = messages[start + 1:]
        previous = [c for m in later for c in m.get("tool_calls", [])]
        last = previous[-1] if previous else None
        name = last["function"]["name"] if last else None
        args = None
        text = "安排好了"
        failure = "autonomy-e2e-failure" in user
        marker = "autonomy-e2e-desktop" if self.desktop else "autonomy-e2e-failure" if failure else "autonomy-e2e-success"
        current = re.search(r"当前日程 ID：(ag-\d+)", user)
        if current is None:
            if last is None:
                name, args = "Agenda", {"action": "add", "title": marker, "kind": "commitment",
                    "when": "2000-01-01 00:00", "criteria": "文件中确实保存了中文内容并重新读到"}
        elif self.desktop:
            pid, window, output = self.desktop
            target = {"pid": pid, "window_id": window}
            names = [c['function']['name'] for c in previous]
            agenda = current[1]
            if last is None or name == 'desktop__set_value':
                schema = next(t['function']['parameters'] for t in req['tools'] if t['function']['name'] == 'desktop__get_window_state')
                extras = {'tree_format': 'both'} if 'tree_format' in schema.get('properties', {}) else {}
                name, args = 'desktop__get_window_state', dict(target, **extras, include_screenshot=True)
            elif name == 'desktop__get_window_state':
                result = next(m['content'] for m in reversed(later) if m.get('tool_call_id') == last['id'])
                data, _ = json.JSONDecoder().raw_decode(result.split('[结构化结果]', 1)[1].lstrip())
                if 'desktop__set_value' not in names:
                    element = next(e for e in data['elements'] if e['role'].casefold() in ('text box', 'entry', 'edit', 'text'))
                    name, args = 'desktop__set_value', dict(target, element_token=element['element_token'], value='中文验证：你好 nano 42', delivery_mode='foreground')
                else:
                    element = next(e for e in data['elements'] if e['role'].casefold() == 'button' and '确定' in e.get('label', ''))
                    name, args = 'desktop__click', dict(target, element_token=element['element_token'])
            elif name == 'desktop__click':
                name, args = 'Read', {'path': str(output)}
            elif name == 'Read':
                result = next(m['content'] for m in reversed(later) if m.get('tool_call_id') == last['id'])
                if '中文验证：你好 nano 42' not in result:
                    raise AssertionError('桌面应用没有输出预期内容: ' + result)
                name, args = 'Agenda', {'action': 'done', 'id': agenda, 'note': '已从真实应用输出核对完整中英文，输入和点击均完成', 'evidence': [last['id']]}
            text = '桌面操作已按实际应用输出验收'
        else:
            agenda = current[1]
            if last is None:
                if failure or "上次的检查点" not in user:
                    name, args = "Write", {"path": str(self.outside if failure else self.work / "result.txt"), "content": "自主执行验证：中文内容正确\n"}
                    type(self).writes += 1
                else:
                    name, args = "Read", {"path": str(self.work / "result.txt")}
            elif name == "Write":
                if failure:
                    name, args = "Agenda", {"action": "done", "id": agenda, "note": "伪造完成应被拒绝", "evidence": [last["id"]]}
                else:
                    name, args = "Read", {"path": str(self.work / "result.txt")}
            elif name == "Read":
                if self.phase == "checkpoint":
                    name, args = "Agenda", {"action": "progress", "id": agenda, "checkpoint": "写入完成；下次重新读取核验，不要重复写入"}
                else:
                    name, args = "Agenda", {"action": "done", "id": agenda, "note": "已重新读到完整的中文内容，满足完成条件", "evidence": [last["id"]]}
            text = "本轮已执行，按检查点继续" if self.phase == "checkpoint" else "已经核验"
        message = {"role": "assistant", "content": text if args is None else ""}
        finish = "stop"
        if args is not None:
            type(self).seq += 1
            message["tool_calls"] = [{"index": 0, "id": "actual-" + str(self.seq), "type": "function",
                                      "function": {"name": name, "arguments": json.dumps(args, ensure_ascii=False)}}]
            finish = "tool_calls"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if req.get("stream") else "application/json")
        self.end_headers()
        if req.get("stream"):
            chunk = {"choices": [{"index": 0, "delta": message, "finish_reason": finish}],
                     "usage": {"prompt_tokens": 100, "completion_tokens": 20}}
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode())
        else:
            self.wfile.write(json.dumps({"choices": [{"message": message, "finish_reason": finish}]}).encode())


def main():
    def interrupted(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    source = Path(__file__).resolve().parent.parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gateway", type=Path, default=source / "gateway/nano-gateway")
    parser.add_argument("--kernel", type=Path, default=source / "kernel/nano-kernel")
    parser.add_argument("--executor", type=Path, default=source / "kernel/nano-exec")
    parser.add_argument("--kernel-listen", help="测试内核监听地址；远端内核可经 SSH 转发连接")
    parser.add_argument("--report", type=Path)
    parser.add_argument("--desktop-driver", help="Driver 在目标机器上的绝对路径")
    parser.add_argument("--desktop-machine", help="远端桌面所在的测试内核节点名")
    parser.add_argument("--desktop-workspace", help="远端桌面夹具输出所在的工作目录")
    parser.add_argument("--desktop-driver-arg", action="append", default=[], help="mcp 后附加的 Driver 参数")
    parser.add_argument("--desktop-pid", type=int)
    parser.add_argument("--desktop-window", type=int)
    parser.add_argument("--desktop-output")
    opts = parser.parse_args()
    if opts.report:
        opts.report.parent.mkdir(parents=True, exist_ok=True)
        opts.report.write_text(json.dumps({'ok': False, 'status': 'running'}))
    root = Path(tempfile.mkdtemp(prefix="nano-autonomy-e2e-"))
    nano, work = root / "nano", root / "work"
    for p in (nano / "lib", work):
        p.mkdir(parents=True)
    for path, name in ((opts.kernel, "nano-kernel"), (opts.executor, "nano-exec")):
        (nano / "lib" / name).symlink_to(path.resolve())
    Model.work, Model.outside = work, root / "must-not-exist.txt"
    if opts.desktop_driver:
        if not all((opts.desktop_pid, opts.desktop_window, opts.desktop_output)):
            parser.error('--desktop-driver 需要 --desktop-pid/--desktop-window/--desktop-output')
        if opts.desktop_machine and not opts.desktop_workspace:
            parser.error('--desktop-machine 需要 --desktop-workspace')
        Model.desktop = (opts.desktop_pid, opts.desktop_window, opts.desktop_output)
    model = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Model)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    config = nano / "gateway.toml"
    kernel_listen = opts.kernel_listen or f"unix:{root}/kernel.sock"
    config.write_text(f'''[kernel]
listen = {json.dumps(kernel_listen)}
token = "isolated-e2e"
[web]
listen = "127.0.0.1:{port}"
password = "isolated-e2e"
[cli]
enabled = false
[llm]
provider = "openai"
base_url = "http://127.0.0.1:{model.server_port}/v1"
api_key = "isolated-e2e"
model = "test"
modalities = ["text", "image"]
[session]
timezone = "UTC"
[search]
enabled = false
[workspace]
mode = "ask"
''')
    env = {k: v for k, v in os.environ.items() if not k.startswith("NANO_")}
    env["NANO_HOME"] = str(nano)
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    proc = None
    log = (root / "gateway.log").open("a")

    def api(path, data=None, method=None):
        req = urllib.request.Request(base + path, data=None if data is None else json.dumps(data).encode(),
                                     method=method, headers={"Content-Type": "application/json", "X-Nano": "1"})
        try:
            with opener.open(req, timeout=5) as r:
                return json.load(r)
        except urllib.error.HTTPError as err:
            raise AssertionError(f"{path}: {err.code} {err.read().decode()}") from err

    def wait(fn, description, timeout=30):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            if proc.poll() is not None:
                raise AssertionError("网关意外退出：" + (root / "gateway.log").read_text()[-3000:])
            value = fn()
            if value:
                return value
            time.sleep(.1)
        raise AssertionError("等待超时：" + description)

    def stop():
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)

    def start():
        nonlocal proc
        proc = subprocess.Popen([str(opts.gateway.resolve()), "-c", str(config)], cwd=root,
                                env=env, stdout=log, stderr=log, start_new_session=True)
        until = time.monotonic() + 20
        while time.monotonic() < until:
            try:
                api("/api/login", {"password": "isolated-e2e"})
                break
            except OSError:
                if proc.poll() is not None:
                    raise AssertionError((root / "gateway.log").read_text()[-3000:])
                time.sleep(.1)
        wait(lambda: api("/api/overview").get("kernels"), "本机内核连接")

    def mind():
        overview = api("/api/overview")
        for approval in overview.get("approvals", []) or []:
            api("/api/approvals/" + str(approval["n"]), {"ok": True})
        return api("/api/agents/default/autonomy")

    checks = []
    try:
        start()
        api("/api/agents/default", {"style": "life", "settings": {"mode": "ask", "life_awake": "00:00-24:00", "memory": "off", "dream_when": "off"}}, "PATCH")
        ws = api("/api/workspaces", {"host": "本机", "path": str(work)})
        api("/api/sessions/main", {"workspaces": [ws["id"]], "mode": "ask"}, "PATCH")
        if opts.desktop_driver:
            if opts.desktop_machine:
                wait(lambda: any(k.get('node') == opts.desktop_machine for k in api('/api/overview').get('kernels', [])), '远端测试内核连接', timeout=45)
                ws = api('/api/workspaces', {'host': opts.desktop_machine, 'path': opts.desktop_workspace})
                api('/api/sessions/main', {'workspaces': [ws['id']], 'mode': 'ask'}, 'PATCH')
            command = opts.desktop_driver if opts.desktop_machine else str(Path(opts.desktop_driver).resolve())
            api('/api/mcp', {'items': [{'name': 'desktop', 'machine': opts.desktop_machine or '',
                                      'command': command, 'args': ['mcp'] + opts.desktop_driver_arg,
                                      'enabled': True, 'approval': 'per_turn'}]})
            connected = api('/api/mcp/test', {'name': 'desktop'})
            assert not connected.get('error'), connected
            api('/api/chat', {'sid': 'main', 'text': 'autonomy-e2e-desktop'})
            view = wait(lambda: (v if (v := mind()).get('executions') and v['executions'][0].get('verified') else None), '自主桌面操作闭环', timeout=45)
            if not opts.desktop_machine:
                assert Path(opts.desktop_output).read_text().strip() == '中文验证：你好 nano 42'
            offered = [t['function']['name'] for r in Model.requests for t in r.get('tools', [])]
            assert 'desktop__set_value' in offered and 'desktop__click' in offered
            assert Model.image_requests > 0, '截图没有以多模态消息送到模型接口'
            report = {'ok': True, 'checks': ['真实网关与 C 内核到点启动自主活动，经 MCP 观察/截图/完整中英文输入/点击，读取应用输出并验收'],
                      'image_requests': Model.image_requests, 'directory': str(root),
                      'desktop_machine': opts.desktop_machine or '本机',
                      'execution': view['executions'][0]}
            if opts.report:
                opts.report.parent.mkdir(parents=True, exist_ok=True)
                picture = next(p['image_url']['url'] for r in Model.requests for m in r['messages']
                               if isinstance(m.get('content'), list) for p in m['content'] if p.get('type') == 'image_url')
                assert picture.startswith('data:image/'), '模型没有收到内嵌截图'
                opts.report.with_suffix('.png').write_bytes(base64.b64decode(picture.split(',', 1)[1], validate=True))
                opts.report.write_text(json.dumps(report, ensure_ascii=False, indent=2))
            print('ok', report['checks'][0], flush=True)
            return
        api("/api/chat", {"sid": "main", "text": "autonomy-e2e-success"})
        view = wait(lambda: (v if (v := mind()).get("executions") else None), "第一次执行与检查点")
        assert (work / "result.txt").read_text() == "自主执行验证：中文内容正确\n"
        assert view["agenda"][0]["checkpoint"] and not view["executions"][0]["verified"]
        agenda = view["agenda"][0]["id"]
        checks.append("真内核按审批模式写文件、重新读取、保存检查点，未验收不算完成")
        stop()
        Model.phase = "verify"
        start()
        api("/api/agents/default/autonomy", {"action": "move_agenda", "id": agenda, "when": "2000-01-01 00:00"})
        view = wait(lambda: (v if (v := mind())["executions"] and v["executions"][0].get("verified") else None), "重启后读取检查点并验收")
        assert Model.writes == 1, "重启后重复写入了已完成的步骤"
        assert view["executions"][0]["evidence"][0]["name"] == "Read"
        checks.append("网关重启后从持久检查点续做，未重复写入，完成关联真实读取证据")
        api("/api/sessions/main", {"mode": "auto"}, "PATCH")
        api("/api/chat", {"sid": "main", "text": "autonomy-e2e-failure"})
        view = wait(lambda: (v if len((v := mind())["executions"]) >= 3 else None), "越界失败与拒绝验收")
        assert not Model.outside.exists(), "执行器允许写出工作目录"
        assert not view["executions"][0]["verified"], "失败工具被错误验收"
        checks.append("写出工作目录被沙箱拒绝，失败调用不能用于验收")
        report = {"ok": True, "checks": checks, "directory": str(root), "model_requests": len(Model.requests)}
        if opts.report:
            opts.report.parent.mkdir(parents=True, exist_ok=True)
            opts.report.write_text(json.dumps(report, ensure_ascii=False, indent=2))
        for check in checks:
            print("ok", check, flush=True)
        print("验证目录", root)
    except Exception:
        print("失败记录", root, flush=True)
        print((root / "gateway.log").read_text()[-4000:], flush=True)
        raise
    finally:
        stop()
        model.shutdown()
        log.close()


if __name__ == "__main__":
    main()
