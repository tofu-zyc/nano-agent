#!/usr/bin/env python3
"""一个最小的 MCP 服务（stdio，一行一条 JSON-RPC），测试透传用。"""
import json, sys

def send(o):
    sys.stdout.write(json.dumps(o) + '\n')
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        m = json.loads(line)
    except Exception:
        continue
    method = m.get('method')
    mid = m.get('id')
    if method == 'initialize':
        send({'jsonrpc': '2.0', 'id': mid, 'result': {
            'protocolVersion': (m.get('params') or {}).get('protocolVersion') or '2025-06-18',
            'capabilities': {'tools': {}},
            'serverInfo': {'name': 'echo', 'version': '0'}}})
    elif method == 'tools/list':
        send({'jsonrpc': '2.0', 'id': mid, 'result': {'tools': [{
            'name': 'echo', 'description': '原样返回',
            'inputSchema': {'type': 'object', 'properties': {'text': {'type': 'string'}}, 'required': ['text']}}]}})
    elif method == 'tools/call':
        args = (m.get('params') or {}).get('arguments') or {}
        send({'jsonrpc': '2.0', 'id': mid, 'result': {'content': [{'type': 'text', 'text': 'echo:' + str(args.get('text', ''))}]}})
    elif method and method.startswith('notifications/'):
        pass
    elif mid is not None:
        send({'jsonrpc': '2.0', 'id': mid, 'error': {'code': -32601, 'message': '不支持 ' + str(method)}})