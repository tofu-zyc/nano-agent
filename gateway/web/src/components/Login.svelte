<script lang="ts">
  import { store } from '../lib/state.svelte';

  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e: Event) {
    e.preventDefault();
    if (!password || busy) return;
    busy = true;
    error = '';
    try {
      await store.login(password);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="wrap">
  <form class="card" onsubmit={submit}>
    <div class="logo">
      <svg viewBox="0 0 120 120" width="88" height="88">
        <defs>
          <radialGradient id="lg" cx="50%" cy="45%" r="60%">
            <stop offset="0%" stop-color="var(--accent-2)" />
            <stop offset="100%" stop-color="var(--accent)" />
          </radialGradient>
        </defs>
        <circle cx="60" cy="60" r="46" class="ring" />
        <circle cx="60" cy="60" r="34" class="ring2" />
        <circle cx="60" cy="60" r="16" fill="url(#lg)" />
        <circle cx="106" cy="60" r="4.5" class="sat" />
      </svg>
    </div>
    <h1>nano-agent</h1>
    <p class="muted">网关管理界面。密码在 <code>data/web.secret</code>，或者用启动时打印的链接直接登录。</p>
    <!-- svelte-ignore a11y_autofocus -->
    <input class="input" type="password" placeholder="密码" bind:value={password} autofocus autocomplete="current-password" />
    {#if error}<div class="err">{error}</div>{/if}
    <button class="btn primary" disabled={busy || !password}>{busy ? '登录中…' : '登录'}</button>
  </form>
</div>

<style>
  .wrap {
    height: 100%;
    display: grid;
    place-items: center;
    background:
      radial-gradient(700px 500px at 50% 30%, color-mix(in srgb, var(--accent) 14%, transparent), transparent 70%),
      var(--bg);
  }
  .card {
    width: min(360px, calc(100vw - 32px));
    padding: 34px 30px 28px;
    border-radius: 20px;
    background: var(--panel);
    border: 1px solid var(--line);
    box-shadow: var(--shadow);
    display: flex;
    flex-direction: column;
    gap: 12px;
    text-align: center;
    animation: rise 0.4s ease;
  }
  .logo {
    display: grid;
    place-items: center;
    margin-bottom: 4px;
  }
  .ring {
    fill: none;
    stroke: color-mix(in srgb, var(--accent) 35%, transparent);
    stroke-width: 1.5;
    stroke-dasharray: 3 7;
    transform-origin: 60px 60px;
    animation: spin 18s linear infinite;
  }
  .ring2 {
    fill: none;
    stroke: color-mix(in srgb, var(--accent) 55%, transparent);
    stroke-width: 1.2;
  }
  .sat {
    fill: var(--c-model);
    transform-origin: 60px 60px;
    animation: spin 6s linear infinite;
  }
  h1 {
    margin: 0;
    font-size: 22px;
    letter-spacing: 0.5px;
  }
  p {
    margin: 0 0 8px;
    font-size: 13px;
  }
  .err {
    color: var(--err);
    font-size: 13px;
  }
  .btn {
    justify-content: center;
    height: 38px;
  }
  .input {
    height: 40px;
    text-align: center;
  }
</style>
