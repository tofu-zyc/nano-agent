<script lang="ts">
  // MCP 服务（拓扑图「能力和内核」一列的「MCP 服务」节点）：加、改、删、测试。
  // 聊天里 AI 也能做同样的事（Gateway mcp 查、GatewayAdmin mcp_save / mcp_delete / mcp_test，见技能 mcp）。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import type { MCPInfo } from '../../lib/types';
  import Icon from '../Icon.svelte';

  type Draft = {
    orig: string; // 编辑时的原名（改名时靠它找回旧的密钥）
    name: string;
    kind: 'stdio' | 'http';
    command: string;
    args: string; // 一行一个
    url: string;
    cwd: string;
    env: string; // KEY=VALUE 一行一个
    agents: string;
    tools: string;
    enabled: boolean;
    approval: string;
    machine: string;
  };

  const mcps = $derived(store.ov?.mcp ?? []);
  // 能挂 MCP 的电脑：在线的内核（本机的内核也算，但一般直接选“网关本机”就行）
  const kernels = $derived(store.ov?.kernels ?? []);
  const online = (node: string) => kernels.some((k) => k.node === node);
  const approvalLabel: Record<string, string> = { never: '不用审批', per_turn: '一轮批一次', always: '每次都批' };

  let draft = $state<Draft | null>(null);
  let busy = $state(false);
  let msg = $state('');
  let open = $state<Record<string, boolean>>({}); // 展开看工具列表

  function draftOf(m?: MCPInfo): Draft {
    return {
      orig: m?.name ?? '',
      name: m?.name ?? '',
      kind: m?.kind ?? 'stdio',
      command: m?.command ?? '',
      args: (m?.args ?? []).join('\n'),
      url: m?.url ?? '',
      cwd: m?.cwd ?? '',
      env: (m?.env_keys ?? []).map((k) => k + '=').join('\n'),
      agents: (m?.agents ?? []).join(', '),
      tools: (m?.tools ?? []).join(', '),
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
  const list = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean);

  // 保存是整份覆盖：没在改的服务原样带上（env 只有名字，值留空 = 沿用）
  function itemOf(m: MCPInfo) {
    return {
      name: m.name, kind: m.kind, command: m.command ?? '', args: m.args ?? [], url: m.url ?? '',
      cwd: m.cwd ?? '', machine: m.machine ?? '', env: Object.fromEntries((m.env_keys ?? []).map((k) => [k, ''])),
      enabled: m.enabled, tools: m.tools ?? [], agents: m.agents ?? [], approval: m.approval,
    };
  }

  async function post(items: unknown[]) {
    busy = true;
    msg = '';
    try {
      await api.post('/api/mcp', { items });
      await store.refresh();
      return true;
    } catch (err) {
      msg = String((err as Error)?.message ?? err);
      return false;
    } finally {
      busy = false;
    }
  }

  async function save() {
    if (!draft) return;
    const d = draft;
    const name = d.name.trim();
    if (!name) {
      msg = '名字不能为空';
      return;
    }
    const orig = d.orig || name;
    const item = {
      name,
      orig,
      kind: d.kind,
      command: d.kind === 'stdio' ? d.command.trim() : '',
      // 一行一个参数（路径里有空格也不会被拆开）
      args: d.kind === 'stdio' ? d.args.split('\n').map((s) => s.trim()).filter(Boolean) : [],
      url: d.kind === 'http' ? d.url.trim() : '',
      cwd: d.kind === 'stdio' ? d.cwd.trim() : '',
      machine: d.kind === 'stdio' ? d.machine : '',
      env: envOf(d.env),
      enabled: d.enabled,
      tools: list(d.tools),
      agents: list(d.agents),
      approval: d.approval || 'never',
    };
    if (await post([...mcps.filter((x) => x.name !== orig).map(itemOf), item])) draft = null;
  }

  async function remove(name: string) {
    if (!confirm(`删除 MCP 服务「${name}」？`)) return;
    await post(mcps.filter((x) => x.name !== name).map(itemOf));
  }

  async function test(name: string) {
    busy = true;
    msg = '';
    try {
      const r = await api.post<{ tools?: { Exposed: string }[]; error?: string }>('/api/mcp/test', { name });
      if (r.error) {
        msg = `${name}：${r.error}`;
      } else {
        const names = (r.tools ?? []).map((t) => t.Exposed);
        msg = names.length ? `${name}：连上了，${names.length} 个工具 —— ${names.join('、')}` : `${name}：连上了，但没有工具`;
      }
      await store.refresh();
    } catch (err) {
      msg = `${name}：${String((err as Error)?.message ?? err)}`;
    }
    busy = false;
  }
</script>

<div class="pnl">
  <p class="p-note">
    MCP 服务给模型加外部工具（文件系统、浏览器、computer use……），工具名是「服务名__工具名」，只有管理员的会话能用。
    可以在网关本机起进程、连远程地址，或者挂到某台接入的电脑上跑。也可以直接在聊天里让 AI 帮你加（它会先查、加好以后测一次）。
  </p>

  <div class="p-card">
    {#each mcps as m (m.name)}
      <div class="srv">
        <div class="p-row">
          <span class="dot" style="background: {m.online ? 'var(--ok)' : 'var(--muted)'}" title={m.online ? '连着' : '没连着（用到时会连）'}></span>
          <span class="p-grow">
            <b>{m.name}</b>
            <span class="tag">{m.kind === 'http' ? '远程地址' : m.machine ? `在 ${m.machine} 上` : '网关本机'}</span>
            {#if m.machine && !online(m.machine)}<span class="tag warn">这台电脑不在线</span>{/if}
            {#if !m.enabled}<span class="tag">已关闭</span>{/if}
            <span class="tag">{approvalLabel[m.approval] ?? m.approval}</span>
            {#if m.agents?.length}<span class="tag">仅 {m.agents.join('、')}</span>{/if}
          </span>
        </div>
        <div class="muted p-small mono">{m.kind === 'http' ? m.url : [m.command, ...(m.args ?? [])].join(' ')}</div>
        <div class="p-row">
          <button class="linkish" onclick={() => (open[m.name] = !open[m.name])} disabled={!m.tool_count}>
            {m.tool_count ? `${m.tool_count} 个工具${open[m.name] ? '（收起）' : '（展开）'}` : '还没列到工具（点“测试”连一次）'}
          </button>
          <span class="p-grow"></span>
          <button class="btn sm" disabled={busy} onclick={() => test(m.name)}>测试</button>
          <button class="btn sm" disabled={busy} onclick={() => (draft = draftOf(m))}>编辑</button>
          <button class="btn sm ghost danger" disabled={busy} onclick={() => remove(m.name)} title="删除"><Icon name="trash" size={13} /></button>
        </div>
        {#if open[m.name] && m.tool_names?.length}
          <div class="tools">{#each m.tool_names as t (t)}<code>{m.name}__{t}</code>{/each}</div>
        {/if}
      </div>
    {:else}
      <p class="p-note">还没有 MCP 服务。</p>
    {/each}
    {#if msg}<div class="p-note p-small">{msg}</div>{/if}
    {#if draft}
      <div class="form">
        <label><span>名字</span><input bind:value={draft.name} placeholder="fs" /></label>
        <label><span>类型</span>
          <select bind:value={draft.kind}>
            <option value="stdio">起一个进程（stdio）</option>
            <option value="http">远程地址（HTTP）</option>
          </select>
        </label>
        {#if draft.kind === 'stdio'}
          <label><span>在哪台机器</span>
            <select bind:value={draft.machine}>
              <option value="">网关本机</option>
              {#each kernels as k (k.node)}<option value={k.node}>{k.node}{k.local ? '（和网关同一台）' : ''}{k.os ? ` · ${k.os}` : ''}</option>{/each}
              {#if draft.machine && !online(draft.machine)}<option value={draft.machine}>{draft.machine}（现在不在线）</option>{/if}
            </select>
          </label>
          <label><span>命令</span><input bind:value={draft.command} placeholder="npx" /></label>
          <label><span>参数</span><textarea bind:value={draft.args} rows="3" placeholder={'一行一个，比如：\n-y\n@modelcontextprotocol/server-filesystem\n/home/me'}></textarea></label>
          <label><span>工作目录</span><input bind:value={draft.cwd} placeholder="留空用默认（网关或内核进程的当前目录）" /></label>
        {:else}
          <label><span>地址</span><input bind:value={draft.url} placeholder="https://example.com/mcp" /></label>
        {/if}
        <label><span>环境变量</span><textarea bind:value={draft.env} rows="3" placeholder="KEY=VALUE，一行一个；密钥放这里（加密保存）；值留空表示沿用已存的"></textarea></label>
        <label><span>只开放工具</span><input bind:value={draft.tools} placeholder="留空就是全部；逗号分隔，!名字 表示禁用" /></label>
        <label><span>给哪些 agent</span><input bind:value={draft.agents} placeholder="留空就是所有 agent；逗号分隔的 agent ID" /></label>
        <label><span>审批</span>
          <select bind:value={draft.approval}>
            <option value="never">不用审批，直接执行</option>
            <option value="per_turn">一轮里批准一次</option>
            <option value="always">每次调用都要批准</option>
          </select>
        </label>
        <label class="inline"><input type="checkbox" bind:checked={draft.enabled} /><span>启用</span></label>
        <div class="p-actions tight">
          <button class="btn sm primary" disabled={busy} onclick={save}>{busy ? '保存中…' : '保存'}</button>
          <button class="btn sm" disabled={busy} onclick={() => (draft = null)}>取消</button>
        </div>
      </div>
    {:else}
      <div class="p-actions tight">
        <button class="btn sm primary" disabled={busy} onclick={() => (draft = draftOf())}><Icon name="plus" size={13} />添加 MCP 服务</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .srv {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 8px 2px;
    border-bottom: 1px solid var(--line);
  }
  .tag {
    margin-left: 4px;
    padding: 1px 7px;
    border-radius: 99px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    font-size: 11px;
    color: var(--text-2);
  }
  .tag.warn {
    color: var(--warn);
  }
  .mono {
    font-family: var(--mono);
    overflow-wrap: anywhere;
  }
  .linkish {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: 12px;
    cursor: pointer;
  }
  .linkish:disabled {
    color: var(--muted);
    cursor: default;
  }
  .tools {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .tools code {
    font-size: 11px;
    padding: 1px 6px;
    border-radius: 6px;
    background: var(--bg-2);
  }
  .form {
    display: grid;
    gap: 8px;
    padding: 10px 4px 4px;
    margin-top: 8px;
  }
  .form label {
    display: grid;
    grid-template-columns: 86px minmax(0, 1fr);
    align-items: center;
    gap: 8px;
    font-size: 12px;
  }
  .form label > span {
    color: var(--muted);
  }
  .form label.inline {
    grid-template-columns: auto 1fr;
    justify-content: start;
  }
  .form input:not([type='checkbox']),
  .form select,
  .form textarea {
    width: 100%;
    box-sizing: border-box;
    padding: 6px 8px;
    border-radius: 8px;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    color: var(--fg);
    font: inherit;
  }
</style>
