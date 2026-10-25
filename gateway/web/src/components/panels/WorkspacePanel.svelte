<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { channelMeta, homeShort } from '../../lib/format';
  import { portal } from '../../lib/portal';
  import type { WorkspaceInfo } from '../../lib/types';
  import WorkspacePicker from '../WorkspacePicker.svelte';
  import Icon from '../Icon.svelte';

  let { id }: { id?: string } = $props();

  const ov = $derived(store.ov!);
  const ws = $derived(id ? (ov.workspaces ?? []).find((w) => w.id === id) : undefined);
  const sessions = $derived(ws ? (ov.sessions ?? []).filter((s) => s.workspaces.some((m) => m.id === ws.id)) : []);
  let adding = $state(false);

  async function setDefault(w: WorkspaceInfo) {
    try {
      await api.put('/api/workspaces/default', { id: w.id, default: !w.default });
      store.toast({ kind: 'ok', text: w.default ? '新会话不再默认挂它' : '新会话会默认挂上它' }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
  async function remove(w: WorkspaceInfo) {
    if (!confirm(`从列表里删掉 ${w.host}:${w.path}？${w.sessions ? `挂着它的 ${w.sessions} 个会话会取下它。` : ''}机器上的文件不会动。`)) return;
    try {
      await api.del(`/api/workspaces/${w.id}`);
      store.open({ type: 'workspace' });
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="pnl">
  {#if id}
    {#if ws}
      <div class="p-card" class:bad={!ws.online}>
        <code class="path">{ws.path}</code>
        <div class="p-row">
          <span class="p-k">机器</span>
          <button class="p-lnk p-grow" onclick={() => store.open({ type: 'machine', name: ws.host })}>{ws.host}</button>
          <span class="dot {ws.online ? 'online' : 'stopped'}"></span>
        </div>
        {#if !ws.online}<p class="p-note">这台机器现在用不了，挂着它的会话暂时不能在这里执行工具。</p>{/if}
        <div class="p-actions tight">
          <button class="btn sm" onclick={() => setDefault(ws)}><span class="star" class:on={ws.default}>★</span>{ws.default ? '取消新会话默认' : '设为新会话的默认'}</button>
        </div>
      </div>
      <h5 class="p-h">在这里工作的会话</h5>
      <div class="p-card">
        {#each sessions as s (s.sid)}
          <button class="p-srow" onclick={() => store.open({ type: 'session', sid: s.sid })}>
            <span class="dot" style="background: {channelMeta[s.channel]?.color}"></span>
            <span class="p-grow">{s.title}</span>
            {#if s.running}<span class="tag">由 {s.run_node} 跑</span>{/if}
            <Icon name="right" size={13} />
          </button>
        {:else}
          <p class="p-note">还没有会话在这里工作。在会话节点里“挂载工作目录”选它。</p>
        {/each}
      </div>
      <p class="p-note" style="margin-top: 10px">工具在工作目录里能读能写，外面只能读，写外面要提权审批。权限模式（放行、自动、审批、只读）跟着会话，新会话的默认值在 Agent 节点里。</p>
      <div class="p-danger-zone">
        <button class="btn sm danger" onclick={() => remove(ws)}><Icon name="trash" size={13} />从列表删掉（机器上的文件不动）</button>
      </div>
    {:else}
      <p class="p-note">这个工作目录已经从列表里删掉了。</p>
    {/if}
  {:else}
    <p class="p-note lead">工作目录是“机器 + 目录”，工具只能写这里面。每个会话可以挂好几个（在不同的机器上也行），标了 ★ 的新会话默认挂上。</p>
    <button class="btn primary" onclick={() => (adding = true)}><Icon name="plus" size={14} />添加工作目录</button>
    <h5 class="p-h">全部工作目录</h5>
    <div class="p-card">
      {#each ov.workspaces ?? [] as w (w.id)}
        <button class="p-srow" onclick={() => store.open({ type: 'workspace', id: w.id })}>
          <span class="dot {w.online ? 'online' : 'stopped'}"></span>
          <span class="muted p-nowrap">{w.host}</span>
          <code class="p-grow">{homeShort(w.path)}</code>
          {#if w.default}<span class="star on">★</span>{/if}
          <span class="muted p-small p-nowrap">{w.sessions} 个会话</span>
          <Icon name="right" size={13} />
        </button>
      {:else}
        <p class="p-note">还没有工作目录：会话在 HOME 下工作，能读、能执行，只能写临时目录。</p>
      {/each}
    </div>
  {/if}
</div>

{#if adding}
  <div use:portal>
    <WorkspacePicker title="添加工作目录" onpick={(wid) => { adding = false; store.open({ type: 'workspace', id: wid }); }} onclose={() => (adding = false)} />
  </div>
{/if}

<style>
  .path {
    word-break: break-all;
    font-size: 13px;
  }
  .star {
    color: var(--line-2);
  }
  .star.on {
    color: var(--warn);
  }
</style>
