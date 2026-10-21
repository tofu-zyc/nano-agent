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

  onMount(() => {
    store.boot();
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        store.palette = !store.palette;
      } else if (e.key === 'Escape') {
        if (store.palette) store.palette = false;
        else if (store.inspector) store.inspector = null;
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
</style>
