<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { ago, modeMeta, truncate } from '../../lib/format';
  import type { Persona } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import Icon from '../Icon.svelte';

  let items = $state<Persona[]>([]);
  let preview = $state('');

  async function load() {
    try {
      items = (await api.get<{ items: Persona[] }>('/api/personas')).items ?? [];
    } catch (err) {
      store.fail(err);
    }
    // 还没写过默认人格（文件不存在）不算错，也不去读
    preview = items.some((p) => p.name === 'default.md')
      ? await api.get<{ text: string }>('/api/personas/default.md').then((r) => r.text ?? '', () => '')
      : '';
  }
  $effect(() => {
    store.personaTick;
    load();
  });

  const own = $derived(items.filter((p) => p.name !== 'default.md'));
  const def = $derived(items.find((p) => p.name === 'default.md'));
  const field = (k: string) => store.field(k)?.value;
  const steps = $derived(Number(field('agent.max_steps') ?? 200));
  const limit = (v: unknown, none: string) => (!v || v === '0' || v === '0s' ? none : String(v));
  const mode = $derived(String(field('workspace.mode') ?? 'ask'));
  const sidOf = (n: string) => decodeURIComponent(n.replace(/\.md$/, ''));
  const titleOf = (n: string) => store.session(sidOf(n))?.title ?? sidOf(n);
  // 人格的头几行，去掉 Markdown 标记
  const lines = $derived(
    preview
      .split('\n')
      .map((l) => l.replace(/^#+\s*|^[-*]\s+|\*\*/g, '').trim())
      .filter(Boolean)
      .slice(0, 4),
  );
</script>

<div class="pnl">
  <p class="p-note lead">Agent 就是回复你的那个“它”：人格决定它是谁、怎么说话，下面的设置决定它怎么干活、谁能用它。以后可以有好几个 agent，各自绑自己的机器人，每个都是图上的一个节点。</p>

  <div class="p-grid">
    <div class="p-stat"><b>{steps > 0 ? steps : '不限'}</b><span>每条消息最多请求模型</span></div>
    <div class="p-stat"><b>{limit(field('agent.run_timeout'), '不限')}</b><span>每条消息最长</span></div>
    <div class="p-stat"><b>{modeMeta[mode]?.label ?? mode}</b><span>新会话的权限</span></div>
  </div>

  <h5 class="p-h">人格 <span class="muted p-small">系统提示词，保存后下一条消息生效</span></h5>
  <div class="p-card">
    <div class="p-row">
      <Icon name="persona" size={15} />
      <strong class="p-grow">默认人格</strong>
      {#if def}<span class="muted p-small p-nowrap">{def.sessions.length} 个会话在用 · {ago(def.mod)}修改</span>{/if}
    </div>
    {#if lines.length}
      <div class="persona-preview">
        {#each lines as l}<div>{truncate(l, 60)}</div>{/each}
      </div>
    {:else}
      <p class="p-note">还没有写默认人格：现在是没有人格的普通助手。点「编辑人格」写一段，或者套用自带的人格。</p>
    {/if}
    <div class="p-actions tight">
      <button class="btn sm primary" onclick={() => (store.persona = { name: 'default.md', isNew: false })}><Icon name="pen" size={13} />编辑人格</button>
    </div>
    {#if own.length}
      <div class="p-small muted">这些会话有自己的人格：</div>
      {#each own as p (p.name)}
        <button class="p-srow" onclick={() => (store.persona = { name: p.name, isNew: false })}>
          <Icon name="chat" size={14} />
          <span class="p-grow">{titleOf(p.name)}</span>
          <span class="muted p-small p-nowrap">{ago(p.mod)}修改</span>
          <Icon name="right" size={13} />
        </button>
      {/each}
    {/if}
    <p class="p-note p-small">要给某个会话单独设定人格：点那个会话的节点，在“人格”里新建。聊天里管理员发 <code>/persona</code> 可以换上自带人格。</p>
  </div>

  <FieldGroup group="agent" focus={store.focus} />
</div>

<style>
  .persona-preview {
    padding: 9px 11px;
    border-radius: 10px;
    background: var(--bg-2);
    font-size: 12.5px;
    color: var(--text-2);
    line-height: 1.6;
  }
</style>
