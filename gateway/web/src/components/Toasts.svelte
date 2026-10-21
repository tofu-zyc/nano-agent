<script lang="ts">
  import { store } from '../lib/state.svelte';
  import Icon from './Icon.svelte';

  const icons = { info: 'activity', ok: 'check', err: 'alert', approval: 'shield', pair: 'link' } as const;
  // 正在看的那个会话里已经有审批卡片了、设备页上已经有配对请求了，不用再弹
  const shown = $derived(
    store.toasts.filter(
      (t) =>
        !(t.approval && store.route.view === 'sessions' && store.route.arg === t.approval.sid) &&
        !(t.pair && store.route.view === 'devices'),
    ),
  );
</script>

<div class="toasts">
  {#each shown as t (t.id)}
    <div class="toast {t.kind}">
      <Icon name={icons[t.kind]} size={16} class="ic" />
      <div class="body">
        <div class="text">{t.text}</div>
        {#if t.approval}
          {@const s = store.session(t.approval.sid)}
          <div class="who">{s?.title ?? t.approval.sid} · 发起人 {t.approval.from}</div>
          <pre>{t.approval.detail}</pre>
          <div class="acts">
            <button class="btn sm primary" onclick={() => store.approve(t.approval!.n, true)}><Icon name="check" size={13} />批准</button>
            <button class="btn sm" onclick={() => store.approve(t.approval!.n, false)}>拒绝</button>
            <a class="btn sm ghost" href="#/sessions/{encodeURIComponent(t.approval.sid)}">查看对话</a>
          </div>
        {:else if t.pair}
          <div class="who">来自 {t.pair.addr} · 确认码 <b class="code">{t.pair.code}</b></div>
          <div class="acts">
            <button class="btn sm primary" onclick={() => store.decidePair(t.pair!, true)}><Icon name="check" size={13} />允许</button>
            <button class="btn sm" onclick={() => store.decidePair(t.pair!, false)}>拒绝</button>
            <a class="btn sm ghost" href="#/devices">设备页</a>
          </div>
        {:else if t.detail}
          <div class="who">{t.detail}</div>
        {/if}
      </div>
      {#if !t.approval && !t.pair}
        <button class="close" onclick={() => store.dismiss(t.id)} aria-label="关闭"><Icon name="x" size={13} /></button>
      {/if}
    </div>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    right: 18px;
    bottom: 46px;
    display: flex;
    flex-direction: column;
    gap: 10px;
    z-index: 50;
    pointer-events: none;
  }
  .toast {
    pointer-events: auto;
    width: 340px;
    display: flex;
    gap: 10px;
    padding: 11px 12px;
    border-radius: 12px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow);
    animation: rise 0.25s ease;
  }
  .toast :global(.ic) {
    margin-top: 2px;
  }
  .toast.ok :global(.ic) {
    color: var(--ok);
  }
  .toast.err {
    border-color: color-mix(in srgb, var(--err) 45%, var(--line));
  }
  .toast.err :global(.ic) {
    color: var(--err);
  }
  .toast.approval {
    width: 400px;
    border-color: color-mix(in srgb, var(--warn) 55%, var(--line));
    box-shadow: var(--shadow), 0 0 0 4px color-mix(in srgb, var(--warn) 10%, transparent);
  }
  .toast.approval :global(.ic) {
    color: var(--warn);
  }
  .toast.pair {
    width: 370px;
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    box-shadow: var(--shadow), 0 0 0 4px color-mix(in srgb, var(--accent) 10%, transparent);
  }
  .toast.pair :global(.ic) {
    color: var(--accent-2);
  }
  .code {
    font-family: var(--mono);
    letter-spacing: 0.08em;
    color: var(--text);
  }
  .body {
    flex: 1;
    min-width: 0;
  }
  .text {
    font-weight: 600;
    font-size: 13.5px;
  }
  .who {
    font-size: 12px;
    color: var(--text-2);
    margin-top: 2px;
  }
  pre {
    margin: 8px 0;
    padding: 8px 10px;
    border-radius: 8px;
    background: var(--bg-2);
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 180px;
    overflow: auto;
  }
  .acts {
    display: flex;
    gap: 6px;
  }
  .close {
    border: none;
    background: none;
    color: var(--muted);
    height: 20px;
    padding: 0;
  }
</style>
