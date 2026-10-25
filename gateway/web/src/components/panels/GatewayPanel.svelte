<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { homeShort, num, pct, since, truncate } from '../../lib/format';
  import { homeOf, strayFields } from '../../lib/nodes';
  import type { FieldState } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import Field from '../Field.svelte';
  import Icon from '../Icon.svelte';
  import { api } from '../../lib/api';
  import type { MCPInfo } from '../../lib/types';

  // ---- MCP 服务（阶段 11）----
  type MCPDraft = {
    name: string;
    kind: 'stdio' | 'http';
    command: string;
    args: string;
    url: string;
    cwd: string;
    env: string;
    agents: string;
    enabled: boolean;
    approval: string;
    machine: string;
  };
  const mcps = $derived(store.ov?.mcp ?? []);
  // MCP 进程可以挂到任何一台在线内核的机器上跑（协议 v10）
  const kernelNodes = $derived((store.ov?.kernels ?? []).map((k) => k.node));
  let mcpDraft = $state<MCPDraft | null>(null);
  let mcpBusy = $state(false);
  let mcpMsg = $state('');

  function draftOf(m?: MCPInfo): MCPDraft {
    return {
      name: m?.name ?? '',
      kind: m?.kind ?? 'stdio',
      command: m?.command ?? '',
      args: (m?.args ?? []).join(' '),
      url: m?.url ?? '',
      cwd: m?.cwd ?? '',
      env: (m?.env_keys ?? []).map((k) => k + '=').join('\n'),
      agents: (m?.agents ?? []).join(', '),
      enabled: m?.enabled ?? true,
      approval: m?.approval ?? 'never',
      machine: m?.machine ?? '',
    };
  }

  // 网页只知道环境变量的名字（值加密存着），值留空表示沿用原来的
  function envOf(text: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const line of text.split('\n')) {
      const t = line.trim();
      if (!t || t.startsWith('#')) continue;
      const i = t.indexOf('=');
      if (i < 0) out[t] = '';
      else out[t.slice(0, i).trim()] = t.slice(i + 1);
    }
    return out;
  }

  function itemOf(m: MCPInfo) {
    return {
      name: m.name, kind: m.kind, command: m.command ?? '', args: m.args ?? [], url: m.url ?? '',
      cwd: m.cwd ?? '', machine: m.machine ?? '', env: Object.fromEntries((m.env_keys ?? []).map((k) => [k, ''])),
      enabled: m.enabled, tools: m.tools ?? [], agents: m.agents ?? [], approval: m.approval,
    };
  }

  async function saveMCP() {
    if (!mcpDraft) return;
    const d = mcpDraft;
    const name = d.name.trim();
    if (!name) {
      mcpMsg = '名字不能为空';
      return;
    }
    const prev = mcps.find((x) => x.name === name);
    const item = {
      name,
      kind: d.kind,
      command: d.kind === 'stdio' ? d.command.trim() : '',
      args: d.kind === 'stdio' ? d.args.trim().split(/\s+/).filter(Boolean) : [],
      url: d.kind === 'http' ? d.url.trim() : '',
      cwd: d.cwd.trim(),
      machine: d.machine,
      env: envOf(d.env),
      enabled: d.enabled,
      tools: prev?.tools ?? [],
      agents: d.agents.split(',').map((s) => s.trim()).filter(Boolean),
      approval: d.approval || 'never',
    };
    mcpBusy = true;
    mcpMsg = '';
    try {
      await api.post('/api/mcp', { items: [...mcps.filter((x) => x.name !== name).map(itemOf), item] });
      mcpDraft = null;
      await store.refresh();
    } catch (err) {
      mcpMsg = String((err as Error)?.message ?? err);
    }
    mcpBusy = false;
  }

  async function removeMCP(name: string) {
    if (!confirm(`删除 MCP 服务「${name}」？`)) return;
    mcpBusy = true;
    try {
      await api.post('/api/mcp', { items: mcps.filter((x) => x.name !== name).map(itemOf) });
      await store.refresh();
    } catch (err) {
      mcpMsg = String((err as Error)?.message ?? err);
    }
    mcpBusy = false;
  }

  async function testMCP(name: string) {
    mcpBusy = true;
    mcpMsg = '';
    try {
      const r = await api.post<{ tools?: { Exposed: string }[]; error?: string }>('/api/mcp/test', { name });
      if (r.error) {
        mcpMsg = `${name}：${r.error}`;
      } else {
        const names = (r.tools ?? []).map((t) => t.Exposed);
        mcpMsg = names.length ? `${name}：连上了，${names.length} 个工具 —— ${names.join('、')}` : `${name}：连上了，但没有工具`;
      }
      await store.refresh();
    } catch (err) {
      mcpMsg = `${name}：${String((err as Error)?.message ?? err)}`;
    }
    mcpBusy = false;
  }

  const ov = $derived(store.ov!);
  const t = $derived(ov.totals);
  const rt = $derived(ov.runtime);
  const up = $derived(rt?.update);
  const pending = $derived(store.fields.filter((f) => f.pending));
  // 网页里改过的设置：在哪个节点、现在是什么，点一下去那个节点
  const changed = $derived(store.fields.filter((f) => f.source === 'web' && f.type !== 'secret'));
  const stray = $derived(strayFields(store.fields));

  function preview(f: FieldState) {
    if (f.type === 'bool') return f.value ? '开' : '关';
    if (Array.isArray(f.value)) return f.value.length ? f.value.join('、') : '空';
    if (f.value === null || f.value === undefined || f.value === '') return '空';
    return truncate(String(typeof f.value === 'object' ? JSON.stringify(f.value) : f.value), 28);
  }
</script>

<div class="pnl">
  <div class="p-card" class:warn={up?.available}>
    <div class="p-row">
      <Icon name="hub" size={14} />
      <span class="p-grow">当前版本 <b>{rt?.version ?? '—'}</b>{#if rt} · 已运行 {since(rt.started)}{/if}</span>
      {#if up?.available}<span class="tag accent">新版本 {up.latest}</span>{:else if up?.checked && !up.error}<span class="tag ok">已经是最新</span>{/if}
    </div>
    {#if up?.error}<div class="p-err">检查失败：{up.error}</div>{/if}
    {#if up?.available && !up.updatable}<p class="p-note">这个网关不能在这里更新：{up.why}</p>{/if}
    <div class="p-actions tight">
      {#if up?.available && up.updatable}
        <button class="btn sm primary" disabled={store.updating || store.restarting} onclick={() => store.updateGateway()}>
          <Icon name="refresh" size={13} />{store.updating ? '更新中…' : `更新到 ${up.latest}`}
        </button>
      {/if}
      <button class="btn sm" disabled={store.checkingUpdate || store.updating} onclick={() => store.checkUpdate()}>
        {store.checkingUpdate ? '检查中…' : '检查更新'}
      </button>
      <button class="btn sm" disabled={store.restarting} onclick={() => confirm('重启网关？正在处理的消息会中断，页面会自动重连。') && store.restartGateway()}>
        <Icon name="refresh" size={13} />{store.restarting ? '重启中…' : '重启网关'}
      </button>
    </div>
    <p class="p-note p-small">更新会备份旧版本，新版本起不来自动退回；接入的电脑上的内核会自动换成同一个版本。检查更新和下载走下面的「默认代理」。</p>
  </div>

  {#if pending.length}
    <div class="p-warn">
      有 {pending.length} 项改动要重启网关才生效：{pending.map((f) => f.label).join('、')}。上面点「重启网关」，网页里的修改会保留。
    </div>
  {/if}

  <div class="p-grid" style="margin-top: 14px">
    <div class="p-stat"><b>{t.runs}</b><span>处理轮数</span></div>
    <div class="p-stat"><b>{t.model_calls}</b><span>模型调用</span></div>
    <div class="p-stat"><b class="acc">{pct(t.cached_tokens, t.prompt_tokens)}</b><span>前缀缓存命中</span></div>
    <div class="p-stat"><b>{num(t.prompt_tokens)}</b><span>输入 tokens</span></div>
    <div class="p-stat"><b>{num(t.completion_tokens)}</b><span>输出 tokens</span></div>
    <div class="p-stat"><b>{num(t.reasoning_tokens)}</b><span>其中思考</span></div>
  </div>
  <div class="p-kv">
    <div><span>配置文件</span><code>{homeShort(ov.gateway.config_dir)}/gateway.toml</code></div>
    <div><span>数据目录</span><code>{homeShort(ov.gateway.data_dir)}</code></div>
    <div><span>内核监听</span><code>{ov.gateway.kernel_listen}</code></div>
  </div>

  <h5 class="p-h">MCP 服务 <span class="muted p-small">{mcps.length ? `${mcps.length} 个，工具名是「服务__工具」` : '还没有'}</span></h5>
  <div class="p-card">
    {#each mcps as m (m.name)}
      <div class="p-row">
        <span class="dot" class:on={m.online} style="background: {m.online ? 'var(--ok)' : 'var(--muted)'}"></span>
        <span class="p-grow">
          <b>{m.name}</b>
          <span class="tag">{m.kind === 'http' ? 'HTTP' : 'stdio'}</span>
          {#if !m.enabled}<span class="tag">已关闭</span>{/if}
          {#if m.tool_count}<span class="tag ok">{m.tool_count} 个工具</span>{/if}
          {#if m.machine}<span class="tag">在 {m.machine} 上</span>{/if}
          {#if m.agents?.length}<span class="tag">仅 {m.agents.join('、')}</span>{/if}
        </span>
        <button class="btn sm" disabled={mcpBusy} onclick={() => testMCP(m.name)}>测试</button>
        <button class="btn sm" disabled={mcpBusy} onclick={() => (mcpDraft = draftOf(m))}>编辑</button>
        <button class="btn sm" disabled={mcpBusy} onclick={() => removeMCP(m.name)}>删除</button>
      </div>
    {/each}
    {#if mcpMsg}<div class="p-note p-small">{mcpMsg}</div>{/if}
    {#if !mcps.length}<p class="p-note">加了以后，管理员会话里的模型就能用它们的工具。默认只在本机跑（stdio）或者连远程（HTTP）。</p>{/if}
    {#if mcpDraft}
      <div class="mcp-form">
        <label><span>名字</span><input bind:value={mcpDraft.name} placeholder="fs" /></label>
        <label><span>类型</span>
          <select bind:value={mcpDraft.kind}>
            <option value="stdio">本机进程（stdio）</option>
            <option value="http">远程（HTTP）</option>
          </select>
        </label>
        {#if mcpDraft.kind === 'stdio'}
          <label><span>在哪台机器</span>
            <select bind:value={mcpDraft.machine}>
              <option value="">网关本机</option>
              {#each kernelNodes as k (k)}<option value={k}>{k}</option>{/each}
            </select>
          </label>
          <label><span>命令</span><input bind:value={mcpDraft.command} placeholder="npx" /></label>
          <label><span>参数</span><input bind:value={mcpDraft.args} placeholder="-y @modelcontextprotocol/server-filesystem /home/me" /></label>
          <label><span>工作目录</span><input bind:value={mcpDraft.cwd} placeholder="留空就是数据目录" /></label>
        {:else}
          <label><span>地址</span><input bind:value={mcpDraft.url} placeholder="https://example.com/mcp" /></label>
        {/if}
        <label><span>环境变量</span><textarea bind:value={mcpDraft.env} rows="3" placeholder="KEY=VALUE，一行一个；值留空表示沿用已存的"></textarea></label>
        <label><span>给哪些 agent</span><input bind:value={mcpDraft.agents} placeholder="留空就是所有 agent" /></label>
        <label><span>审批</span>
          <select bind:value={mcpDraft.approval}>
            <option value="never">不用审批，直接执行</option>
            <option value="per_turn">一轮里批准一次</option>
            <option value="always">每次调用都要批准</option>
          </select>
        </label>
        <label class="inline"><input type="checkbox" bind:checked={mcpDraft.enabled} /><span>启用</span></label>
        <div class="p-actions tight">
          <button class="btn sm primary" disabled={mcpBusy} onclick={saveMCP}>{mcpBusy ? '保存中…' : '保存'}</button>
          <button class="btn sm" disabled={mcpBusy} onclick={() => (mcpDraft = null)}>取消</button>
        </div>
      </div>
    {:else}
      <div class="p-actions tight">
        <button class="btn sm" disabled={mcpBusy} onclick={() => (mcpDraft = draftOf())}>添加 MCP 服务</button>
      </div>
    {/if}
  </div>
  <FieldGroup group="gateway" focus={store.focus} />

  <h5 class="p-h">网页里改过的设置 <span class="muted p-small">{changed.length ? `${changed.length} 项，点一下去它所在的节点` : '都还是配置文件或默认值'}</span></h5>
  {#if changed.length}
    <div class="p-card">
      {#each changed as f (f.key)}
        {@const h = homeOf(f)}
        <button class="p-srow" onclick={() => store.open(h.target, f.key)}>
          <span class="dot" style="background: {h.color}"></span>
          <span class="muted p-nowrap">{h.label}</span>
          <span class="p-grow">{f.label}</span>
          <code class="muted">{preview(f)}</code>
          <Icon name="right" size={13} />
        </button>
      {/each}
    </div>
  {/if}

  {#if stray.length}
    <h5 class="p-h">其他设置 <span class="muted p-small">网页还不认识它们该挂在哪个节点</span></h5>
    <div class="stray">
      {#each stray as f (f.key)}
        <Field {f} highlight={store.focus === f.key} />
      {/each}
    </div>
  {/if}
</div>

<style>
  .mcp-form {
    display: grid;
    gap: 8px;
    padding: 10px 4px 4px;
    border-top: 1px solid var(--line);
    margin-top: 8px;
  }
  .mcp-form label {
    display: grid;
    grid-template-columns: 86px 1fr;
    align-items: center;
    gap: 8px;
    font-size: 12px;
  }
  .mcp-form label > span {
    color: var(--muted);
  }
  .mcp-form label.inline {
    grid-template-columns: auto 1fr;
    justify-content: start;
  }
  .mcp-form input:not([type='checkbox']),
  .mcp-form select,
  .mcp-form textarea {
    width: 100%;
    padding: 6px 8px;
    border-radius: 8px;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    color: var(--fg);
    font: inherit;
  }
  .stray {
    border-radius: 14px;
    background: var(--panel-2);
    border: 1px solid var(--line);
  }
  .stray :global(.field + .field) {
    border-top: 1px solid var(--line);
  }
</style>
