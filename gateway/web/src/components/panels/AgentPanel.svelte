<script lang="ts">
  // 一个 agent 的节点：状态 → 操作 → 设置 → 危险操作。
  // 设置分两层：这里能改的是它自己覆盖的项，没覆盖的跟随「Agent 默认值」。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { modeMeta } from '../../lib/format';
  import { botKindLabel, optionLabel, toolLabel } from '../../lib/labels';
  import type { AgentSetting } from '../../lib/types';
  import Icon from '../Icon.svelte';

  let { id }: { id: string } = $props();

  const a = $derived(store.agent(id));
  const ov = $derived(store.ov!);
  const models = $derived(ov.models?.models ?? []);
  const unbound = $derived(store.bots.filter((b) => b.agent !== id));
  let busy = $state(false);
  let pick = $state('');

  async function patch(body: any) {
    busy = true;
    try {
      await api.patch('/api/agents/' + encodeURIComponent(id), body);
      await store.refresh();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  async function saveSetting(k: string, v: any) {
    await patch({ settings: { [k]: v } });
    store.toast({ kind: 'ok', text: v === null ? '已恢复跟随全局' : '已保存' }, 1800);
  }

  function shown(s: AgentSetting): string {
    const v = s.value ?? s.effective;
    if (v === null || v === undefined) return '';
    return s.type === 'enum' ? optionLabel(s.key, String(v)) : String(v);
  }
  // 下拉框用的是原始值，显示的是中文
  function settingValue(s: AgentSetting): string {
    const v = s.value ?? s.effective;
    return v === null || v === undefined ? '' : String(v);
  }

  async function toggleTool(name: string) {
    if (!a) return;
    const cur = new Set(a.tools);
    if (cur.has(name)) cur.delete(name);
    else cur.add(name);
    await patch({ tools: [...cur] });
  }

  async function bind(botID: string) {
    if (!botID) return;
    try {
      await api.patch('/api/bots/' + encodeURIComponent(botID), { agent: id });
      pick = '';
      await store.refresh();
      store.toast({ kind: 'ok', text: '已经绑上这个机器人' }, 2200);
    } catch (err) {
      store.fail(err);
    }
  }

  async function unbind(botID: string) {
    if (!confirm('把这个机器人交给默认 agent？（机器人不能没有 agent，想彻底不用可以停用它）')) return;
    try {
      await api.patch('/api/bots/' + encodeURIComponent(botID), { agent: 'default' });
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }

  async function remove() {
    const botHint = !a || !a.bots.length ? '' : '\n绑着它的 ' + a.bots.length + ' 个机器人会交给默认 agent（' + a.bots.map((b) => b.name).join('、') + '）。';
    if (!a || !confirm('删除 agent「' + a.name + '」？' + botHint + '会话记录还在，但它的人格文件不再被使用。')) return;
    try {
      await api.del('/api/agents/' + encodeURIComponent(id));
      store.inspector = null;
      await store.refresh();
      store.toast({ kind: 'ok', text: '已删除' }, 2200);
    } catch (err) {
      store.fail(err);
    }
  }
</script>

{#if a}
  <div class="pnl">
    <div class="p-grid">
      <div class="p-stat"><b>{a.sessions}</b><span>会话</span></div>
      <div class="p-stat"><b class:acc={a.bots.length > 0}>{a.bots.length}</b><span>绑的机器人</span></div>
      <div class="p-stat"><b>{a.running ? '在跑' : '空闲'}</b><span>现在</span></div>
    </div>

    <div class="p-card">
      <label class="p-label">名字{#if a.default}<span class="tag accent">默认</span>{/if}
        <input class="p-input" value={a.name} onchange={(e) => patch({ name: (e.target as HTMLInputElement).value })} />
      </label>
      <label class="p-label">头像（一个 emoji）
        <input class="p-input" value={a.emoji} placeholder="🦦" onchange={(e) => patch({ emoji: (e.target as HTMLInputElement).value })} />
      </label>
      <div class="p-row"><span class="p-k">模型</span><span class="p-grow">{a.model || a.model_label}</span></div>
      <div class="p-row"><span class="p-k">人格</span><span class="p-grow">{a.persona_chars ? a.persona_chars + ' 字' : '还没有写'}</span></div>
      <div class="p-row"><span class="p-k">会话 ID</span><code class="p-grow">{a.default ? 'main' : '@' + a.id + '/main'}</code></div>
      <p class="p-note p-small">改名、换头像不影响会话 ID，历史和绑定的机器人都还在。</p>
    </div>

    <h5 class="p-h">人格 <span class="muted p-small">agents/{a.id}/persona.md，保存后下一条消息生效</span></h5>
    <div class="p-card">
      <div class="p-actions tight">
        <button class="btn sm primary" onclick={() => (store.persona = { name: 'persona.md', isNew: false, agent: id })}><Icon name="pen" size={13} />编辑人格</button>
        <button class="btn sm" onclick={() => store.open({ type: 'session', sid: a!.default ? 'main' : '@' + a!.id + '/main' })}><Icon name="chat" size={13} />打开会话</button>
      </div>
      <p class="p-note p-small">没写就是没有人格。升级时会把 v0.4 的 personas/default.md 复制成这个文件（原文件留着，方便退回旧版本）。想要另一个人格就再加一个 agent（每个人格一个 agent）；只想让某个会话换人格，用那个会话里的「人格」新建。</p>
    </div>

    <h5 class="p-h">机器人 <span class="muted p-small">一个机器人只绑一个 agent</span></h5>
    <div class="p-card">
      {#if a.bots.length}
        {#each a.bots as b (b.id)}
          <button class="p-srow" onclick={() => store.open({ type: 'bot', id: b.id })} title={'渠道 ID：' + b.channel}>
            <Icon name={b.kind === 'feishu' ? 'feishu' : 'send'} size={14} />
            <span class="p-grow">{b.name}{#if b.name !== botKindLabel(b.kind)} <span class="muted p-small">{botKindLabel(b.kind)}</span>{/if}</span>
            <span class="muted p-small">{b.enabled ? '开着' : '停用'}</span>
            <Icon name="right" size={13} />
          </button>
        {/each}
      {:else}
        <p class="p-note">还没绑机器人：只能在这里的网页里说话。</p>
      {/if}
      {#if unbound.length}
        <select class="p-select" bind:value={pick} onchange={() => bind(pick)}>
          <option value="">把一个机器人绑到它…</option>
          {#each unbound as b (b.id)}<option value={b.id}>{b.name}{#if b.name !== botKindLabel(b.kind)}（{botKindLabel(b.kind)}）{/if}{b.enabled ? '' : ' · 停用'}</option>{/each}
        </select>
      {/if}
      <div class="p-actions tight">
        <button class="btn sm" onclick={() => store.open({ type: 'bots' })}><Icon name="plus" size={13} />加一个机器人</button>
      </div>
    </div>

    <h5 class="p-h">模型和思考强度 <span class="muted p-small">空表示跟随全局</span></h5>
    <div class="p-card">
      <label class="p-label">模型
        <select class="p-select" value={a.model} onchange={(e) => patch({ model: (e.target as HTMLSelectElement).value })}>
          <option value="">默认模型（{models.find((m) => m.default)?.label ?? '没配'}）</option>
          {#each models as m (m.id)}<option value={m.id}>{m.label}</option>{/each}
        </select>
      </label>
      <label class="p-label">思考强度
        <select class="p-select" value={a.thinking} onchange={(e) => patch({ thinking: (e.target as HTMLSelectElement).value })}>
          <option value="">模型默认</option>
          {#each a.thinking_levels ?? [] as l (l)}<option value={l}>{optionLabel('thinking', l)}</option>{/each}
        </select>
      </label>
    </div>

    <h5 class="p-h">覆盖的设置 <span class="muted p-small">没改的跟随 <button class="p-lnk" onclick={() => store.open({ type: 'agentDefaults' })}>Agent 默认值</button></span></h5>
    <div class="p-card">
      {#each a.settings as s (s.key)}
        <div class="setting">
          <div class="p-row">
            <span class="p-grow">{s.label}{#if s.value !== null}<span class="tag">自己设的</span>{/if}</span>
            {#if s.value !== null}
              <button class="p-lnk p-small" onclick={() => saveSetting(s.key, null)}>跟随全局</button>
            {/if}
          </div>
          {#if s.type === 'enum'}
            <select class="p-select" value={settingValue(s)} onchange={(e) => saveSetting(s.key, (e.target as HTMLSelectElement).value)}>
              {#each s.options ?? [] as o (o)}<option value={o}>{optionLabel(s.key, o)}</option>{/each}
            </select>
          {:else}
            <input class="p-input" value={shown(s)} inputmode={s.type === 'string' ? 'text' : 'decimal'}
              onchange={(e) => {
                const raw = (e.target as HTMLInputElement).value.trim();
                if (raw === '') return saveSetting(s.key, null);
                saveSetting(s.key, s.type === 'string' ? raw : Number(raw));
              }} />
          {/if}
          {#if s.value === null}<p class="p-note p-small">现在用全局的：{s.type === 'enum' ? optionLabel(s.key, String(s.effective)) : s.effective}{#if s.key === 'mode'}{' · ' + (modeMeta[String(s.effective)]?.label ?? '')}{/if}</p>{/if}
          {#if s.help}<p class="p-note p-small">{s.help}</p>{/if}
        </div>
      {/each}
    </div>

    <h5 class="p-h">工具白名单 <span class="muted p-small">点一下加减；一个都不选表示全部能用</span></h5>
    <div class="p-card">
      <div class="tools">
        {#each a.tools_available ?? [] as t (t)}
          <button class="chip" class:on={a.tools.includes(t)} onclick={() => toggleTool(t)} disabled={busy} title={'模型看到的工具名：' + t}>
            {toolLabel(t)}{#if toolLabel(t) !== t}<span class="tiny">{t}</span>{/if}
          </button>
        {/each}
      </div>
      {#if !(a.tools_available ?? []).length}<p class="p-note">还没有内核在线，读不到工具列表；先装上内核再来选。</p>{/if}
    </div>

    {#if !a.default}
      <div class="p-danger-zone">
        <button class="btn sm danger" onclick={remove}><Icon name="trash" size={13} />删除这个 agent</button>
        <p class="p-note p-small">人格文件（agents/{a.id}/persona.md）留在磁盘上；绑着它的机器人会交给默认 agent。</p>
      </div>
    {/if}
  </div>
{:else}
  <p class="p-note">没有这个 agent，可能刚被删掉。</p>
{/if}

<style>
  .setting {
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .setting + .setting {
    margin-top: 6px;
    padding-top: 10px;
    border-top: 1px dashed var(--line);
  }
  .tools {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .chip {
    padding: 3px 9px;
    border-radius: 8px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--text-2);
    font: inherit;
    font-size: 12px;
    cursor: pointer;
  }
  .chip .tiny {
    margin-left: 4px;
    opacity: 0.55;
    font-size: 10.5px;
  }
  .chip.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 12%, transparent);
    color: var(--text);
  }
</style>
