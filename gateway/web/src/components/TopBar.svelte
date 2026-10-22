<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { num, pct } from '../lib/format';
  import Icon from './Icon.svelte';

  const titles: Record<string, string> = { topology: '拓扑', sessions: '会话', personas: '人格', settings: '设置', devices:'设备与服务器' };
  const ov = $derived(store.ov!);
  const t = $derived(ov.totals);
  const isMac = navigator.platform.toLowerCase().includes('mac');
</script>

<header class="top">
  <div class="title">
    <h2>{titles[store.route.view]}</h2>
    {#if !store.connected}<span class="tag warn">实时连接断开，重连中…</span>{/if}
  </div>

  <button class="search" onclick={() => (store.palette = true)}>
    <Icon name="search" size={15} />
    <span>搜索设置、会话、操作…</span>
    <span class="kbd">{isMac ? '⌘' : 'Ctrl'} K</span>
  </button>

  <div class="pills">
    <button class="pill" onclick={() => store.open({ type: 'kernel' })} title="内核（节点）">
      <span class="dot {ov.kernels.length ? 'online' : 'error'}"></span>
      {#if ov.kernels.length === 1}
        内核 · {ov.kernels[0].node}
        {#if ov.kernels[0].landlock > 0}<Icon name="shield" size={13} class="shield" />{/if}
      {:else if ov.kernels.length}
        {ov.kernels.length} 个内核在线
      {:else}
        内核未连接
      {/if}
    </button>
    <button class="pill" onclick={() => store.open({ type: 'model' })} title="模型">
      <Icon name="sparkles" size={14} class="spark" />
      {ov.model.model || '未配置模型'}
    </button>
    <button class="pill" onclick={() => store.open({ type: 'gateway' })} title="启动以来的前缀缓存命中率">
      <Icon name="zap" size={14} class="zap" />
      缓存 {pct(t.cached_tokens, t.prompt_tokens)}
      <span class="muted">· {num(t.prompt_tokens)} tokens</span>
    </button>
  </div>
</header>

<style>
  .top {
    height: 58px;
    flex: none;
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 0 18px 0 22px;
    border-bottom: 1px solid var(--line);
    background: color-mix(in srgb, var(--panel-solid) 45%, transparent);
    backdrop-filter: blur(12px);
    z-index: 4;
  }
  .title {
    display: flex;
    align-items: center;
    gap: 10px;
    min-width: 120px;
  }
  h2 {
    margin: 0;
    font-size: 16px;
    font-weight: 600;
  }
  .search {
    flex: 1;
    max-width: 460px;
    margin: 0 auto;
    height: 36px;
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 0 12px;
    border-radius: 11px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--muted);
    transition: border-color 0.15s, color 0.15s;
  }
  .search:hover {
    border-color: color-mix(in srgb, var(--accent) 45%, var(--line));
    color: var(--text-2);
  }
  .search span:nth-child(2) {
    flex: 1;
    text-align: left;
  }
  .pills {
    display: flex;
    gap: 8px;
  }
  .pill {
    height: 30px;
    display: inline-flex;
    align-items: center;
    gap: 7px;
    padding: 0 11px;
    border-radius: 15px;
    border: 1px solid var(--line);
    background: var(--panel);
    font-size: 12.5px;
    color: var(--text-2);
    white-space: nowrap;
    transition: border-color 0.15s, color 0.15s;
  }
  .pill:hover {
    color: var(--text);
    border-color: var(--line-2);
  }
  .pill :global(.shield) {
    color: var(--c-kernel);
  }
  .pill :global(.spark) {
    color: var(--c-model);
  }
  .pill :global(.zap) {
    color: var(--accent-2);
  }
  @media (max-width: 1100px) {
    .pills .pill:nth-child(3) {
      display: none;
    }
  }
  @media (max-width: 800px) {
    .pills {
      display: none;
    }
  }
</style>
