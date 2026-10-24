<script lang="ts">
  // 上下文与记忆：上面是每个 agent 的长期记忆（人物、记忆、自我认知、梦境日志，能看能搜能改），
  // 下面是上下文整理（什么时候整理、整理到多少、归档留多久）。
  import { untrack } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { markdown } from '../../lib/markdown';
  import type { MemoryItem, MemoryView } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import Icon from '../Icon.svelte';

  let { agent: wanted }: { agent?: string } = $props();

  const mi = $derived(store.ov?.memory);
  const agents = $derived(store.ov?.agents ?? []);
  let picked = $state('');
  const agentID = $derived(picked || wanted || agents.find((x) => x.default)?.id || 'default');
  const ag = $derived(store.agent(agentID));

  type Tab = 'people' | 'items' | 'self' | 'dreams';
  let tab = $state<Tab>('people');
  let view = $state<MemoryView | null>(null);
  let query = $state('');
  let hits = $state<MemoryItem[] | null>(null);
  let open = $state(''); // 正在看的记忆路径（'+new' 是新建）
  let raw = $state('');
  let selfText = $state('');
  let days = $state<string[]>([]);
  let day = $state('');
  let dreamText = $state('');
  let busy = $state(false);
  let err = $state('');

  const enc = encodeURIComponent;

  async function load() {
    err = '';
    try {
      view = await api.get<MemoryView>('/api/agents/' + enc(agentID) + '/memory');
      selfText = view.self;
    } catch (e) {
      view = null;
      err = e instanceof Error ? e.message : String(e);
    }
  }
  async function loadDreams(d = '') {
    try {
      const r = await api.get<{ days: string[]; text: string }>('/api/agents/' + enc(agentID) + '/dreams' + (d ? '?day=' + enc(d) : ''));
      days = r.days;
      dreamText = r.text;
      day = d || r.days[0] || '';
    } catch (e) {
      store.fail(e);
    }
  }
  // 换 agent、记忆有变化（网关推送）时重读
  $effect(() => {
    void agentID;
    void store.memoryTick;
    untrack(() => {
      load();
      if (tab === 'dreams') loadDreams(day);
    });
  });

  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  function onSearch() {
    clearTimeout(searchTimer);
    const q = query.trim();
    if (!q) {
      hits = null;
      return;
    }
    searchTimer = setTimeout(async () => {
      try {
        const r = await api.get<{ items: MemoryItem[] }>('/api/agents/' + enc(agentID) + '/memory/search?q=' + enc(q));
        hits = r.items;
      } catch (e) {
        store.fail(e);
      }
    }, 250);
  }

  const listed = $derived.by(() => {
    if (hits) return hits;
    const all = view?.entries ?? [];
    return tab === 'people' ? all.filter((x) => x.type === '人物') : all.filter((x) => x.type !== '人物');
  });

  async function toggle(it: MemoryItem) {
    if (open === it.path) {
      open = '';
      return;
    }
    try {
      const r = await api.get<{ raw: string }>('/api/agents/' + enc(agentID) + '/memory/item?path=' + enc(it.path));
      raw = r.raw;
      open = it.path;
    } catch (e) {
      store.fail(e);
    }
  }

  function startNew() {
    const people = tab === 'people';
    raw = `---\ntitle: ${people ? '名字' : '标题'}\ntype: ${people ? '人物' : '事实'}\nsummary: 一句话概括\nvisible: admin\n---\n\n${people ? '是谁、怎么称呼、和它的关系、性格、喜好、要紧的事。' : '正文'}\n`;
    open = '+new';
  }

  async function save() {
    busy = true;
    try {
      const path = open === '+new' ? '' : open;
      const it = await api.put<MemoryItem>('/api/agents/' + enc(agentID) + '/memory/item?path=' + enc(path), { raw });
      open = it.path;
      store.toast({ kind: 'ok', text: '记好了' }, 1600);
      await load();
    } catch (e) {
      store.fail(e);
    } finally {
      busy = false;
    }
  }

  async function invalidate(it: MemoryItem, restore: boolean) {
    const reason = restore ? '' : prompt('为什么过时了？（可以不写）', '') ?? null;
    if (reason === null) return;
    try {
      await api.post('/api/agents/' + enc(agentID) + '/memory/invalidate?path=' + enc(it.path), { reason, restore });
      await load();
      if (hits) onSearch();
    } catch (e) {
      store.fail(e);
    }
  }

  async function remove(it: MemoryItem) {
    if (!confirm(`真的删掉「${it.title}」？（一般标成过时就够了，还能找回）`)) return;
    try {
      await api.del('/api/agents/' + enc(agentID) + '/memory/item?path=' + enc(it.path));
      open = '';
      await load();
      if (hits) onSearch();
    } catch (e) {
      store.fail(e);
    }
  }

  async function saveSelf() {
    busy = true;
    try {
      await api.put('/api/agents/' + enc(agentID) + '/memory/self', { text: selfText });
      store.toast({ kind: 'ok', text: '自我认知保存了' }, 1600);
    } catch (e) {
      store.fail(e);
    } finally {
      busy = false;
    }
  }

  async function dreamNow() {
    busy = true;
    try {
      await api.post('/api/agents/' + enc(agentID) + '/dream', {});
      store.toast({ kind: 'ok', text: '去整理记忆了，整理完梦境日志里能看到' }, 2200);
    } catch (e) {
      store.fail(e);
    } finally {
      busy = false;
    }
  }

  function setTab(t: Tab) {
    tab = t;
    open = '';
    if (t === 'dreams') loadDreams();
  }

  const visLabel = (v?: string) => (!v || v === 'all' ? '谁在都能用' : v === 'admin' ? '只在主人的会话里' : '只在和 ' + v + ' 的私聊里');

  function pctText(key: string, def: number) {
    const v = Number(store.field(key)?.value);
    return Math.round((v > 0 ? v : def) * 100) + '%';
  }
  const idle = $derived.by(() => {
    const v = String(store.field('context.idle_compact')?.value ?? '30m');
    if (v === '0' || v === '0s') return '空闲时不整理';
    const m = v.match(/^(\d+)(m|h)$/);
    return m ? `空闲 ${m[1]} ${m[2] === 'h' ? '小时' : '分钟'}后整理一次` : `空闲 ${v} 后整理一次`;
  });
</script>

<div class="pnl">
  <h5 class="p-h">长期记忆 <span class="muted p-small">每个 agent 记住的人、事、知识；睡着或空闲时自己整理</span></h5>
  {#if agents.length > 1}
    <div class="who">
      {#each agents as x (x.id)}
        <button class="chip" class:on={x.id === agentID} onclick={() => { picked = x.id; open = ''; hits = null; query = ''; }}>{x.emoji || '🙂'} {x.name}</button>
      {/each}
    </div>
  {/if}
  {#if err}
    <p class="p-note">{err}</p>
  {:else if view}
    <div class="p-grid">
      <div class="p-stat"><b>{view.total - view.invalid}</b><span>条记忆</span></div>
      <div class="p-stat"><b>{view.people}</b><span>个人</span></div>
      <div class="p-stat"><b>{ag?.memory.dreaming ? '🌙 整理中' : ag?.memory.last_dream ? new Date(ag.memory.last_dream).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '还没有'}</b><span>上次整理</span></div>
    </div>
    {#if !view.on}
      <p class="p-note">{ag?.name ?? '它'}关掉了长期记忆（在它的 agent 节点里「长期记忆」能打开）。下面是以前记下的。</p>
    {/if}
    <div class="p-actions tight">
      <div class="p-seg">
        <button class:active={tab === 'people'} onclick={() => setTab('people')}>人物</button>
        <button class:active={tab === 'items'} onclick={() => setTab('items')}>记忆</button>
        <button class:active={tab === 'self'} onclick={() => setTab('self')}>自我认知</button>
        <button class:active={tab === 'dreams'} onclick={() => setTab('dreams')}>梦境日志</button>
      </div>
      <button class="btn sm" disabled={busy || ag?.memory.dreaming || !view.on} onclick={dreamNow} title="现在读最近的聊天，把值得记住的整理进来"><Icon name="layers" size={13} />现在整理</button>
    </div>

    {#if tab === 'people' || tab === 'items'}
      <div class="p-actions tight">
        <input class="p-input grow" placeholder="搜记忆：名字、关键词…" bind:value={query} oninput={onSearch} />
        <button class="btn sm" onclick={startNew}><Icon name="plus" size={13} />{tab === 'people' ? '加一个人' : '记一条'}</button>
      </div>
      {#if open === '+new'}
        <div class="p-card">
          <textarea class="p-input raw" rows="12" bind:value={raw}></textarea>
          <div class="p-actions tight">
            <button class="btn sm primary" disabled={busy} onclick={save}>保存</button>
            <button class="btn sm" onclick={() => (open = '')}>取消</button>
          </div>
          <p class="p-note p-small">开头是属性：title 标题、type 类型（人物 事实 偏好 事件 经验 流程 知识 看法）、summary 一句话、visible 谁在的时候能用（all / admin / 某个身份）。</p>
        </div>
      {/if}
      {#if !listed.length}
        <p class="p-note">{hits ? '没搜到。' : tab === 'people' ? '还不认识谁：聊过天、整理过记忆以后，这里会有人物画像。' : '还没有记忆：它会在聊天里记下要紧的事，睡着或空闲时把最近的聊天整理进来。'}</p>
      {/if}
      <div class="items">
        {#each listed as it (it.path)}
          <div class="item" class:gone={!!it.invalid}>
            <button class="head" onclick={() => toggle(it)}>
              <span class="tag">{it.type}</span>
              <b>{it.title}</b>
              {#if it.shared}<span class="tag">共用</span>{/if}
              <span class="line muted">{it.invalid ? '已过时：' + it.invalid : (it.snippet || it.line)}</span>
            </button>
            {#if open === it.path}
              <div class="body">
                <textarea class="p-input raw" rows="12" bind:value={raw}></textarea>
                <div class="p-note p-small">{visLabel(it.visible)}{#if it.source} · 来自 {it.source}{/if}{#if it.updated} · {it.updated} 更新{/if} · {it.path}</div>
                <div class="p-actions tight">
                  <button class="btn sm primary" disabled={busy} onclick={save}>保存</button>
                  {#if it.invalid}
                    <button class="btn sm" onclick={() => invalidate(it, true)}>恢复</button>
                  {:else}
                    <button class="btn sm" onclick={() => invalidate(it, false)} title="不删，标成过时：以后不会再想起，需要时还能找回">标成过时</button>
                  {/if}
                  <button class="btn sm danger" onclick={() => remove(it)}><Icon name="trash" size={12} />删除</button>
                </div>
              </div>
            {/if}
          </div>
        {/each}
      </div>
    {:else if tab === 'self'}
      <div class="p-card">
        <p class="p-note p-small">{ag?.name ?? '它'}对自己的认识：是谁、兴趣、最近在想什么、对身边人的感觉。{ag?.style === 'life' ? '拟人型睡着整理记忆时会自己更新。' : '工具型不会自己写，你可以写点它该知道的。'}每次聊天都会带上。</p>
        <textarea class="p-input raw" rows="12" bind:value={selfText} placeholder="还没有写"></textarea>
        <div class="p-actions tight"><button class="btn sm primary" disabled={busy} onclick={saveSelf}>保存</button></div>
      </div>
    {:else}
      {#if days.length}
        <div class="who">
          {#each days.slice(0, 14) as d (d)}
            <button class="chip" class:on={d === day} onclick={() => loadDreams(d)}>{d.slice(5)}</button>
          {/each}
        </div>
        <div class="p-card md">{@html markdown(dreamText)}</div>
      {:else}
        <p class="p-note">还没有梦境日志：每次整理记忆完，它会写几句今晚整理了什么。</p>
      {/if}
    {/if}
  {/if}

  <h5 class="p-h">上下文整理 <span class="muted p-small">只动这段对话的上下文，不动长期记忆</span></h5>
  <div class="p-grid">
    <div class="p-stat"><b>{pctText('context.compact_at', 0.75)}</b><span>到这里自动整理</span></div>
    <div class="p-stat"><b>{mi?.summarized ?? 0}</b><span>整理过的会话</span></div>
    <div class="p-stat"><b>{mi?.archives ?? 0}</b><span>归档</span></div>
  </div>
  <div class="p-card">
    <p class="p-note">每个会话的上下文到模型窗口的 {pctText('context.compact_at', 0.75)} 时自动整理，整理到 {pctText('context.compact_to', 0.4)}：先把旧的长工具输出换成一行占位，还不够再把更早的对话合成一段摘要，最近几轮原样保留。{idle}。原始聊天记录不删，整理记忆时还会读。聊天里发 <code>/context</code> 看用量，<code>/compact</code> 马上整理，<code>/reset</code> 把旧对话归档后清空；<code>/memory</code> 看记忆，<code>/sleep</code> 马上整理记忆。</p>
  </div>
  <FieldGroup group="memory" focus={store.focus} />
</div>

<style>
  .who {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .chip {
    padding: 3px 10px;
    border-radius: 8px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--text-2);
    font: inherit;
    font-size: 12.5px;
    cursor: pointer;
  }
  .chip.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 12%, transparent);
    color: var(--text);
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .items {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .item {
    border: 1px solid var(--line);
    border-radius: 10px;
    background: var(--bg-1);
  }
  .item.gone {
    opacity: 0.55;
  }
  .item .head {
    display: flex;
    align-items: baseline;
    gap: 8px;
    width: 100%;
    padding: 8px 10px;
    border: none;
    background: none;
    color: var(--text);
    font: inherit;
    font-size: 13px;
    text-align: left;
    cursor: pointer;
    min-width: 0;
  }
  .item .head b {
    white-space: nowrap;
  }
  .item .line {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 12px;
  }
  .item .body {
    padding: 0 10px 10px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .raw {
    width: 100%;
    height: auto;
    min-height: 220px;
    padding: 8px 10px;
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 12px;
    line-height: 1.55;
    resize: vertical;
  }
  .md {
    font-size: 13px;
    line-height: 1.65;
  }
</style>
