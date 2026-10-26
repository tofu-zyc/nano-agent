#!/usr/bin/env python3
"""有界真实模型验收：创建日程、真实执行、检查点、网关重启、续做与证据验收。

显式指定 --source-home 读取已有默认模型；只将所需的加密端点及密钥暂存到私有目录，
测试结束删除副本。不会复制聊天、工作目录、机器人或 MCP 配置，也不会替换正式网关。
会消耗所选模型的 API 用量；默认最多 16 步/轮、每轮 2 分钟、两次自主执行。
提供完整 --desktop-* 参数时改测一轮远端桌面任务，通常由 e2e-desktop-win.py 启动。
"""
import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import signal
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    def interrupted(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gateway', type=Path, required=True)
    parser.add_argument('--kernel', type=Path, required=True)
    parser.add_argument('--executor', type=Path, required=True)
    parser.add_argument('--source-home', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--skills-dir', type=Path, help='待验证的技能源目录；测试技能发现、加载及实际执行')
    parser.add_argument('--kernel-listen')
    parser.add_argument('--desktop-driver')
    parser.add_argument('--desktop-machine')
    parser.add_argument('--desktop-workspace')
    parser.add_argument('--desktop-pid', type=int)
    parser.add_argument('--desktop-window', type=int)
    parser.add_argument('--desktop-output')
    parser.add_argument('--desktop-driver-arg', action='append', default=[])
    opts = parser.parse_args()
    opts.report.parent.mkdir(parents=True, exist_ok=True)
    opts.report.write_text(json.dumps({'ok': False, 'status': 'running'}))
    source = opts.source_home.resolve()
    source_models = json.loads((source/'data/models.json').read_text())
    model = next(m for m in source_models['models'] if m['id'] == source_models['default'])
    endpoint = next(e for e in source_models['endpoints'] if e['id'] == model['endpoint'])
    assert endpoint['kind'] != 'mock', '默认模型是假模型，不能用于真实模型验收'
    if opts.desktop_driver:
        if not all((opts.desktop_machine, opts.desktop_workspace, opts.desktop_pid, opts.desktop_window, opts.desktop_output)):
            parser.error('真实桌面验收需要完整的 --desktop-* 目标参数')
        assert 'image' in model.get('modalities', []), '真实桌面验收需要已配置支持图片的模型'
    # 只保存这一次需要的端点，不复制其他服务的凭据。
    isolated_models = {'endpoints': [endpoint], 'models': [dict(model, max_tokens=4096)],
                       'default': model['id'], 'seeded': True}
    root = Path(tempfile.mkdtemp(prefix='nano-autonomy-real-'))
    nano, work = root/'nano', root/'work'
    for path in (nano/'lib', nano/'data', nano/'personas', work):
        path.mkdir(parents=True, mode=0o700)
    if opts.skills_dir:
        shutil.copytree(opts.skills_dir.resolve(), nano/'skills')
    expected = '自主执行挑战：' + secrets.token_hex(12) + ' 中文与 nano 42'
    (work/'input.txt').write_text(expected, encoding='utf-8')
    for path, name in ((opts.kernel, 'nano-kernel'), (opts.executor, 'nano-exec')):
        (nano/'lib'/name).symlink_to(path.resolve())
    # 仅向目标模型提交本测试的合成任务；不带生产人格或历史。
    (nano/'personas/default.md').write_text('你是 nano-agent 的执行助手。按用户要求用工具完成任务，如实说明结果。不要把计划或工具调用成功当成任务已验收。', encoding='utf-8')
    web_port = free_port()
    config = nano/'gateway.toml'
    kernel_listen = opts.kernel_listen or f'unix:{root}/kernel.sock'
    config.write_text(f'''[kernel]
listen = {json.dumps(kernel_listen)}
token = "isolated-real-e2e"
[web]
listen = "127.0.0.1:{web_port}"
password = "isolated-real-e2e"
[cli]
enabled = false
[agent]
max_steps = 16
run_timeout = "2m"
[session]
timezone = "UTC"
[search]
enabled = false
[workspace]
mode = "ask"
''')
    env = {k: v for k, v in os.environ.items() if not k.startswith('NANO_')}
    env['NANO_HOME'] = str(nano)
    base = f'http://127.0.0.1:{web_port}'
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    proc = None
    credential_files = [nano/'secret.key', nano/'data/models.json']
    log = (root/'gateway.log').open('a')

    def api(path, data=None, method=None):
        request = urllib.request.Request(base+path, data=None if data is None else json.dumps(data).encode(), method=method,
                                         headers={'Content-Type': 'application/json', 'X-Nano': '1'})
        try:
            with opener.open(request, timeout=5) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise AssertionError(f'{path}: {error.code} {error.read().decode()}') from error

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
        proc = subprocess.Popen([str(opts.gateway.resolve()), '-c', str(config)], cwd=root,
                                env=env, stdout=log, stderr=log, start_new_session=True)
        until = time.monotonic() + 20
        while time.monotonic() < until:
            assert proc.poll() is None, '临时网关启动失败，见 ' + str(root/'gateway.log')
            try:
                api('/api/login', {'password': 'isolated-real-e2e'})
                if api('/api/overview').get('kernels'):
                    return
            except (OSError, AssertionError):
                pass
            time.sleep(.15)
        raise AssertionError('临时网关启动超时')

    def wait(predicate, description, timeout=160):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            assert proc.poll() is None, '临时网关意外退出'
            for approval in api('/api/overview').get('approvals', []) or []:
                api('/api/approvals/'+str(approval['n']), {'ok': True})
            view = api('/api/agents/default/autonomy')
            value = predicate(view)
            if value:
                return view
            time.sleep(.3)
        raise AssertionError('等待超时：'+description+'；记录在 '+str(root))

    def file_test():
        criteria = ('这是分两轮的文件任务。没有检查点时：用 Read 读取 input.txt，将其中的完整文字原样保存到 first.txt，'
                    '重新读取 first.txt 核对，通过 Agenda progress 保存检查点并结束本轮，此时不要 done。'
                    '已有检查点时：读取 first.txt，把相同的完整文字原样保存到 final.txt，再 Read final.txt 核对，'
                    '只在一致时用 Agenda done 提交此次真实 Read 的工具调用 ID 作为 evidence。不得修改 input.txt 或重写 first.txt。')
        lead = '先用 Skill 加载适合的任务技能，再' if opts.skills_dir else '请'
        api('/api/chat', {'sid': 'main', 'text': lead+'只创建一条已到期的 Agenda commitment，标题“真实模型自主续做验收”，when 填“2000-01-01 00:00”。'
            '把下面的整个操作约定原文写入 criteria。创建后立刻结束聊天这一轮，不要现在执行文件任务，等自主活动处理：'+criteria})
        first = wait(lambda v: len(v.get('executions', [])) >= 1, '真实模型第一次自主执行')
        assert len(first['agenda']) == 1 and first['agenda'][0].get('checkpoint'), '真实模型未保存检查点'
        assert not first['executions'][0].get('verified'), '第一轮不应该被验收完成'
        assert (work/'first.txt').read_text(encoding='utf-8').strip() == expected
        assert not (work/'final.txt').exists(), '第一轮越过了约定的检查点'
        first_stat = (work/'first.txt').stat()
        agenda = first['agenda'][0]['id']
        stop()
        start()
        api('/api/agents/default/autonomy', {'action': 'move_agenda', 'id': agenda, 'when': '2000-01-01 00:00'})
        final = wait(lambda v: len(v.get('executions', [])) >= 2, '重启后续做并验收')
        assert final['executions'][0].get('verified'), '真实模型第二轮未通过证据验收'
        assert (work/'final.txt').read_text(encoding='utf-8').strip() == expected
        assert (work/'first.txt').stat().st_mtime_ns == first_stat.st_mtime_ns, '续做重写了已完成的文件'
        execution = final['executions'][0]
        assert any(e['name'] == 'Read' and e.get('verified') for e in execution.get('evidence', []))
        if opts.skills_dir:
            history = [json.loads(line) for line in (nano/'data/sessions/main.jsonl').read_text().splitlines()]
            names = [json.loads(c['function']['arguments']).get('name') for m in history for c in m.get('tool_calls', []) if c['function']['name'] == 'Skill']
            assert 'autonomous-tasks' in names, '真实模型没有加载自主任务技能'
        return {'ok': True, 'model': model['name'], 'directory': str(root), 'execution': execution,
                  'autonomy_tokens': sum(e.get('tokens', 0) for e in final['executions']),
                  'checks': ['真实模型自己创建日程，审批后读取未知挑战文本并写文件',
                             '保存检查点后停止，第一轮没有冒充验收完成',
                             '网关重启后从检查点续做，不重写已完成文件',
                             '第二轮重新读取最终文件，以真实 Read 回执验收；测试端独立核对内容']}

    def desktop_test():
        api('/api/mcp', {'items': [{'name': 'desktop', 'machine': opts.desktop_machine, 'command': opts.desktop_driver,
             'args': ['mcp'] + opts.desktop_driver_arg, 'enabled': True, 'approval': 'per_turn'}]})
        connected = api('/api/mcp/test', {'name': 'desktop'})
        assert not connected.get('error'), connected
        criteria = (f'只操作本次独立测试窗口 Nano Agent Desktop Verification，pid={opts.desktop_pid}、window_id={opts.desktop_window}。'
                    '先用桌面工具 get_window_state 观察界面和截图，tree_format 用 both；用 set_value 将编辑框改为“中文验证：你好 nano 42”，'
                    '再次观察确认，再用 click 点击“确定”。每次使用最新的 element_token，不操作其他窗口。'
                    f'之后通过 Read 读取 {opts.desktop_output}，确认其中完整文字确实是“中文验证：你好 nano 42”，'
                    '最后 Agenda done，evidence 引用本轮实际 Read 结果开头的凭据 id。不得用文件写入代替真实桌面动作。')
        api('/api/chat', {'sid': 'main', 'text': '请只用 Agenda 创建一条已到期的 commitment，标题“真实模型桌面操作验收”，'
            'when 填“2000-01-01 00:00”，下面整段操作约定写入 criteria。创建后结束本轮聊天，等自主活动来做：'+criteria})
        final = wait(lambda v: len(v.get('executions', [])) >= 1, '真实模型桌面操作验收')
        execution = final['executions'][0]
        assert execution.get('verified'), '真实模型未能完成桌面任务验收'
        assert any(e['name'] == 'Read' and e.get('verified') and '中文验证：你好 nano 42' in e.get('result', '') for e in execution.get('evidence', []))
        history = [json.loads(line) for line in (nano/'data/sessions/@default%2Flife.jsonl').read_text().splitlines()]
        calls = [call['function']['name'] for message in history for call in message.get('tool_calls', [])]
        assert all(name in calls for name in ('desktop__get_window_state', 'desktop__set_value', 'desktop__click', 'Read', 'Agenda'))
        screenshots = [picture for message in history if message['role'] == 'tool' for picture in message.get('images', [])]
        assert screenshots, '桌面回执没有保存截图'
        picture = screenshots[-1]
        assert len(picture['id']) == 64 and all(c in '0123456789abcdef' for c in picture['id'])
        opts.report.with_suffix('.png').write_bytes((nano/'data/uploads'/picture['id']).read_bytes())
        return {'ok': True, 'model_kind': 'real', 'model': model['name'], 'directory': str(root),
                'screenshot_results': len(screenshots), 'execution': execution, 'autonomy_tokens': execution.get('tokens', 0),
                'checks': ['真实模型自行排期并经审批观察桌面、输入完整中英文、点击确定',
                           '通过 Windows 执行器回读应用输出，以真实 Read 凭据验收']}

    try:
        for target, content in ((credential_files[0], (source/'secret.key').read_bytes()),
                                (credential_files[1], json.dumps(isolated_models).encode())):
            with target.open('xb') as file:
                os.chmod(target, 0o600)
                file.write(content)
        start()
        tools = ['Read', 'Agenda', 'desktop__get_window_state', 'desktop__set_value', 'desktop__click'] if opts.desktop_driver else ['Read', 'Write', 'Agenda']
        if opts.skills_dir:
            tools.append('Skill')
        api('/api/agents/default', {'style': 'life', 'tools': tools, 'settings': {'mode': 'ask', 'life_awake': '00:00-00:01',
            'life_reach': 'off', 'life_budget': 150000, 'memory': 'off', 'dream_when': 'off'}}, 'PATCH')
        if opts.desktop_driver:
            wait(lambda _: any(k['node'] == opts.desktop_machine for k in api('/api/overview').get('kernels', [])), '远端测试内核连接', timeout=45)
        workspace = api('/api/workspaces', {'host': opts.desktop_machine or '本机', 'path': opts.desktop_workspace or str(work)})
        api('/api/sessions/main', {'workspaces': [workspace['id']], 'mode': 'ask'}, 'PATCH')
        report = desktop_test() if opts.desktop_driver else file_test()
    finally:
        stop()
        log.close()
        for path in credential_files:
            path.unlink(missing_ok=True)
        print('测试记录', root, flush=True)
    opts.report.write_text(json.dumps(report, ensure_ascii=False, indent=2))
    for check in report['checks']:
        print('ok', check, flush=True)


if __name__ == '__main__':
    main()
