<script lang="ts">
  // 一个 agent 的节点：状态 → 操作 → 设置 → 危险操作。
  // 设置分两层：这里能改的是它自己覆盖的项，没覆盖的跟随「Agent 默认值」。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { botKindMeta, modeMeta } from '../../lib/format';
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
    const s = a?.settings.find((x) => x.key === k);
    await patch({ settings: { [k]: v } });
    store.toast({ kind: 'ok', text: v === null ? (s && ownDefault(s) ? '已恢复默认' : '已恢复跟随全局') : '已保存' }, 1800);
  }

  // 设置分组：干活跟随全局；群聊、拟人、记忆这几组没有全局设置，用的是默认值
  const groups: { key: AgentSetting['group']; title: string; note: string }[] = [
    { key: 'work', title: '干活', note: '没改的跟随' },
    { key: 'group', title: '群聊', note: '在群里怎么说话' },
    { key: 'life', title: '拟人', note: '作息、多久醒一次、主动找谁' },
    { key: 'memory', title: '长期记忆', note: '记什么、什么时候整理' },
  ];
  // 拟人型的权限模式默认放行，不跟随全局
  const ownDefault = (s: AgentSetting) => s.group !== 'work' || (s.key === 'mode' && a?.style === 'life');
  const life = $derived(a?.style === 'life');

  async function setStyle(style: 'tool' | 'life') {
    if (!a || a.style === style) return;
    await patch({ style });
    store.toast({ kind: 'ok', text: style === 'life' ? '改成拟人型了：它会有自己的作息，醒着时偶尔自己活动' : '改成工具型了：只在有人找它时干活' }, 2600);
  }

  // ---- 自主活动（v0.7.3）：日程、目标、想法 ----
  const auto = $derived(a?.autonomy);
  // 今天的活动账本：探索占每日次数，约定/Hook/手动各记各的
  const ledger = $derived.by(() => {
    const out: Record<string, { starts: number; tokens: number; reserved?: number }> = {};
    for (const row of auto?.ledger ?? []) out[row.source] = row;
    return out;
  });
  const ledgerText = $derived.by(() => {
    if (!ledger.exploration) return '';
    const limit = a?.life?.max_wakes ?? 0;
    const unlimited = a?.life?.wakes_unlimited ?? limit === 0;
    const exploration = `探索 ${ledger.exploration.starts}${unlimited ? '（不限）' : ' / ' + limit}`;
    const parts = [exploration];
    if (ledger.commitment?.starts) parts.push(`约定 ${ledger.commitment.starts}`);
    if (ledger.hook?.starts) parts.push(`Hook ${ledger.hook.starts}`);
    if (ledger.manual?.starts) parts.push(`手动 ${ledger.manual.starts}`);
    return '今天：' + parts.join(' · ');
  });
  const kindLabel: Record<string, string> = { commitment: '约定', checkup: '复查', plan: '计划', exploration: '探索' };
  const mindState: Record<string, string> = {
    accepted: '推进中', running: '正在做', waiting: '等下一步', paused: '暂停', done: '完成', failed: '失败', cancelled: '取消',
  };
  async function autonomy(action: string, body: Record<string, unknown> = {}) {
    busy = true;
    try {
      await api.post('/api/agents/' + encodeURIComponent(id) + '/autonomy', { action, ...body });
      await store.refresh();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
  async function cancelAgenda(item: string, title: string) {
    if (!confirm('取消这件事？\n' + title)) return;
    await autonomy('cancel_agenda', { id: item });
    store.toast({ kind: 'ok', text: '已取消' }, 1800);
  }
  async function dropIdea(item: string, text: string) {
    if (!confirm('放弃这个想法？\n' + text)) return;
    await autonomy('drop_idea', { id: item });
    store.toast({ kind: 'ok', text: '已放弃' }, 1800);
  }
  // 心智数值（设计 §7.2）：兴趣/新颖/优先级/进度/置信度都能在面板里改，改完立刻参与挑选
  async function setNumber(id: string, field: string, value: number) {
    if (!Number.isFinite(value)) return;
    try {
      await api.post('/api/agents/' + encodeURIComponent(id) + '/autonomy', { action: 'set_number', id, field, value });
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
  // 心智档案：预设、候选权重和可选维度都走同一个 autonomy 接口
  async function profileAction(action: string, body: Record<string, unknown>) {
    await autonomy(action, body);
    store.toast({ kind: 'ok', text: '心智档案已更新，下一次挑选就用新值' }, 2200);
  }
  async function deferIdea(item: string) {
    await autonomy('defer_idea', { id: item, when: '明天 09:00' });
    store.toast({ kind: 'ok', text: '推迟到明天上午再看' }, 1800);
  }

  async function post(path: string, ok: string) {
    busy = true;
    try {
      await api.post('/api/agents/' + encodeURIComponent(id) + path, {});
      store.toast({ kind: 'ok', text: ok }, 2200);
      await store.refresh();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  function when(ms?: number): string {
    if (!ms) return '';
    const d = new Date(ms);
    const now = new Date();
    const hm = d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    if (d.toDateString() === now.toDateString()) return hm;
    const tomorrow = new Date(now.getTime() + 86400000);
    if (d.toDateString() === tomorrow.toDateString()) return '明天 ' + hm;
    return `${d.getMonth() + 1}-${d.getDate()} ${hm}`;
  }
  const lifeState: Record<string, { icon: string; text: string }> = {
    sleeping: { icon: '💤', text: '睡着' },
    awake: { icon: '🌱', text: '醒着' },
    thinking: { icon: '💭', text: '在想事情' },
  };
  const sessionOf = (kind: string) => '@' + id + '/' + kind;
  function modelLabelOf(v: string) {
    if (!v) return '跟这个 agent 一样';
    return models.find((m) => m.id === v)?.label ?? v;
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
      {#if a.doing}
        <div class="p-stat"><b>{a.doing.icon} {a.doing.brief}</b><span>现在</span></div>
      {:else if a.life}
        <div class="p-stat"><b>{lifeState[a.life.state]?.icon} {lifeState[a.life.state]?.text}</b><span>现在</span></div>
      {:else}
        <div class="p-stat"><b>{a.running ? '在跑' : '空闲'}</b><span>现在</span></div>
      {/if}
    </div>

    <div class="styles" role="radiogroup" aria-label="类型">
      <button class="style" class:on={!life} role="radio" aria-checked={!life} disabled={busy} onclick={() => setStyle('tool')}>
        <span class="si">🛠</span><b>工具型</b><span class="muted p-small">有人找它才干活；群里只回被叫到的</span>
      </button>
      <button class="style" class:on={life} role="radio" aria-checked={life} disabled={busy} onclick={() => setStyle('life')}>
        <span class="si">🌱</span><b>拟人型</b><span class="muted p-small">像人一样：有作息，没事时自己上网、想事情、找人聊天</span>
      </button>
    </div>

    {#if a.life}
      {@const L = a.life}
      <div class="p-card lifecard">
		{#if L.error}<p class="p-note warn">{L.error}</p>{/if}
        <div class="p-row">
          <span class="p-grow">
            {#if a.doing}{a.doing.icon} {a.doing.text}{:else if L.state === 'sleeping'}💤 睡着了，{when(L.wake_at)} 醒{:else}🌱 醒着{/if}
            {#if auto}
              <span class="tag" class:accent={!auto.paused} class:warn={auto.paused}>{auto.paused ? '自主活动暂停' : '自主活动开着'}</span>
            {/if}
          </span>
        </div>
        <div class="p-note p-small">
          今天探索 {L.wakes} 次{L.wakes_unlimited ? ' / 不限' : ` / ${L.max_wakes} 次`} · 主动联系 {L.reaches} 次{L.reaches_unlimited ? ' / 不限' : ` / ${L.max_reaches} 次`} · 用了 {L.tokens.toLocaleString()} token{#if auto?.budget?.limit}（预算上限 {auto.budget.limit.toLocaleString()}）{/if}{#if auto?.next_check} · 下次自己看 {when(auto.next_check)}{/if}{#if auto} · 今天日程 {auto.today_done}/{auto.today_planned} 件{/if}{#if L.waiting?.length} · 在等 {L.waiting.join('、')} 回话{/if}
        </div>
        <details class="experience" open>
          <summary>最近的经历（带来源和结局）</summary>
          {#if L.experience?.length}
            <ul class="recent">
              {#each L.experience as n (n.id)}
                <li><span class="muted">{when(n.at * 1000)} · {n.where ?? n.kind} · {n.visible}</span>
                  <details><summary>{n.said || n.reason || n.kind} · {n.status === 'ok' ? '已结束' : n.status}</summary>
                    {#if n.asked}<p>当时的请求：{n.asked}</p>{/if}
                    {#if n.did?.length}<p>实际操作：{n.did.join('；')}</p>{/if}
                    {#if n.episode}<p>片段：{n.episode}</p>{/if}
                    {#if n.agenda_id}<p>日程：{n.agenda_id}</p>{/if}
                  </details>
                </li>
              {/each}
            </ul>
          {:else}<p class="p-note p-small">还没有经历；聊天和活动结束后会在这里记录实际结果。</p>{/if}
        </details>
        <div class="p-actions tight">
          <button class="btn sm primary" disabled={busy || L.state === 'thinking'} onclick={() => post('/wake', '叫醒了，它在想事情')}><Icon name="sparkles" size={13} />现在叫醒它</button>
          {#if auto?.paused}
            <button class="btn sm primary" disabled={busy} onclick={() => autonomy('resume')}>恢复自主活动</button>
          {:else if auto}
            <button class="btn sm" disabled={busy} onclick={() => autonomy('pause')}>暂停自主活动</button>
          {/if}
          {#if L.recent.length || L.wakes}
            <a class="btn sm" href="#/sessions/{encodeURIComponent(sessionOf('life'))}" onclick={() => (store.inspector = null)}><Icon name="chat" size={13} />看它的内心</a>
          {/if}
        </div>

        {#if auto}
          {#if auto.paused}
            <p class="p-note p-small">已暂停：{auto.pause_reason || '不会再自己安排或做事'}。恢复后接着做之前的事。</p>
          {/if}
          {#if auto.agenda.length}
            <div class="p-sub">日程</div>
            {#each auto.agenda as it (it.id)}
              <div class="p-row mind-item">
                <span class="p-grow"><b>{when(it.at * 1000)}</b> {it.title} <span class="tag">{kindLabel[it.kind] ?? it.kind}</span>
                  {#if it.criteria}<span class="p-note p-small">完成条件：{it.criteria}</span>{/if}
                  {#if it.checkpoint}<span class="p-note p-small">接着做：{it.checkpoint}</span>{/if}
                </span>
                <button class="btn sm ghost" title="取消这件事" disabled={busy} onclick={() => cancelAgenda(it.id, it.title)}><Icon name="x" size={12} /></button>
              </div>
            {/each}
          {/if}
          {#if ledgerText}<p class="p-note p-small">{ledgerText}</p>{/if}
          {#if auto.intentions.length}
            <div class="p-sub">在推进的目标</div>
            {#each auto.intentions as it (it.id)}
              <div class="mind-line">
                {it.title} <span class="muted p-small">{mindState[it.state] ?? it.state}{#if it.blocked} · {it.blocked}{/if}</span>
                <label class="num" title="优先级（1–5）">优先<input type="number" min="1" max="5" value={it.priority ?? 1} disabled={busy} onchange={(e)=>void setNumber(it.id, 'priority', +e.currentTarget.value)}/></label>
                <label class="num" title="进度（0–100）">进度<input type="number" min="0" max="100" value={it.progress} disabled={busy} onchange={(e)=>void setNumber(it.id, 'progress', +e.currentTarget.value)}/></label>
              </div>
            {/each}
          {/if}
          {#if auto.ideas.length}
            <div class="p-sub">想到的事</div>
            {#each auto.ideas as x (x.id)}
              <div class="p-row mind-item">
                <span class="p-grow">{x.text} <span class="muted p-small">来源：{x.source}</span></span>
                <label class="num" title="兴趣（1–5）">趣<input type="number" min="1" max="5" value={x.interest} disabled={busy} onchange={(e)=>void setNumber(x.id, 'interest', +e.currentTarget.value)}/></label>
                <label class="num" title="新颖（1–5）">新<input type="number" min="1" max="5" value={x.novelty} disabled={busy} onchange={(e)=>void setNumber(x.id, 'novelty', +e.currentTarget.value)}/></label>
                <button class="btn sm ghost" title="推迟到明天再想" disabled={busy} onclick={() => deferIdea(x.id)}>推迟</button>
                <button class="btn sm ghost" title="放弃这个想法" disabled={busy} onclick={() => dropIdea(x.id, x.text)}><Icon name="x" size={12} /></button>
              </div>
            {/each}
          {/if}
          {#if !auto.agenda.length && !auto.intentions.length && !auto.ideas.length}
            <p class="p-note p-small">没有排着的事，也没有候选的想法。约定到点、或它自己觉得该看看时再来处理。</p>
          {/if}
          {#if auto.executions.length}
            <details class="mind-done">
              <summary class="muted p-small">最近的执行记录（{auto.executions.length}）</summary>
              <ul class="recent">
                {#each auto.executions as e (e.id)}
                  <li><span class="muted">{when(e.ended * 1000)}</span> {e.ok && e.verified ? '✅ 已验收' : e.ok ? '待验收' : '未完成'} {e.summary}
                    {#if e.evidence?.length}
                      <details><summary>查看执行依据</summary>
                        {#each e.evidence as proof (proof.id)}<p class="p-note p-small"><b>{proof.name}</b>：{proof.result}</p>{/each}
                      </details>
                    {/if}
                  </li>
                {/each}
              </ul>
            </details>
          {/if}
          {#if auto.profile}
            <div class="p-sub">心智档案</div>
            <div class="p-row">
              <span class="p-grow muted p-small">预设（展开成具体数值，改过就是 custom）</span>
              <select disabled={busy} value={auto.profile.preset ?? 'balanced'} onchange={(e) => void profileAction('set_preset', { id: e.currentTarget.value })}>
                <option value="balanced">balanced 均衡</option>
                <option value="curious">curious 好奇</option>
                <option value="focused">focused 专注</option>
                <option value="custom">custom 自定义</option>
              </select>
            </div>
            <div class="p-row">
              <label class="num" title="兴趣权重（0–5）">兴趣<input type="number" min="0" max="5" step="0.5" value={auto.profile.weights.interest} disabled={busy} onchange={(e) => void profileAction('set_weight', { field: 'interest', value: +e.currentTarget.value })}/></label>
              <label class="num" title="新颖权重（0–5）">新颖<input type="number" min="0" max="5" step="0.5" value={auto.profile.weights.novelty} disabled={busy} onchange={(e) => void profileAction('set_weight', { field: 'novelty', value: +e.currentTarget.value })}/></label>
              <label class="num" title="成本权重（0–5）">成本<input type="number" min="0" max="5" step="0.5" value={auto.profile.weights.cost} disabled={busy} onchange={(e) => void profileAction('set_weight', { field: 'cost', value: +e.currentTarget.value })}/></label>
              <label class="num" title="成本参照分钟数（1–480）">参照<input type="number" min="1" max="480" value={auto.profile.weights.cost_scale} disabled={busy} onchange={(e) => void profileAction('set_weight', { field: 'cost_scale', value: +e.currentTarget.value })}/></label>
            </div>
            {#if auto.profile.pinned && Object.keys(auto.profile.pinned).length}
              <p class="p-note p-small">人工锁定：{Object.entries(auto.profile.pinned).map(([k, v]) => k + '=' + v).join(' · ')}（模型自己提的值会被替换成它）</p>
            {/if}
            {#each ['curiosity', 'social_drive', 'focus', 'energy'] as dim (dim)}
              {@const d = auto.profile.dimensions?.[dim]}
              <div class="p-row">
                <label class="p-grow p-small"><input type="checkbox" checked={d?.enabled ?? false} disabled={busy} onchange={(e) => void profileAction('set_dimension', { id: dim, field: 'enabled', value: e.currentTarget.checked ? 1 : 0 })}/> {dim}</label>
                {#if d}
                  <label class="num" title="当前值（0–100）">值<input type="number" min="0" max="100" value={d.value} disabled={busy} onchange={(e) => void profileAction('set_dimension', { id: dim, field: 'value', value: +e.currentTarget.value })}/></label>
                  <label class="num" title="基线（0–100）">基线<input type="number" min="0" max="100" value={d.baseline} disabled={busy} onchange={(e) => void profileAction('set_dimension', { id: dim, field: 'baseline', value: +e.currentTarget.value })}/></label>
                  <label class="num" title="每次结算向基线靠多少（0–10）">漂移<input type="number" min="0" max="10" value={d.drift ?? 0} disabled={busy} onchange={(e) => void profileAction('set_dimension', { id: dim, field: 'drift', value: +e.currentTarget.value })}/></label>
                  <label class="num" title="越过它发一条 mind.threshold_crossed（0 表示不设）">阈值<input type="number" min="0" max="100" value={d.threshold ?? 0} disabled={busy} onchange={(e) => void profileAction('set_dimension', { id: dim, field: 'threshold', value: +e.currentTarget.value })}/></label>
                {/if}
              </div>
            {/each}
          {/if}
          <details class="mind-state">
            <summary>心智状态（长期兴趣、认知、反思）</summary>
            <div class="p-sub">长期兴趣</div>
            {#if auto.self?.interests?.length}
              {#each auto.self.interests as it (it.text)}
                <div class="mind-line">{it.text} <span class="muted p-small">来源：{it.source} · {when(it.at * 1000)}</span></div>
              {/each}
            {:else}
              <p class="p-note p-small">还没有明确的长期兴趣；它会在反思和探索里慢慢形成。</p>
            {/if}
            <div class="p-sub">认知</div>
            {#if auto.beliefs.length}
              {#each auto.beliefs as b (b.subject + b.at)}
                <div class="mind-line"><b>{b.subject}</b>：{b.value} <span class="muted p-small">来源：{b.source || '—'} · {when(b.at * 1000)}</span>
                  <label class="num" title="置信度（0–100%，低于 50% 会当成还没确认）">置信<input type="number" min="0" max="100" value={Math.round((b.confidence ?? 0) * 100)} disabled={busy} onchange={(e)=>void setNumber(b.subject, 'confidence', +e.currentTarget.value / 100)}/>%</label>
                </div>
              {/each}
            {:else}
              <p class="p-note p-small">还没有形成明确的认识。</p>
            {/if}
            <div class="p-sub">最近反思</div>
            {#if auto.reflections.length}
              {#each auto.reflections as r (r.at + r.text)}
                <div class="mind-line">{r.text}{#if r.change}<span class="muted p-small"> · 调整：{r.change}</span>{/if} <span class="muted p-small">· {when(r.at * 1000)}</span></div>
              {/each}
            {:else}
              <p class="p-note p-small">还没有反思记录；整理记忆或做完事情后会写在这里。</p>
            {/if}
          </details>
        {/if}
      </div>
    {/if}

    <div class="p-card">
      <div class="p-row">
        <span class="p-k">记忆</span>
        <span class="p-grow">{#if !a.memory.on}关着{:else}{a.memory.total} 条，{a.memory.people} 个人{/if}</span>
        {#if a.memory.dreaming}<span class="tag accent">🌙 在整理</span>{/if}
      </div>
      {#if !a.memory.on}
        <div class="p-actions tight">
          <button class="btn sm" disabled={busy} onclick={() => saveSetting('memory', null)}><Icon name="brain" size={13} />打开长期记忆</button>
        </div>
      {:else}
        <div class="p-note p-small">{#if a.memory.last_dream}上次整理：{when(a.memory.last_dream)}{#if a.memory.last_note}。{a.memory.last_note}{/if}{:else}还没整理过：{life ? '睡着时' : '所有聊天都安静一阵以后'}会读最近的聊天，把值得记住的人和事记下来。{/if}</div>
        <div class="p-actions tight">
          <button class="btn sm" onclick={() => store.open({ type: 'memory', agent: id })}><Icon name="brain" size={13} />看记忆</button>
          <button class="btn sm" disabled={busy || a.memory.dreaming} onclick={() => post('/dream', '去整理记忆了')}><Icon name="layers" size={13} />现在整理</button>
        </div>
      {/if}
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
            <Icon name={botKindMeta[b.kind]?.icon ?? 'send'} size={14} />
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

    {#each groups as g (g.key)}
      {#if g.key !== 'life' || life}
        <h5 class="p-h">{g.title} <span class="muted p-small">{#if g.key === 'work'}没改的跟随 <button class="p-lnk" onclick={() => store.open({ type: 'agentDefaults' })}>Agent 默认值</button>{:else}{g.note}{/if}</span></h5>
        <div class="p-card">
          {#if g.key === 'group' && !life}
            <p class="p-note p-small">工具型在群里只回被叫到的（@ 它、回复它、叫它名字），不主动插话；想让它在群里像人一样聊，改成拟人型。</p>
          {/if}
          {#each a.settings.filter((x) => (x.group ?? 'work') === g.key) as s (s.key)}
            <div class="setting">
              <div class="p-row">
                <span class="p-grow">{s.label}{#if s.value !== null}<span class="tag">自己设的</span>{/if}</span>
                {#if s.value !== null}
                  <button class="p-lnk p-small" onclick={() => saveSetting(s.key, null)}>{ownDefault(s) ? '用默认' : '跟随全局'}</button>
                {/if}
              </div>
              {#if s.type === 'enum'}
                <select class="p-select" value={settingValue(s)} disabled={s.key === 'group_eagerness' && !life} onchange={(e) => saveSetting(s.key, (e.target as HTMLSelectElement).value)}>
                  {#each s.options ?? [] as o (o)}<option value={o}>{optionLabel(s.key, o)}</option>{/each}
                </select>
              {:else if s.type === 'model'}
                <select class="p-select" value={String(s.value ?? '')} onchange={(e) => saveSetting(s.key, (e.target as HTMLSelectElement).value || null)}>
                  {#each s.options ?? [''] as o (o)}<option value={o}>{modelLabelOf(o)}</option>{/each}
                </select>
              {:else}
                <input class="p-input" value={shown(s)} inputmode={s.type === 'string' ? 'text' : 'decimal'}
                  onchange={(e) => {
                    const raw = (e.target as HTMLInputElement).value.trim();
                    if (raw === '') return saveSetting(s.key, null);
                    saveSetting(s.key, s.type === 'string' ? raw : Number(raw));
                  }} />
              {/if}
              {#if s.value === null && s.type !== 'model'}<p class="p-note p-small">现在用{ownDefault(s) ? '默认的' : '全局的'}：{s.type === 'enum' ? optionLabel(s.key, String(s.effective)) : s.effective}{#if s.key === 'mode' && modeMeta[String(s.effective)]}{'，' + modeMeta[String(s.effective)].desc}{/if}</p>{/if}
              {#if s.help}<p class="p-note p-small">{s.help}</p>{/if}
            </div>
          {/each}
        </div>
      {/if}
    {/each}

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
  .num {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    font-size: 11px;
    color: var(--text-3);
  }
  .num input {
    width: 54px;
    padding: 2px 4px;
    font-size: 11px;
    background: var(--panel);
    color: var(--text);
    border: 1px solid var(--line);
    border-radius: 6px;
  }
  .styles {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .style {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 3px;
    padding: 10px 12px;
    border-radius: 12px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .style .si {
    font-size: 18px;
  }
  .style.on {
    border-color: color-mix(in srgb, var(--accent) 60%, var(--line));
    background: color-mix(in srgb, var(--accent) 10%, var(--bg-2));
  }
  .p-sub {
    margin: 10px 0 2px;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-2);
  }
  .mind-item {
    gap: 6px;
  }
  .mind-line {
    padding: 3px 0;
    font-size: 12.5px;
    line-height: 1.5;
  }
  .mind-done {
    margin-top: 8px;
  }
  .mind-state {
    margin-top: 10px;
    padding-top: 8px;
    border-top: 1px dashed var(--line);
  }
  .mind-state summary {
    cursor: pointer;
    font-size: 12.5px;
    color: var(--text-2);
    user-select: none;
  }
  .mind-state summary:hover {
    color: var(--text);
  }
  .lifecard .recent,
  .mind-done .recent {
    margin: 4px 0 0;
    padding-left: 16px;
    font-size: 12.5px;
    line-height: 1.6;
    color: var(--text-2);
  }
  @media (max-width: 420px) {
    .styles {
      grid-template-columns: 1fr;
    }
  }
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
