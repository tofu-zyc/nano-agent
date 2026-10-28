<script lang="ts">
  import { onMount } from 'svelte';
  import { listDir, toTilde, join, isWindowsOS, isWindowsPath, type DirListing } from '../lib/fs';
  import Icon from './Icon.svelte';

  let {
    allowSelf = false,
    start = '~',
    host = '',
    onpick,
    onclose,
  }: { allowSelf?: boolean; start?: string; host?: string; onpick: (path: string) => void; onclose: () => void } = $props();

  let cur = $state<DirListing | null>(null);
  let error = $state('');
  let loading = $state(false);
  let hidden = $state(false);
  let filter = $state('');

  async function go(path: string) {
    loading = true;
    error = '';
    try {
      cur = await listDir(path, host);
      filter = '';
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    go(start).then(() => {
      if (error && start !== '~') go('~');
    });
  });

  const shown = $derived(
    (cur?.entries ?? []).filter((e) => (hidden || !e.name.startsWith('.')) && (!filter || e.name.toLowerCase().includes(filter.toLowerCase()))),
  );

  // 路径属于它所在的机器：Windows 上看盘符和反斜杠，不把 POSIX 规则套过去
  const win = $derived(isWindowsOS(cur?.os) || isWindowsPath(cur?.path ?? ''));
  const rootPath = $derived(win ? '' : '/');
  const rootLabel = $derived(win ? '盘符' : '/');

  // 面包屑：主目录下从 ~ 开始；Windows 从盘符开始
  const crumbs = $derived.by(() => {
    if (!cur) return [];
    if (!cur.path) return [{ label: '此电脑', path: '' }];
    const out: { label: string; path: string }[] = [];
    const t = toTilde(cur.path, cur.home);
    if (t === '~') {
      out.push({ label: '~', path: cur.home });
    } else if (t.startsWith('~')) {
      out.push({ label: '~', path: cur.home });
      let p = cur.home;
      for (const seg of t.slice(1).split(/[\\/]/).filter(Boolean)) {
        p = join(p, seg);
        out.push({ label: seg, path: p });
      }
    } else if (win) {
      const drive = cur.path.slice(0, 2);
      out.push({ label: drive, path: drive + '\\' });
      let p = drive + '\\';
      for (const seg of cur.path.slice(2).split(/[\\/]/).filter(Boolean)) {
        p = join(p, seg);
        out.push({ label: seg, path: p });
      }
    } else {
      out.push({ label: '/', path: '/' });
      let p = '';
      for (const seg of cur.path.split('/').filter(Boolean)) {
        p += '/' + seg;
        out.push({ label: seg, path: p });
      }
    }
    return out;
  });

  function up() {
    if (!cur || !cur.path) return;
    if (win) {
      if (/^[A-Za-z]:[\\/]?$/.test(cur.path)) {
        go('');
        return;
      }
      const m = cur.path.match(/^(.*)[\\/][^\\/]+$/);
      go(m ? m[1] : '');
      return;
    }
    if (cur.path === '/') return;
    go(cur.path.replace(/\/[^/]+$/, '') || '/');
  }

  function pick() {
    if (cur && (!cur.blocked || allowSelf)) onpick(toTilde(cur.path, cur.home));
  }

  function onkey(e: KeyboardEvent) {
    if (e.isComposing) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      onclose();
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="backdrop" onclick={onclose}>
  <div class="picker" onclick={(e) => e.stopPropagation()}>
    <header>
      <Icon name="folder" size={17} />
      <strong>选择目录</strong>
      <span class="muted small">{host ? `${host} 上的目录` : '本机（网关所在的机器）上的目录'}</span>
      <span class="grow"></span>
      <button class="btn sm ghost" onclick={onclose} aria-label="关闭"><Icon name="x" size={14} /></button>
    </header>

    <div class="nav">
      <button class="btn sm" onclick={up} disabled={!cur || !cur.path} title="上一级"><Icon name="right" size={13} class="flip" /></button>
      <div class="crumbs">
        {#each crumbs as c, i}
          {#if i > 0}<span class="sep">{win ? '\\' : '/'}</span>{/if}
          <button class="crumb" class:last={i === crumbs.length - 1} onclick={() => go(c.path)}>{c.label}</button>
        {/each}
      </div>
      <button class="btn sm ghost" onclick={() => go('~')} title="主目录">~</button>
      <button class="btn sm ghost" onclick={() => go(rootPath)} title={win ? '盘符' : '根目录'}>{rootLabel}</button>
    </div>

    <div class="tools">
      <div class="search">
        <Icon name="search" size={13} />
        <input bind:value={filter} placeholder="筛选" />
      </div>
      <label><input type="checkbox" bind:checked={hidden} />显示隐藏目录</label>
    </div>

    <div class="list">
      {#if error}
        <div class="err"><Icon name="alert" size={14} />{error}</div>
      {:else if loading && !cur}
        <div class="muted pad">读取中…</div>
      {:else}
        {#each shown as e (e.name)}
          <button class="row" class:blocked={e.blocked} onclick={() => cur && go(join(cur.path, e.name))} title={e.blocked ? '包含 nano-agent 自己；只有明确关闭执行隔离时才能选择' : ''}>
            <Icon name="folder" size={15} />
            <span class="name">{e.name}</span>
            {#if e.blocked}<span class="tag warn">含 nano-agent</span>{/if}
            <Icon name="right" size={13} />
          </button>
        {:else}
          <div class="muted pad">{filter ? '没有匹配的目录' : '这里没有子目录'}</div>
        {/each}
        {#if cur?.truncated}<div class="muted pad small">目录太多，只列出了前 1000 个</div>{/if}
      {/if}
    </div>

    <footer>
      <code class="cur">{cur ? toTilde(cur.path, cur.home) : ''}</code>
      {#if cur?.blocked}<span class="warnt small">包含 nano-agent 自己；只有明确关闭执行隔离时才能选择</span>{/if}
      <span class="grow"></span>
      <button class="btn" onclick={onclose}>取消</button>
      <button class="btn primary" onclick={pick} disabled={!cur || (cur.blocked && !allowSelf)}><Icon name="check" size={14} />选择这个目录</button>
    </footer>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 70;
    display: grid;
    place-items: center;
    background: rgba(5, 7, 12, 0.5);
    backdrop-filter: blur(3px);
  }
  .picker {
    width: min(620px, calc(100vw - 32px));
    height: min(560px, calc(100vh - 60px));
    display: flex;
    flex-direction: column;
    border-radius: 16px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow);
    animation: rise 0.18s ease;
    overflow: hidden;
  }
  header,
  footer {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 12px 14px;
  }
  header {
    border-bottom: 1px solid var(--line);
    color: var(--c-dir);
  }
  header strong {
    color: var(--text);
  }
  footer {
    border-top: 1px solid var(--line);
  }
  .grow {
    flex: 1;
  }
  .small {
    font-size: 12px;
  }
  .nav {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 10px 14px 6px;
  }
  .nav :global(.flip) {
    transform: rotate(180deg);
  }
  .crumbs {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 1px;
    font-family: var(--mono);
    font-size: 12.5px;
  }
  .crumb {
    border: none;
    background: none;
    padding: 2px 5px;
    border-radius: 6px;
    color: var(--text-2);
  }
  .crumb:hover {
    background: var(--panel-2);
    color: var(--text);
  }
  .crumb.last {
    color: var(--text);
    font-weight: 600;
  }
  .sep {
    color: var(--muted);
  }
  .tools {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 4px 14px 8px;
    font-size: 12.5px;
    color: var(--text-2);
  }
  .tools label {
    display: flex;
    gap: 5px;
    align-items: center;
  }
  .tools input[type='checkbox'] {
    accent-color: var(--accent);
  }
  .search {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 7px;
    height: 30px;
    padding: 0 10px;
    border-radius: 9px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    color: var(--muted);
  }
  .search input {
    flex: 1;
    border: none;
    outline: none;
    background: none;
    color: var(--text);
  }
  .list {
    flex: 1;
    overflow: auto;
    padding: 4px 8px;
  }
  .row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 10px;
    border: none;
    border-radius: 9px;
    background: none;
    text-align: left;
    color: var(--c-dir);
  }
  .row:hover {
    background: var(--panel-2);
  }
  .row .name {
    flex: 1;
    color: var(--text);
    font-family: var(--mono);
    font-size: 13px;
  }
  .row.blocked {
    opacity: 0.6;
  }
  .pad {
    padding: 20px;
    text-align: center;
  }
  .err {
    display: flex;
    gap: 8px;
    align-items: center;
    margin: 12px;
    padding: 10px 12px;
    border-radius: 10px;
    color: var(--err);
    background: color-mix(in srgb, var(--err) 8%, transparent);
    font-size: 13px;
  }
  .cur {
    font-size: 12.5px;
    max-width: 260px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .warnt {
    color: var(--warn);
  }
</style>
