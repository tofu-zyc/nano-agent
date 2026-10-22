<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { homeShort, machineMeta, shortPath } from '../lib/format';
  import type { MachineInfo, WorkspaceInfo } from '../lib/types';
  import DirPicker from './DirPicker.svelte';
  import Icon from './Icon.svelte';

  // 工作目录是“机器 + 目录”。两种用法：
  //   - 给会话挂载（mounted 不为 null）：可以同时挂好几个，在不同的机器上，点一下挂上、再点取下；
  //   - 只选一个（onpick）：比如在拓扑图上添加工作目录。
  // 列表里没有的，先选机器再浏览那台机器上的目录，添加进列表（挂载模式下顺便挂上）。
  let {
    mounted = null,
    title = '工作目录',
    onchange,
    onpick,
    onclose,
  }: {
    mounted?: string[] | null;
    title?: string;
    onchange?: (ids: string[]) => void;
    onpick?: (id: string) => void;
    onclose: () => void;
  } = $props();

  const mounting = $derived(mounted !== null);
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

  function choose(w: WorkspaceInfo) {
    if (!mounting) return onpick?.(w.id);
    const ids = mounted!;
    onchange?.(ids.includes(w.id) ? ids.filter((x) => x !== w.id) : [...ids, w.id]);
  }

  async function add(path: string) {
    const m = browsing!;
    browsing = null;
    busy = true;
    try {
      const w = await api.post<WorkspaceInfo>('/api/workspaces', { host: m.name, path });
      if (mounting) onchange?.([...mounted!.filter((x) => x !== w.id), w.id]);
      else onpick?.(w.id);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  async function star(w: WorkspaceInfo, e: MouseEvent) {
    e.stopPropagation();
    try {
      await api.put('/api/workspaces/default', { id: w.id, default: !w.default });
      store.toast({ kind: 'ok', text: w.default ? '新会话不再默认挂它' : `新会话会默认挂上 ${w.host}:${w.path}` }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }

  function onkey(e: KeyboardEvent) {
    if (e.isComposing || e.key !== 'Escape' || browsing) return; // 目录选择器盖在上面时，由它自己关
    e.preventDefault();
    onclose();
  }
</script>

<svelte:window onkeydown={onkey} />

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
    {#if mounting}
      <p class="lead">挂上的工作目录 AI 都能用，可以在不同的机器上；挂了好几个时，它调用工具会写明在哪个上执行。排第一的决定优先由哪台机器上的内核来跑。</p>
    {:else}
      <p class="lead">工作目录是“机器 + 目录”：工具在那台机器上、只能碰到这个目录。</p>
    {/if}

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
            {@const on = mounted?.includes(w.id)}
            <button class="ws" class:cur={on} onclick={() => choose(w)} disabled={busy}>
              {#if mounting}
                <span class="check" class:on>{#if on}<Icon name="check" size={12} />{/if}</span>
              {:else}
                <Icon name="folder" size={15} />
              {/if}
              <code class="p" title={w.path}>{shortPath(homeShort(w.path), 54)}</code>
              {#if on && mounted!.length > 1}<span class="ord" title="挂载的顺序">#{mounted!.indexOf(w.id) + 1}</span>{/if}
              <span class="meta">{w.sessions ? `${w.sessions} 个会话` : ''}</span>
              <span class="star" class:on={w.default} role="button" tabindex="-1" title={w.default ? '新会话默认挂上它（点一下取消）' : '新会话默认挂上它'} onclick={(e) => star(w, e)}>★</span>
            </button>
          {/each}
        </div>
      {:else}
        <p class="empty">还没有工作目录。在下面选一台机器，浏览它上面的目录添加一个。</p>
      {/each}

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
    {#if mounting}
      <footer>
        <span class="sum">{mounted!.length ? `挂了 ${mounted!.length} 个` : '没有挂载：只读，不能写文件、执行命令'}</span>
        {#if mounted!.length}<button class="btn sm ghost" onclick={() => onchange?.([])}>全部取下</button>{/if}
        <button class="btn sm primary" onclick={onclose}>完成</button>
      </footer>
    {/if}
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
  .check {
    width: 17px;
    height: 17px;
    flex: none;
    display: grid;
    place-items: center;
    border-radius: 5px;
    border: 1.5px solid var(--line-2);
    color: #fff;
  }
  .check.on {
    background: var(--accent);
    border-color: var(--accent);
  }
  .ws .check :global(svg) {
    color: #fff;
  }
  .ord {
    font-size: 11px;
    color: var(--accent-2);
    font-family: var(--mono);
  }
  footer {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 14px;
    border-top: 1px solid var(--line);
  }
  .sum {
    flex: 1;
    font-size: 12px;
    color: var(--muted);
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
