<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { homeShort, machineMeta, shortPath } from '../lib/format';
  import type { MachineInfo, WorkspaceInfo } from '../lib/types';
  import DirPicker from './DirPicker.svelte';
  import Icon from './Icon.svelte';

  // 选工作目录：工作目录是“机器 + 目录”，选哪个，会话就在哪台机器上干活。
  // 列表里没有的，先选机器再浏览那台机器上的目录，添加进列表。
  let {
    current = '',
    title = '选择工作目录',
    none = true,
    onpick,
    onclose,
  }: { current?: string; title?: string; none?: boolean; onpick: (id: string) => void; onclose: () => void } = $props();

  const machines = $derived(store.machines);
  const groups = $derived(
    machines
      .map((m) => ({ m, list: store.workspacesOn(m.name) }))
      .filter((g) => g.list.length),
  );
  let browsing = $state<MachineInfo | null>(null);
  let busy = $state(false);

  function status(m: MachineInfo) {
    if (m.online) return m.kind === 'local' && !m.kernels.length ? '在线 · 由网关执行' : '在线';
    return m.detail || '现在用不了';
  }

  async function add(path: string) {
    const m = browsing!;
    browsing = null;
    busy = true;
    try {
      const w = await api.post<WorkspaceInfo>('/api/workspaces', { host: m.name, path });
      onpick(w.id);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  async function star(w: WorkspaceInfo, e: MouseEvent) {
    e.stopPropagation();
    try {
      await api.put('/api/workspaces/default', { id: w.default ? '' : w.id });
      store.toast({ kind: 'ok', text: w.default ? '新会话不再默认用它' : `新会话默认在 ${w.host}:${w.path} 上工作` }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="backdrop" onclick={onclose}>
  <div class="picker" onclick={(e) => e.stopPropagation()}>
    <header>
      <Icon name="folder" size={17} />
      <strong>{title}</strong>
      <span class="grow"></span>
      <button class="btn sm ghost" onclick={onclose} aria-label="关闭"><Icon name="x" size={14} /></button>
    </header>
    <p class="lead">工作目录是“机器 + 目录”：选哪个，这个会话就在哪台机器上读写文件、执行命令。</p>

    <div class="body">
      {#each groups as { m, list } (m.name)}
        <div class="group">
          <div class="mh">
            <Icon name={machineMeta[m.kind].icon} size={14} />
            <b>{m.name}</b>
            <span class="kind">{m.kind === 'local' ? '网关所在的机器' : machineMeta[m.kind].label}</span>
            <span class="st" class:off={!m.online}><i class="dot {m.online ? 'online' : 'stopped'}"></i>{status(m)}</span>
          </div>
          {#each list as w (w.id)}
            <button class="ws" class:cur={w.id === current} onclick={() => onpick(w.id)} disabled={busy}>
              <Icon name="folder" size={15} />
              <code class="p" title={w.path}>{shortPath(homeShort(w.path), 58)}</code>
              <span class="meta">{w.sessions ? `${w.sessions} 个会话` : ''}</span>
              <span class="star" class:on={w.default} role="button" tabindex="-1" title={w.default ? '新会话默认用它（点一下取消）' : '设为新会话的默认'} onclick={(e) => star(w, e)}>★</span>
              {#if w.id === current}<span class="tag accent">当前</span>{/if}
            </button>
          {/each}
        </div>
      {:else}
        <p class="empty">还没有工作目录。在下面选一台机器，浏览它上面的目录添加一个。</p>
      {/each}

      {#if none}
        <button class="ws chat" class:cur={!current} onclick={() => onpick('')} disabled={busy}>
          <Icon name="chat" size={15} />
          <span class="p">只聊天，不用工作目录</span>
          {#if !current}<span class="tag accent">当前</span>{/if}
        </button>
      {/if}

      <div class="add">
        <div class="ah"><Icon name="plus" size={14} />添加新的工作目录：先选机器</div>
        <div class="machines">
          {#each machines as m (m.name)}
            <button class="mc" disabled={!m.online || busy} onclick={() => (browsing = m)} title={m.online ? `浏览 ${m.name} 上的目录` : status(m)}>
              <Icon name={machineMeta[m.kind].icon} size={14} />
              {m.name}
              <span class="dot {m.online ? 'online' : 'stopped'}"></span>
            </button>
          {/each}
          <button class="mc ghost" onclick={() => { onclose(); store.go('#/devices'); }}><Icon name="server" size={14} />添加 SSH 服务器…</button>
        </div>
      </div>
    </div>
  </div>
</div>

{#if browsing}
  <DirPicker host={browsing.kind === 'local' ? '' : browsing.name} onpick={add} onclose={() => (browsing = null)} />
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 60;
    display: grid;
    place-items: center;
    background: rgba(0, 0, 0, 0.35);
    backdrop-filter: blur(2px);
    animation: fade 0.15s ease;
  }
  .picker {
    width: min(620px, calc(100vw - 32px));
    max-height: min(720px, calc(100vh - 48px));
    display: flex;
    flex-direction: column;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    border-radius: 16px;
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  header {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 14px 16px 6px;
    color: var(--c-dir);
  }
  header strong {
    color: var(--text);
    font-size: 15px;
  }
  .grow {
    flex: 1;
  }
  .lead {
    margin: 0;
    padding: 0 16px 12px;
    color: var(--muted);
    font-size: 12.5px;
    line-height: 1.6;
    border-bottom: 1px solid var(--line);
  }
  .body {
    overflow: auto;
    padding: 10px 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .group {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .mh {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 4px 6px;
    font-size: 12.5px;
    color: var(--text-2);
  }
  .mh b {
    color: var(--text);
  }
  .kind {
    color: var(--muted);
  }
  .st {
    margin-left: auto;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 11.5px;
    color: var(--muted);
    max-width: 50%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .st.off {
    color: var(--warn);
  }
  .ws {
    display: flex;
    align-items: center;
    gap: 9px;
    width: 100%;
    padding: 9px 11px;
    border-radius: 10px;
    border: 1px solid var(--line);
    background: var(--panel);
    color: var(--text);
    text-align: left;
    cursor: pointer;
    font: inherit;
  }
  .ws:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .ws.cur {
    border-color: color-mix(in srgb, var(--accent) 60%, var(--line));
    background: color-mix(in srgb, var(--accent) 9%, var(--panel));
  }
  .ws :global(svg) {
    color: var(--c-dir);
    flex: none;
  }
  .ws.chat :global(svg) {
    color: var(--muted);
  }
  .p {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
  }
  code.p {
    font-family: var(--mono);
    font-size: 12.5px;
  }
  .meta {
    color: var(--muted);
    font-size: 11.5px;
    white-space: nowrap;
  }
  .star {
    color: var(--line-2);
    font-size: 15px;
    padding: 0 3px;
    cursor: pointer;
  }
  .star:hover {
    color: var(--warn);
  }
  .star.on {
    color: var(--warn);
  }
  .empty {
    margin: 4px 6px;
    color: var(--muted);
    font-size: 13px;
  }
  .add {
    border-top: 1px dashed var(--line-2);
    padding-top: 12px;
    margin-top: 2px;
  }
  .ah {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12.5px;
    color: var(--text-2);
    margin: 0 4px 9px;
  }
  .machines {
    display: flex;
    flex-wrap: wrap;
    gap: 7px;
  }
  .mc {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 7px 11px;
    border-radius: 9px;
    border: 1px solid var(--line-2);
    background: var(--panel-2);
    color: var(--text);
    font: inherit;
    font-size: 12.5px;
    cursor: pointer;
  }
  .mc:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .mc:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .mc.ghost {
    background: transparent;
    color: var(--muted);
    border-style: dashed;
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    display: inline-block;
  }
  @keyframes fade {
    from {
      opacity: 0;
    }
  }
</style>
