<script module lang="ts">
  export type MenuItem = { label: string; icon?: string; danger?: boolean; disabled?: boolean; hint?: string; run: () => void } | 'sep';
</script>

<script lang="ts">
  import { untrack } from 'svelte';
  import Icon from './Icon.svelte';

  // 右键菜单：在 (x, y) 处弹出，点菜单项或者点别处就收起
  let { x, y, title = '', items, onclose }: { x: number; y: number; title?: string; items: MenuItem[]; onclose: () => void } = $props();

  let el = $state<HTMLDivElement>();
  let pos = $state(untrack(() => ({ left: x, top: y })));
  $effect(() => {
    const w = el?.offsetWidth ?? 230;
    const h = el?.offsetHeight ?? 200;
    pos = {
      left: Math.max(8, Math.min(x, window.innerWidth - w - 8)),
      top: Math.max(8, Math.min(y, window.innerHeight - h - 8)),
    };
  });

  function onkey(e: KeyboardEvent) {
    if (e.isComposing) return;
    if (e.key === 'Escape') {
      e.preventDefault(); // 让 Esc 只关菜单，不再顺手关掉下层的设置面板
      onclose();
    }
  }
</script>

<svelte:window onkeydown={onkey} onblur={onclose} onresize={onclose} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="back"
  onclick={onclose}
  onpointerdown={(e) => e.stopPropagation()}
  onpointerup={(e) => e.stopPropagation()}
  onwheel={(e) => e.stopPropagation()}
  oncontextmenu={(e) => { e.preventDefault(); onclose(); }}
>
  <div class="menu" bind:this={el} style="left: {pos.left}px; top: {pos.top}px" role="menu" tabindex="-1" onclick={(e) => e.stopPropagation()}>
    {#if title}<div class="title">{title}</div>{/if}
    {#each items as it, i (i)}
      {#if it === 'sep'}
        <div class="sep"></div>
      {:else}
        <button class="item" class:danger={it.danger} disabled={it.disabled} role="menuitem" title={it.hint ?? ''}
          onclick={() => { onclose(); it.run(); }}>
          <span class="ico">{#if it.icon}<Icon name={it.icon} size={14} />{/if}</span>
          {it.label}
        </button>
      {/if}
    {/each}
  </div>
</div>

<style>
  .back {
    position: fixed;
    inset: 0;
    z-index: 80;
  }
  .menu {
    position: fixed;
    min-width: 200px;
    max-width: 300px;
    padding: 5px;
    border-radius: 12px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    box-shadow: var(--shadow);
    animation: pop 0.1s ease;
  }
  .title {
    padding: 6px 10px 5px;
    font-size: 11.5px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .item {
    display: flex;
    align-items: center;
    gap: 9px;
    width: 100%;
    padding: 7px 10px;
    border: 0;
    border-radius: 8px;
    background: none;
    color: var(--text);
    font: inherit;
    font-size: 13px;
    text-align: left;
    cursor: pointer;
  }
  .item:hover:not(:disabled) {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .item:disabled {
    opacity: 0.45;
    cursor: default;
  }
  .item.danger {
    color: var(--err);
  }
  .ico {
    width: 15px;
    display: grid;
    place-items: center;
    color: var(--muted);
  }
  .danger .ico {
    color: var(--err);
  }
  .sep {
    height: 1px;
    margin: 4px 6px;
    background: var(--line);
  }
  @keyframes pop {
    from {
      opacity: 0;
      transform: scale(0.97);
    }
  }
</style>
