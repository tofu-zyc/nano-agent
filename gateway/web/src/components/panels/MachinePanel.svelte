<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { homeShort, osLabel } from '../../lib/format';
  import { portal } from '../../lib/portal';
  import type { WorkspaceInfo } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import DirPicker from '../DirPicker.svelte';
  import SshForm from './SshForm.svelte';
  import Icon from '../Icon.svelte';

  let { name }: { name: string } = $props();

  const sshState: Record<string, string> = { online: '已连接', offline: '未连接', connecting: '连接中', reconnecting: '等待重连', disconnecting: '断开中', error: '连接失败' };
  const m = $derived(store.machines.find((x) => x.name === name));
  const conn = $derived(m?.kind === 'ssh' ? store.devices?.connections.find((c) => c.name === name) : undefined);
  const paired = $derived(m?.kind === 'kernel' ? store.devices?.paired.find((p) => p.node === name) : undefined);
  const localKernel = $derived(store.ov?.kernels.find((k) => k.managed));
  // 挂在这台机器上跑的 MCP 服务（machine 写的是那台内核的名字）
  const mcpHere = $derived((store.ov?.mcp ?? []).filter((x) => x.machine && (x.machine === name || (m?.kernels ?? []).includes(x.machine))));
  const leases = $derived((store.ov?.execution?.leases ?? []).filter((x)=>x.machine===name));
  const executionStates:Record<string,string>={queued:'排队中',granted:'已获执行槽',running:'执行中',cancelling:'正在停止',unknown:'等待核对'};
  const waitReasons:Record<string,string>={resource:'资源被占用',node_slots:'节点执行槽',machine_slots:'机器执行槽',shell_slots:'命令执行槽',run_slots:'当前任务并行上限',reconcile:'等待执行状态核对'};
  let browsing = $state(false);
  let editing = $state(false);
  let busy = $state(false);

  // SSH 连接的状态（连接中、等待重连……）没有单独的事件，面板开着时隔一会儿读一次
  onMount(() => {
    store.loadDevices();
    const t = setInterval(() => m?.kind === 'ssh' && store.loadDevices(), 2500);
    return () => clearInterval(t);
  });

  function status() {
    if (!m) return '不在线';
    if (!m.online) return m.detail || '现在用不了';
    if (m.kind === 'local') return m.kernels.length ? `内核 ${m.kernels.join('、')} 在这台机器上，工具由它直接执行` : '这台机器上没有在线的内核，工具由网关直接执行';
    if (m.kind === 'ssh') return '网关通过 SSH 连着，工具由网关在服务器上执行（服务器上不跑内核）';
    return `内核 ${m.name} 在这台电脑上，工具由它直接执行`;
  }
  async function act(fn: () => Promise<unknown>, ok?: string) {
    busy = true;
    try {
      await fn();
      if (ok) store.toast({ kind: 'ok', text: ok }, 2000);
      await store.loadDevices();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
  async function addOn(path: string) {
    browsing = false;
    try {
      const w = await api.post<WorkspaceInfo>('/api/workspaces', { host: name, path });
      store.open({ type: 'workspace', id: w.id });
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="pnl">
  <div class="p-card" class:bad={!m?.online}>
    <div class="p-row"><span class="p-k">状态</span><span class="p-grow">{status()}</span></div>
    {#if m?.online}
      <div class="p-row">
        <span class="p-k">沙箱</span>
        {#if m.landlock > 0}<span class="tag ok"><Icon name="shield" size={11} />Landlock v{m.landlock}</span>{:else}<span class="tag warn">没有沙箱，自动模式下的命令也要审批</span>{/if}
      </div>
    {/if}
    {#if m?.os}<div class="p-row"><span class="p-k">系统</span><span class="p-grow">{osLabel(m.os)}</span></div>{/if}
    {#if m?.kind === 'local' && localKernel}
      <div class="p-row"><span class="p-k">内核</span><span class="p-grow">网关启动的 {localKernel.node}</span><button class="btn sm" onclick={() => store.open({ type: 'kernel', node: localKernel.node })}>查看</button></div>
    {/if}
    {#if paired}
      <div class="p-row"><span class="p-k">配对</span><span class="p-grow">{new Date(paired.since).toLocaleDateString()} 配对 · 从 {paired.addr} 连来</span></div>
    {/if}
  </div>

  {#if store.ov?.execution?.error}<div class="p-err">执行记录未能保存：{store.ov.execution.error}</div>{/if}
  {#if leases.length}
    <h5 class="p-h">执行和资源占用 · {leases.length}</h5>
    <div class="p-card">
      {#each leases as lease (lease.id)}
        <div class="p-row"><b>{lease.name || lease.call_id}</b><span class="p-grow">{executionStates[lease.state] ?? lease.state}{lease.background?' · 后台':''}</span><code>{lease.sid}</code></div>
        {#if lease.wait_for}<p class="p-note">{waitReasons[lease.wait_for] ?? lease.wait_for}{lease.holder?' · 占用者 '+lease.holder:''}</p>{/if}
        {#if lease.state==='unknown'}<p class="p-warn">执行状态尚未确认，继续保留执行槽和资源，节点重连后核对。</p>{/if}
        <details><summary class="p-small muted">调用身份和资源</summary><pre class="detail">{lease.run_id} / {lease.turn_id} / {lease.call_id}
共享：{(lease.shared??[]).join('、') || '无'}
独占：{(lease.exclusive??[]).join('、') || '无'}</pre></details>
      {/each}
    </div>
  {/if}

  {#if conn}
    <h5 class="p-h">SSH 连接</h5>
    <div class="p-card" class:bad={conn.state === 'error'}>
      <div class="p-row">
        <span class="dot {conn.state === 'online' ? 'online' : conn.state === 'error' ? 'error' : conn.state === 'offline' ? 'stopped' : 'starting'}"></span>
        <code class="p-grow">{conn.user ? conn.user + '@' : ''}{conn.host}{conn.port ? ':' + conn.port : ''}</code>
        <span class="muted p-small p-nowrap">{sshState[conn.state] ?? conn.state}</span>
      </div>
      {#if conn.detail}<pre class="detail">{conn.detail}</pre>{/if}
      <p class="p-note p-small">{conn.has_password ? '密码认证' : 'SSH 配置 / 密钥认证'}{conn.auto_connect ? ' · 网关启动时连接，掉线自动重连' : ' · 手动连接'}</p>
      <div class="p-actions tight">
        {#if ['online', 'connecting', 'reconnecting'].includes(conn.state)}
          <button class="btn sm" disabled={busy} onclick={() => act(() => api.post(`/api/devices/${conn.id}/disconnect`), '正在断开')}>断开连接</button>
        {:else}
          <button class="btn sm primary" disabled={busy || conn.state === 'disconnecting'} onclick={() => act(() => api.post(`/api/devices/${conn.id}/connect`), '正在连接')}>连接</button>
        {/if}
        <button class="btn sm" disabled={busy} onclick={() => (editing = true)}><Icon name="pen" size={13} />编辑</button>
      </div>
    </div>
  {/if}

  {#if m?.kind === 'local'}
    <FieldGroup group="local" focus={store.focus} />
  {/if}

  {#if mcpHere.length}
    <h5 class="p-h">这台机器上跑的 MCP 服务</h5>
    <div class="p-card">
      {#each mcpHere as x (x.name)}
        <button class="p-srow" onclick={() => store.open({ type: 'mcp' })}>
          <Icon name="plug" size={14} />
          <span class="p-grow">{x.name}</span>
          <span class="muted p-small p-nowrap">{!x.enabled ? '已关闭' : x.online ? `连着 · ${x.tool_count} 个工具` : '没连着'}</span>
          <Icon name="right" size={13} />
        </button>
      {/each}
    </div>
  {/if}

  <h5 class="p-h">这台机器上的工作目录</h5>
  <div class="p-card">
    {#each store.workspacesOn(name) as w (w.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'workspace', id: w.id })}>
        <Icon name="folder" size={14} />
        <code class="p-grow">{homeShort(w.path)}</code>
        {#if w.default}<span class="tag accent">新会话默认</span>{/if}
        <span class="muted p-small p-nowrap">{w.sessions} 个会话</span>
        <Icon name="right" size={13} />
      </button>
    {:else}
      <p class="p-note">还没有。</p>
    {/each}
    <button class="btn sm" disabled={!m?.online} onclick={() => (browsing = true)} title={m?.online ? '' : '这台机器现在用不了'}><Icon name="plus" size={13} />浏览这台机器，添加工作目录</button>
  </div>

  {#if conn || paired}
    <div class="p-danger-zone">
      {#if conn}
        <button class="btn sm danger" disabled={busy || ['online', 'connecting', 'reconnecting'].includes(conn.state)} title="先断开连接再删除"
          onclick={() => confirm(`删除 SSH 服务器「${conn.name}」？它上面的工作目录会从列表里去掉，服务器上的文件不会动。`) && act(() => api.del(`/api/devices/${conn.id}`), '已删除')}>
          <Icon name="trash" size={13} />删除这台服务器
        </button>
      {/if}
      {#if paired}
        <button class="btn sm danger" disabled={busy}
          onclick={() => confirm(`移除 ${paired.node}？它的口令会作废${paired.online ? '，现在就会断开' : ''}。要再接入，需要重新配对。`) && act(() => api.del(`/api/paired/${encodeURIComponent(paired.node)}`), '已移除')}>
          <Icon name="trash" size={13} />移除这台电脑（口令作废）
        </button>
      {/if}
    </div>
  {/if}
</div>

{#if browsing && m}
  <div use:portal>
    <DirPicker host={m.kind === 'local' ? '' : m.name} onpick={addOn} onclose={() => (browsing = false)} />
  </div>
{/if}
{#if editing && conn}
  <SshForm {conn} onclose={() => (editing = false)} />
{/if}

<style>
  .detail {
    margin: 0;
    font: 11px/1.6 var(--mono);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 130px;
    overflow: auto;
    color: var(--muted);
    background: var(--bg-2);
    padding: 8px 10px;
    border-radius: 8px;
  }
</style>
