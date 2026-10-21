<script lang="ts">
  import { listDir, splitTyping, toTilde, type DirEntry } from '../lib/fs';
  import DirPicker from './DirPicker.svelte';
  import Icon from './Icon.svelte';

  // 目录列表：已选的目录显示成标签；输入时自动列出匹配的子目录，也可以点“浏览”一层层选
  let {
    values,
    onchange,
    placeholder = '输入路径，比如 ~/projects，会自动提示',
    first = true,
  }: { values: string[]; onchange: (v: string[]) => void; placeholder?: string; first?: boolean } = $props();

  let text = $state('');
  let open = $state(false);
  let sel = $state(0);
  let items = $state<{ path: string; name: string; blocked?: boolean }[]>([]);
  let note = $state('');
  let picking = $state(false);
  let timer = 0;
  let seq = 0;

  function add(v: string) {
    v = v.trim().replace(/(.)\/+$/, '$1');
    text = '';
    open = false;
    if (v && !values.includes(v)) onchange([...values, v]);
  }

  function remove(i: number) {
    onchange(values.filter((_, j) => j !== i));
  }

  function suggest() {
    clearTimeout(timer);
    timer = window.setTimeout(async () => {
      const sp = splitTyping(text);
      if (!sp) {
        items = [];
        note = '要写绝对路径（/ 开头）或者 ~ 开头';
        open = true;
        return;
      }
      const my = ++seq;
      try {
        const l = await listDir(sp.parent);
        if (my !== seq) return;
        const base = sp.parent === '~' ? '~' : toTilde(l.path, l.home);
        const pre = sp.prefix.toLowerCase();
        const match = (e: DirEntry) => (pre.startsWith('.') || !e.name.startsWith('.')) && e.name.toLowerCase().startsWith(pre);
        const contains = (e: DirEntry) => !match(e) && pre && !e.name.startsWith('.') && e.name.toLowerCase().includes(pre);
        const list = [...l.entries.filter(match), ...l.entries.filter(contains)].slice(0, 12);
        items = list.map((e) => ({ name: e.name, blocked: e.blocked, path: (base === '/' ? '' : base) + '/' + e.name }));
        note = list.length ? '' : '没有匹配的子目录';
        sel = 0;
        open = true;
      } catch (err) {
        if (my !== seq) return;
        items = [];
        note = err instanceof Error ? err.message : String(err);
        open = true;
      }
    }, 120);
  }

  function onkey(e: KeyboardEvent) {
    if (e.key === 'ArrowDown' && open) {
      e.preventDefault();
      sel = Math.min(sel + 1, items.length - 1);
    } else if (e.key === 'ArrowUp' && open) {
      e.preventDefault();
      sel = Math.max(sel - 1, 0);
    } else if (e.key === 'Tab' && open && items[sel]) {
      // 补全到这个目录，继续往下选
      e.preventDefault();
      text = items[sel].path + '/';
      suggest();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (open && items[sel] && !items[sel].blocked && text.trim() !== items[sel].path) add(items[sel].path);
      else add(text);
    } else if (e.key === 'Escape') {
      open = false;
    } else if (e.key === 'Backspace' && !text && values.length) {
      remove(values.length - 1);
    }
  }
</script>

<div class="dirs">
  <div class="box">
    {#each values as v, i}
      <span class="chip" title={v}>
        {#if i === 0 && first}<i class="first" title="第一个是默认目录，命令从这里开始执行"></i>{/if}
        <span class="v">{v}</span>
        <button onclick={() => remove(i)} aria-label="移除"><Icon name="x" size={11} /></button>
      </span>
    {/each}
    <input
      bind:value={text}
      oninput={suggest}
      onfocus={() => text && suggest()}
      onkeydown={onkey}
      onblur={() => setTimeout(() => (open = false), 150)}
      {placeholder}
      spellcheck="false"
    />
    <button class="browse" onclick={() => (picking = true)} title="浏览目录"><Icon name="folder" size={14} />浏览…</button>
  </div>

  {#if open}
    <div class="drop">
      {#each items as it, i (it.path)}
        <button
          class="opt"
          class:sel={i === sel}
          class:blocked={it.blocked}
          onmouseenter={() => (sel = i)}
          onmousedown={(e) => {
            e.preventDefault();
            if (!it.blocked) add(it.path);
          }}
        >
          <Icon name="folder" size={14} />
          <span class="p">{it.path}</span>
          {#if it.blocked}<span class="tag warn">含 nano-agent</span>{:else}<span class="hint">Tab 进入 · Enter 添加</span>{/if}
        </button>
      {/each}
      {#if note}<div class="note">{note}</div>{/if}
    </div>
  {/if}
</div>

{#if picking}
  <DirPicker
    start={splitTyping(text)?.parent ?? '~'}
    onpick={(p) => {
      picking = false;
      add(p);
    }}
    onclose={() => (picking = false)}
  />
{/if}

<style>
  .dirs {
    position: relative;
  }
  .box {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    padding: 6px;
    min-height: 38px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
  }
  .box:focus-within {
    border-color: var(--accent);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    max-width: 100%;
    height: 24px;
    padding: 0 4px 0 9px;
    border-radius: 7px;
    background: var(--panel-2);
    border: 1px solid var(--line-2);
    font-family: var(--mono);
    font-size: 12px;
  }
  .v {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .first {
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: var(--c-kernel);
  }
  .chip button {
    display: grid;
    place-items: center;
    width: 18px;
    height: 18px;
    border: none;
    border-radius: 5px;
    background: none;
    color: var(--muted);
  }
  .chip button:hover {
    background: color-mix(in srgb, var(--err) 18%, transparent);
    color: var(--err);
  }
  input {
    flex: 1;
    min-width: 160px;
    border: none;
    outline: none;
    background: none;
    font-family: var(--mono);
    font-size: 12.5px;
    padding: 0 4px;
  }
  .browse {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    height: 26px;
    padding: 0 9px;
    border-radius: 7px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    color: var(--c-dir);
    font-size: 12px;
  }
  .browse:hover {
    border-color: var(--c-dir);
  }
  .drop {
    position: absolute;
    left: 0;
    right: 0;
    top: calc(100% + 4px);
    z-index: 30;
    max-height: 300px;
    overflow: auto;
    padding: 4px;
    border-radius: 11px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow);
  }
  .opt {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 9px;
    border: none;
    border-radius: 8px;
    background: none;
    text-align: left;
    color: var(--c-dir);
  }
  .opt.sel {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .opt.blocked {
    opacity: 0.55;
  }
  .p {
    flex: 1;
    min-width: 0;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .hint {
    font-size: 11px;
    color: var(--muted);
  }
  .note {
    padding: 8px 10px;
    font-size: 12px;
    color: var(--muted);
  }
</style>
