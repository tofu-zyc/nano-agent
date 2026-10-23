<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from './lib/state.svelte';
  import Login from './components/Login.svelte';
  import Rail from './components/Rail.svelte';
  import TopBar from './components/TopBar.svelte';
  import Palette from './components/Palette.svelte';
  import Inspector from './components/Inspector.svelte';
  import Toasts from './components/Toasts.svelte';
  import ActivityBar from './components/ActivityBar.svelte';
  import Topology from './views/Topology.svelte';
  import Sessions from './views/Sessions.svelte';
  import Personas from './views/Personas.svelte';
  import Settings from './views/Settings.svelte';
  import Devices from './views/Devices.svelte';
 import Schedules from './views/Schedules.svelte';

  onMount(() => {
    store.boot();
    const onKey = (e: KeyboardEvent) => {
      if (e.isComposing) return; // 输入法正在选字，Esc 是取消候选，不是关面板
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        store.palette = !store.palette;
      } else if (e.key === 'Escape') {
        // 先让当前打开的菜单/弹窗处理 Esc；它们都不处理时，才收起右侧设置面板。
        // 必须等这一轮 keydown 的所有监听器都跑完（用 setTimeout，不能用微任务：
        // 微任务会在两个监听器之间执行），再看有没有人 preventDefault。
        setTimeout(() => {
          if (e.defaultPrevented) return;
          if (store.palette) store.palette = false;
          else if (store.inspector) store.inspector = null;
        }, 0);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  });
</script>

{#if store.authed === false}
  <Login />
{:else if store.authed && store.ov}
  <div class="shell" class:with-inspector={!!store.inspector}>
    <Rail />
    <div class="main">
      <TopBar />
      <div class="stage">
        {#if store.route.view === 'sessions'}
          <Sessions />
        {:else if store.route.view === 'personas'}
          <Personas />
        {:else if store.route.view === 'settings'}
          <Settings />
        {:else if store.route.view === 'schedules'}
 <Schedules />
 {:else if store.route.view === 'devices'}
          <Devices />
        {:else}
          <Topology />
        {/if}
        {#if store.inspector}
          <Inspector />
        {/if}
      </div>
      <ActivityBar />
    </div>
  </div>
  {#if store.palette}
    <Palette />
  {/if}
  {#if store.restarting}
    <div class="restarting"><div class="orb small"></div><div>网关正在重启，页面会自动重连…</div></div>
  {/if}
  <Toasts />
{:else}
  <div class="boot">
    <div class="orb"></div>
  </div>
{/if}

<style>
  .shell {
    display: flex;
    height: 100%;
    background:
      radial-gradient(1200px 700px at 70% -10%, color-mix(in srgb, var(--accent) 9%, transparent), transparent 60%),
      radial-gradient(900px 600px at -10% 110%, color-mix(in srgb, var(--c-kernel) 6%, transparent), transparent 60%),
      var(--bg);
  }
  .main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .stage {
    flex: 1;
    min-height: 0;
    position: relative;
    display: flex;
  }
  .boot {
    height: 100%;
    display: grid;
    place-items: center;
  }
  .orb {
    width: 34px;
    height: 34px;
    border-radius: 50%;
    border: 3px solid var(--accent);
    border-top-color: transparent;
    animation: spin 0.9s linear infinite;
  }
  .orb.small {
    width: 18px;
    height: 18px;
    border-width: 2px;
  }
  .restarting {
    position: fixed;
    right: 18px;
    bottom: 18px;
    z-index: 20;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 14px;
    border: 1px solid var(--line);
    border-radius: 12px;
    background: var(--panel-solid);
    color: var(--muted);
    font-size: 13px;
    box-shadow: 0 8px 30px rgba(0, 0, 0, 0.18);
  }
</style>
