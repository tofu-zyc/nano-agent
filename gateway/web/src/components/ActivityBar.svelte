<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { describe, type Line } from '../lib/events';
  import { clock } from '../lib/format';
  import Icon from './Icon.svelte';

  let open = $state(false);
  const title = (sid: string) => store.session(sid)?.title ?? sid;
  const lines = $derived(
    store.log
      .map((e) => ({ e, l: describe(e, title) }))
      .filter((x): x is { e: (typeof store.log)[number]; l: Line } => x.l !== null)
      .slice(-120)
      .reverse(),
  );
  const latest = $derived(lines[0]);
  const busy = $derived(Object.keys(store.phase).length);

  function onkey(e: KeyboardEvent) {
    if (e.isComposing) return;
    if (e.key === 'Escape' && open) {
      e.preventDefault();
      open = false;
    }
  }
</script>

<svelte:window onkeydown={onkey} />

{#if open}
  <div class="panel">
    <div class="head">
      <strong>实时活动</strong>
      <span class="muted">网关启动后的事件，最近的在上面</span>
      <button class="btn sm ghost" onclick={() => (open = false)}><Icon name="down" size={14} /></button>
    </div>
    <div class="list">
      {#each lines as { e, l }}
        <a class="row {l.tone ?? ''}" href={l.sid ? `#/sessions/${encodeURIComponent(l.sid)}` : undefined}>
          <span class="time">{clock(e.at)}</span>
          <Icon name={l.icon} size={14} />
          <span class="text">{l.text}</span>
        </a>
      {:else}
        <div class="empty muted">还没有事件。在任意渠道发条消息试试。</div>
      {/each}
    </div>
  </div>
{/if}

<button class="bar" aria-label="实时活动" title="实时活动" onclick={() => (open = !open)}>
  <span class="live" class:busy={busy > 0}></span>
  {#if busy}
    <span class="busy-text">{busy} 个会话正在处理</span>
  {/if}
  {#if latest}
    <span class="time">{clock(latest.e.at)}</span>
    <span class="text {latest.l.tone ?? ''}">{latest.l.text}</span>
  {:else}
    <span class="text muted">等待活动……</span>
  {/if}
  <Icon name={open ? 'down' : 'activity'} size={14} />
</button>

<style>
  .bar {
    height: 30px;
    flex: none;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 16px;
    border: none;
    border-top: 1px solid var(--line);
    background: color-mix(in srgb, var(--panel-solid) 55%, transparent);
    color: var(--text-2);
    font-size: 12px;
    text-align: left;
  }
  .live {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--muted);
  }
  .live.busy {
    background: var(--accent);
    box-shadow: 0 0 10px var(--accent);
    animation: blink 1s infinite;
  }
  .busy-text {
    color: var(--accent-2);
  }
  .time {
    color: var(--muted);
    font-family: var(--mono);
    font-size: 11px;
  }
  .text {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ok {
    color: var(--ok);
  }
  .err {
    color: var(--err);
  }
  .warn {
    color: var(--warn);
  }
  .accent {
    color: var(--accent-2);
  }
  .panel {
    position: absolute;
    left: 72px;
    right: 0;
    bottom: 30px;
    height: 320px;
    display: flex;
    flex-direction: column;
    background: var(--panel-solid);
    border-top: 1px solid var(--line-2);
    box-shadow: 0 -12px 40px rgba(0, 0, 0, 0.25);
    z-index: 20;
    animation: rise 0.2s ease;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 16px;
    border-bottom: 1px solid var(--line);
    font-size: 13px;
  }
  .head .muted {
    flex: 1;
    font-size: 12px;
  }
  .list {
    flex: 1;
    overflow: auto;
    padding: 6px 8px;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 5px 8px;
    border-radius: 7px;
    color: var(--text-2);
    text-decoration: none;
    font-size: 12.5px;
  }
  .row[href]:hover {
    background: var(--panel-2);
  }
  .empty {
    padding: 30px;
    text-align: center;
  }
</style>
