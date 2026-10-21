<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { ago } from '../lib/format';
  import { markdown } from '../lib/markdown';
  import type { Persona } from '../lib/types';
  import Icon from '../components/Icon.svelte';

  let items = $state<Persona[]>([]);
  let dir = $state('');
  let text = $state('');
  let saved = $state('');
  let exists = $state(false);
  let preview = $state(false);
  let busy = $state(false);

  const name = $derived(store.route.arg ?? 'default.md');
  const isNew = $derived(location.hash.includes('new=1'));
  const dirty = $derived(text !== saved || !exists);
  const current = $derived(items.find((i) => i.name === name));
  const sidOf = (n: string) => (n === 'default.md' ? '' : decodeURIComponent(n.replace(/\.md$/, '')));
  const titleOf = (n: string) => {
    if (n === 'default.md') return '默认人格';
    const sid = sidOf(n);
    return (store.session(sid)?.title ?? sid) + ' 的专属人格';
  };

  async function loadList() {
    try {
      const r = await api.get<{ dir: string; items: Persona[] }>('/api/personas');
      items = r.items;
      dir = r.dir;
    } catch (err) {
      store.fail(err);
    }
  }

  async function loadFile(n: string) {
    try {
      const r = await api.get<{ text: string }>('/api/personas/' + encodeURIComponent(n));
      text = saved = r.text;
      exists = true;
    } catch {
      // 新建的会话人格：从默认人格复制一份作为起点
      exists = false;
      saved = '';
      try {
        const d = await api.get<{ text: string }>('/api/personas/default.md');
        text = d.text;
      } catch {
        text = '';
      }
    }
  }

  $effect(() => {
    store.personaTick;
    loadList();
  });
  $effect(() => {
    loadFile(name);
  });

  async function save() {
    busy = true;
    try {
      await api.put('/api/personas/' + encodeURIComponent(name), { text });
      saved = text;
      exists = true;
      store.toast({ kind: 'ok', text: '已保存，下一条消息开始生效' });
      loadList();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  async function remove() {
    if (!confirm(`删除「${titleOf(name)}」？这个会话会改用默认人格。`)) return;
    try {
      await api.del('/api/personas/' + encodeURIComponent(name));
      location.hash = '#/personas/default.md';
    } catch (err) {
      store.fail(err);
    }
  }

  function onkey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === 's') {
      e.preventDefault();
      if (dirty) save();
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="personas">
  <aside class="list">
    <div class="head">
      <strong>人格文件</strong>
      <span class="muted small">{dir}</span>
    </div>
    {#each items as it (it.name)}
      <a class="item" class:active={it.name === name} href="#/personas/{encodeURIComponent(it.name)}">
        <div class="ic" class:def={it.name === 'default.md'}><Icon name={it.name === 'default.md' ? 'persona' : 'chat'} size={15} /></div>
        <div class="txt">
          <div class="t">{titleOf(it.name)}</div>
          <div class="s">{it.sessions.length} 个会话在用 · {ago(it.mod)}修改</div>
        </div>
      </a>
    {/each}
    {#if isNew && !exists}
      <div class="item active">
        <div class="ic"><Icon name="plus" size={15} /></div>
        <div class="txt">
          <div class="t">{titleOf(name)}</div>
          <div class="s">新建 · 还没保存</div>
        </div>
      </div>
    {/if}
    <p class="tip">
      每个会话默认用 <code>default.md</code>。要给某个群或某个人单独设定，到它的会话设置里点“给这个会话单独设定人格”。
    </p>
  </aside>

  <section class="editor">
    <header>
      <div>
        <h3>{titleOf(name)}</h3>
        <div class="muted small">
          <code>{name}</code>
          {#if current}· {current.sessions.length} 个会话在用{/if}
          {#if !exists}· <span class="warnt">新文件，保存后生效</span>{:else if dirty}· <span class="warnt">有未保存的修改</span>{/if}
        </div>
      </div>
      <div class="acts">
        <div class="seg">
          <button class:active={!preview} onclick={() => (preview = false)}>编辑</button>
          <button class:active={preview} onclick={() => (preview = true)}>预览</button>
        </div>
        {#if name !== 'default.md' && exists}
          <button class="btn ghost danger" onclick={remove}><Icon name="trash" size={14} /></button>
        {/if}
        <button class="btn primary" onclick={save} disabled={!dirty || busy}>
          <Icon name="check" size={14} />保存 <span class="kbd">⌘S</span>
        </button>
      </div>
    </header>
    {#if preview}
      <div class="preview">{@html markdown(text)}</div>
    {:else}
      <textarea bind:value={text} spellcheck="false" placeholder="写下机器人的性格、说话方式、要遵守的规矩……"></textarea>
    {/if}
    <footer class="muted small">
      {Array.from(text).length} 字 · 保存后下一条消息就生效，不用重启；人格改动大的话，可以清空相关会话的上下文。
    </footer>
  </section>
</div>

<style>
  .personas {
    flex: 1;
    min-width: 0;
    display: flex;
  }
  .list {
    width: 290px;
    flex: none;
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 12px 8px;
    border-right: 1px solid var(--line);
    overflow: auto;
  }
  .head {
    display: flex;
    flex-direction: column;
    padding: 2px 8px 10px;
  }
  .small {
    font-size: 11.5px;
    word-break: break-all;
  }
  .item {
    display: flex;
    gap: 10px;
    padding: 9px 10px;
    border-radius: 11px;
    text-decoration: none;
    color: inherit;
  }
  .item:hover {
    background: var(--panel-2);
  }
  .item.active {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .ic {
    width: 32px;
    height: 32px;
    flex: none;
    border-radius: 10px;
    display: grid;
    place-items: center;
    color: var(--c-session);
    background: color-mix(in srgb, var(--c-session) 14%, transparent);
  }
  .ic.def {
    color: var(--accent-2);
    background: color-mix(in srgb, var(--accent) 16%, transparent);
  }
  .txt {
    min-width: 0;
  }
  .t {
    font-weight: 600;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .s {
    font-size: 11.5px;
    color: var(--muted);
  }
  .tip {
    margin: 14px 8px 0;
    font-size: 12px;
    color: var(--muted);
    line-height: 1.6;
  }
  .editor {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    padding: 16px 22px 12px;
    gap: 12px;
  }
  header {
    display: flex;
    align-items: center;
    gap: 14px;
  }
  header > div:first-child {
    flex: 1;
    min-width: 0;
  }
  h3 {
    margin: 0 0 2px;
    font-size: 16px;
  }
  .warnt {
    color: var(--warn);
  }
  .acts {
    display: flex;
    gap: 8px;
    align-items: center;
  }
  .acts .kbd {
    margin-left: 4px;
    border-color: rgba(255, 255, 255, 0.35);
    color: rgba(255, 255, 255, 0.85);
  }
  .seg {
    display: inline-flex;
    padding: 3px;
    gap: 2px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
  }
  .seg button {
    height: 26px;
    padding: 0 12px;
    border: none;
    border-radius: 7px;
    background: none;
    color: var(--text-2);
    font-size: 12.5px;
  }
  .seg button.active {
    background: var(--panel-solid);
    color: var(--text);
    box-shadow: inset 0 0 0 1px var(--line-2);
  }
  textarea,
  .preview {
    flex: 1;
    min-height: 0;
    padding: 18px 20px;
    border-radius: 14px;
    border: 1px solid var(--line);
    background: var(--panel-solid);
    font-family: var(--mono);
    font-size: 13.5px;
    line-height: 1.75;
    outline: none;
    resize: none;
  }
  textarea:focus {
    border-color: color-mix(in srgb, var(--accent) 60%, var(--line));
  }
  .preview {
    overflow: auto;
    font-family: var(--font);
    font-size: 14px;
  }
  .preview :global(h4) {
    margin: 14px 0 6px;
    font-size: 15px;
    color: var(--accent-2);
  }
  .preview :global(ul) {
    padding-left: 20px;
  }
</style>
