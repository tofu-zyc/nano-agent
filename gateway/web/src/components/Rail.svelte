<script lang="ts">
  import { store, type View } from '../lib/state.svelte';
  import Icon from './Icon.svelte';

  const items: { view: View; icon: string; label: string; hash: string }[] = [
    { view: 'topology', icon: 'graph', label: '拓扑', hash: '#/' },
    { view: 'sessions', icon: 'chat', label: '会话', hash: '#/sessions' },
    { view: 'personas', icon: 'persona', label: '人格', hash: '#/personas' },
    { view: 'settings', icon: 'sliders', label: '设置', hash: '#/settings' },
  ];

  const running = $derived(store.ov?.sessions?.filter((s) => s.running).length ?? 0);
  const pending = $derived(store.ov?.approvals.length ?? 0);
</script>

<nav class="rail">
  <a class="brand" href="#/" title="nano-agent">
    <svg viewBox="0 0 40 40" width="30" height="30">
      <circle cx="20" cy="20" r="15" fill="none" stroke="var(--accent)" stroke-width="2.4" stroke-dasharray="70 25" />
      <circle cx="20" cy="20" r="6" fill="var(--accent)" />
      <circle cx="35" cy="20" r="2.6" fill="var(--c-model)" />
    </svg>
  </a>
  {#each items as it}
    <a class="item" class:active={store.route.view === it.view} href={it.hash}>
      <Icon name={it.icon} size={19} />
      <span>{it.label}</span>
      {#if it.view === 'sessions' && (running || pending)}
        <i class="badge" class:warn={pending > 0}>{pending || running}</i>
      {/if}
    </a>
  {/each}
  <div class="grow"></div>
  <button
    class="item small"
    title="切换主题"
    onclick={() => store.setTheme(store.theme === 'auto' ? (store.dark ? 'light' : 'dark') : store.theme === 'dark' ? 'light' : 'auto')}
  >
    <Icon name={store.theme === 'auto' ? 'auto' : store.theme === 'dark' ? 'moon' : 'sun'} size={17} />
    <span>{store.theme === 'auto' ? '跟随' : store.theme === 'dark' ? '深色' : '浅色'}</span>
  </button>
  <button class="item small" title="退出登录" onclick={() => store.logout()}>
    <Icon name="logout" size={17} />
    <span>退出</span>
  </button>
</nav>

<style>
  .rail {
    width: 72px;
    flex: none;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 4px;
    padding: 12px 0;
    border-right: 1px solid var(--line);
    background: color-mix(in srgb, var(--panel-solid) 60%, transparent);
    backdrop-filter: blur(12px);
    z-index: 5;
  }
  .brand {
    display: grid;
    place-items: center;
    width: 44px;
    height: 44px;
    margin-bottom: 10px;
  }
  .brand svg circle:first-child {
    transform-origin: 20px 20px;
    animation: spin 14s linear infinite;
  }
  .item {
    position: relative;
    width: 58px;
    padding: 8px 0 6px;
    border-radius: 12px;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 3px;
    color: var(--muted);
    text-decoration: none;
    font-size: 11.5px;
    border: none;
    background: none;
    transition: color 0.15s, background 0.15s;
  }
  .item:hover {
    color: var(--text);
    background: var(--panel-2);
  }
  .item.active {
    color: var(--text);
    background: color-mix(in srgb, var(--accent) 16%, transparent);
  }
  .item.active::before {
    content: '';
    position: absolute;
    left: -7px;
    top: 12px;
    bottom: 12px;
    width: 3px;
    border-radius: 3px;
    background: var(--accent);
  }
  .item.small {
    font-size: 10.5px;
  }
  .grow {
    flex: 1;
  }
  .badge {
    position: absolute;
    top: 3px;
    right: 9px;
    min-width: 16px;
    height: 16px;
    padding: 0 4px;
    border-radius: 8px;
    font-style: normal;
    font-size: 10px;
    line-height: 16px;
    text-align: center;
    background: var(--accent);
    color: #fff;
  }
  .badge.warn {
    background: var(--warn);
    color: #241a00;
  }
</style>
