<script lang="ts">
  // 人格编辑器：从 Agent 节点或会话节点打开，盖在页面上。左边是人格文件和自带人格，右边编辑。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { ago } from '../../lib/format';
  import { markdown } from '../../lib/markdown';
  import { portal } from '../../lib/portal';
  import type { Persona, PersonaPreset } from '../../lib/types';
  import Icon from '../Icon.svelte';

  let items = $state<Persona[]>([]);
  let dir = $state('');
  let text = $state('');
  let saved = $state('');
  let exists = $state(false);
  let preview = $state(false);
  let busy = $state(false);
  let presets = $state<PersonaPreset[]>([]);

  const name = $derived(store.persona?.name ?? 'default.md');
  // 正在编辑某个 agent 自己的人格：只有一个人格文件，左边不列会话专属的那些
  const agentId = $derived(store.persona?.agent ?? '');
  const agentName = $derived(store.agent(agentId)?.name ?? agentId);
  const dirty = $derived(text !== saved || !exists);
  const current = $derived(items.find((i) => i.name === name));
  const same = (a: string, b: string) => a.trim() === b.trim();
  const matched = $derived(presets.find((p) => same(p.text, text)));
  const sidOf = (n: string) => (n === 'default.md' ? '' : decodeURIComponent(n.replace(/\.md$/, '')));
  const titleOf = (n: string) => {
    if (agentId) return agentName + ' 的人格';
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
    preview = false;
    if (agentId) {
      try {
        const r = await api.get<{ text: string }>('/api/agents/' + encodeURIComponent(agentId) + '/persona');
        text = saved = r.text ?? '';
        exists = text.trim() !== '';
      } catch {
        text = saved = '';
        exists = false;
      }
      return;
    }
    try {
      const r = await api.get<{ text: string }>('/api/personas/' + encodeURIComponent(n));
      text = saved = r.text;
      exists = true;
    } catch {
      // 新建的会话人格：从默认人格复制一份作为起点
      exists = false;
      saved = '';
      try {
        text = (await api.get<{ text: string }>('/api/personas/default.md')).text;
      } catch {
        text = '';
      }
    }
  }

  api.get<{ items: PersonaPreset[] }>('/api/persona-presets').then((r) => (presets = r.items)).catch(() => (presets = []));

  $effect(() => {
    store.personaTick;
    loadList();
  });
  $effect(() => {
    loadFile(name);
  });

  function usePreset(p: PersonaPreset) {
    if (same(p.text, text)) return;
    if (text.trim() && !matched && !confirm(`用「${p.name}」替换编辑框里现在的内容？保存后才生效。`)) return;
    text = p.text;
    preview = false;
    store.toast({ kind: 'ok', text: `已填入「${p.name}」，保存后生效` });
  }

  function leave(): boolean {
    return !(exists && text !== saved) || confirm('有没保存的修改，不要了？');
  }
  function open(n: string) {
    if (n === name || !leave()) return;
    store.persona = { name: n, isNew: false };
  }
  function close() {
    if (leave()) store.persona = null;
  }

  async function save() {
    busy = true;
    try {
      if (agentId) {
        await api.put('/api/agents/' + encodeURIComponent(agentId) + '/persona', { text });
      } else {
        await api.put('/api/personas/' + encodeURIComponent(name), { text });
      }
      saved = text;
      exists = text.trim() !== '';
      store.toast({ kind: 'ok', text: '已保存，下一条消息开始生效' });
      if (!agentId) loadList();
      store.refresh().catch(() => {});
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
      saved = text;
      store.persona = { name: 'default.md', isNew: false };
    } catch (err) {
      store.fail(err);
    }
  }

  function onkey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === 's') {
      e.preventDefault();
      if (dirty) save();
    } else if (e.key === 'Escape' && !e.isComposing) {
      e.preventDefault();
      close();
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="backdrop" use:portal onclick={(e) => e.target === e.currentTarget && close()} role="presentation">
  <div class="sheet" role="dialog" aria-label="编辑人格">
    <aside class="list">
      {#if !agentId}
      <div class="head">
        <strong>人格文件</strong>
        <span class="muted small">{dir}</span>
      </div>
      {#each items as it (it.name)}
        <button class="item" class:active={it.name === name} onclick={() => open(it.name)}>
          <span class="ic" class:def={it.name === 'default.md'}><Icon name={it.name === 'default.md' ? 'persona' : 'chat'} size={15} /></span>
          <span class="txt">
            <span class="t">{titleOf(it.name)}</span>
            <span class="s">{it.sessions.length} 个会话在用 · {ago(it.mod)}修改</span>
          </span>
        </button>
      {/each}
      {#if !exists}
        <div class="item active">
          <span class="ic"><Icon name="plus" size={15} /></span>
          <span class="txt"><span class="t">{titleOf(name)}</span><span class="s">新建 · 还没保存</span></span>
        </div>
      {/if}
      {/if}
      {#if presets.length}
        <div class="head presets">
          <strong>自带人格</strong>
          <span class="muted small">{agentId ? '点一下填进这个 agent 的人格，保存后生效' : '点一下填进「' + titleOf(name) + '」，保存后生效'}</span>
        </div>
        {#each presets as p (p.id)}
          <button class="preset" class:on={matched?.id === p.id} onclick={() => usePreset(p)}>
            <span class="t">{p.name}{#if matched?.id === p.id}<span class="ptag">编辑框里就是它</span>{/if}</span>
            <span class="s">{p.desc}</span>
          </button>
        {/each}
      {/if}
      {#if agentId}
        <div class="head presets">
          <strong>再要一个人格？</strong>
        </div>
        <p class="muted small hint">每个人格就是一个 agent：拓扑图 Agent 列的「＋ 添加 agent」，再给它写人格。只想让某个会话换个人格，去那个会话的「人格」里新建。</p>
      {/if}
    </aside>

    <section class="editor">
      <header>
        <div class="ttl">
          <h3>{titleOf(name)}</h3>
          <div class="muted small">
            <code>{name}</code>
            {#if current}· {current.sessions.length} 个会话在用{/if}
            {#if matched}· 自带人格「{matched.name}」{/if}
            {#if !exists}· <span class="warnt">新文件，保存后生效</span>{:else if dirty}· <span class="warnt">有未保存的修改</span>{/if}
          </div>
        </div>
        <div class="seg">
          <button class:active={!preview} onclick={() => (preview = false)}>编辑</button>
          <button class:active={preview} onclick={() => (preview = true)}>预览</button>
        </div>
        {#if !agentId && name !== 'default.md' && exists}
          <button class="btn ghost danger" title="删除这个会话的专属人格" onclick={remove}><Icon name="trash" size={14} /></button>
        {/if}
        <button class="btn primary" onclick={save} disabled={!dirty || busy}><Icon name="check" size={14} />保存</button>
        <button class="btn ghost" onclick={close} aria-label="关闭"><Icon name="x" size={15} /></button>
      </header>
      {#if preview}
        <div class="preview">{@html markdown(text)}</div>
      {:else}
        <textarea bind:value={text} spellcheck="false" placeholder="写下机器人的性格、说话方式、要遵守的规矩……"></textarea>
      {/if}
      <footer class="muted small">{Array.from(text).length} 字 · Ctrl/⌘ S 保存 · 保存后下一条消息就生效，不用重启；人格改动大的话，可以清空相关会话的上下文。</footer>
    </section>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 40;
    display: grid;
    place-items: center;
    padding: 24px;
    background: color-mix(in srgb, var(--bg) 60%, transparent);
    backdrop-filter: blur(4px);
  }
  .sheet {
    width: min(1100px, 100%);
    height: min(820px, 100%);
    display: flex;
    border-radius: 18px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.4);
    overflow: hidden;
    animation: rise 0.2s ease;
  }
  .list {
    width: 270px;
    flex: none;
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 14px 8px;
    border-right: 1px solid var(--line);
    overflow: auto;
  }
  .head {
    display: flex;
    flex-direction: column;
    padding: 2px 8px 10px;
  }
  .hint {
    margin: 2px 10px 10px;
    line-height: 1.5;
  }
  .head.presets {
    margin-top: 16px;
    padding-bottom: 6px;
  }
  .small {
    font-size: 11.5px;
    word-break: break-all;
  }
  .item {
    display: flex;
    gap: 10px;
    padding: 9px 10px;
    border: none;
    border-radius: 11px;
    background: none;
    color: inherit;
    text-align: left;
    font: inherit;
    cursor: pointer;
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
    color: var(--c-agent);
    background: color-mix(in srgb, var(--c-agent) 16%, transparent);
  }
  .txt {
    min-width: 0;
    display: flex;
    flex-direction: column;
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
  .preset {
    display: flex;
    flex-direction: column;
    width: 100%;
    margin: 0 0 6px;
    padding: 9px 10px;
    border: 1px solid var(--line);
    border-radius: 11px;
    background: none;
    color: inherit;
    text-align: left;
    font: inherit;
    cursor: pointer;
  }
  .preset:hover {
    background: var(--panel-2);
  }
  .preset.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 8%, transparent);
  }
  .preset .s {
    margin-top: 2px;
    line-height: 1.5;
  }
  .ptag {
    margin-left: 6px;
    font-size: 11px;
    font-weight: 500;
    color: var(--accent-2);
  }
  .editor {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    padding: 16px 18px 12px 22px;
    gap: 12px;
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .ttl {
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
    background: var(--bg-2);
    font-family: var(--mono);
    font-size: 13.5px;
    line-height: 1.75;
    outline: none;
    resize: none;
    color: var(--text);
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
  @media (max-width: 760px) {
    .list {
      display: none;
    }
  }
</style>
