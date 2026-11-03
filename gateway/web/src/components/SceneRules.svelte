<script lang="ts">
  // 场景规则：内部群和外部群共用。说明和角色提示词放在显眼处，其余收在“更多规则”里。
  // onsave 收到要改的字段；内部群直接 PATCH 群，外部群由调用方合成整块 scene。
  import { api } from '../lib/api';
  import { store } from '../lib/state.svelte';
  import { toolLabel } from '../lib/labels';
  import { ago } from '../lib/format';
  import type { ObserveRecord, SceneRules, SceneUse, TeamRules } from '../lib/types';

  let {
    rules,
    use,
    sceneKey,
    external = false,
    members,
    speakers,
    onsave,
    onreset,
  }: {
    rules: SceneRules;
    use?: SceneUse;
    sceneKey: string;
    external?: boolean;
    members: string[];
    speakers?: Record<string, string>; // 外部群里说过话的人：名字 → 身份串
    onsave: (p: Record<string, unknown>) => Promise<void> | void;
    onreset?: () => void;
  } = $props();

  // 场景里可选的工具：由后端从登记表返回
  const sceneTools = $derived(rules.stage ? store.stageTools : store.sceneTools); // 戏里只有联网查资料的两个
  const agentName = (id: string) => store.agents.find((a) => a.id === id)?.name ?? id;
  const eagerLabels: Record<string, string> = { '': '沿用 agent 的设置', off: '不主动插话', low: '低', normal: '中', high: '高' };

  const customTools = $derived(rules.tools !== null && rules.tools !== undefined);
  function setTool(name: string, on: boolean) {
    const cur = rules.tools ?? [];
    onsave({ tools: on ? [...cur, name] : cur.filter((t) => t !== name) });
  }
  function setRole(id: string, field: 'prompt' | 'eagerness', value: string) {
    const roles = { ...(rules.roles ?? {}) };
    roles[id] = { ...(roles[id] ?? {}), [field]: value };
    if (!roles[id].prompt && !roles[id].eagerness) delete roles[id];
    onsave({ roles });
  }
  const num = (e: Event) => Number((e.target as HTMLInputElement).value) || 0;
  const str = (e: Event) => (e.target as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement).value;

  // 团队：名单里的人点名 agent 才能建工单。名单从这个群说过话的人里勾选，显示名字、存身份串
  const team = $derived(rules.team ?? null);
  const speakerList = $derived(Object.entries(speakers ?? {}).sort((a, b) => a[0].localeCompare(b[0])));
  const speakerName = (who: string) => speakerList.find(([, w]) => w === who)?.[0] ?? who;
  const workspaces = $derived(store.ov?.workspaces ?? []);
  function saveTeam(p: Partial<TeamRules>) {
    onsave({ team: { roster: [], workspace: '', ...(team ?? {}), ...p } });
  }
  function toggleRoster(who: string, on: boolean) {
    const cur = team?.roster ?? [];
    saveTeam({ roster: on ? [...cur.filter((w) => w !== who), who] : cur.filter((w) => w !== who) });
  }

  let recs = $state<ObserveRecord[]>([]);
  let showSilent = $state(false);
  async function loadObserve() {
    try {
      const r = await api.get<{ items: ObserveRecord[] }>(`/api/observe?scene=${encodeURIComponent(sceneKey)}&n=50${showSilent ? '&all=1' : ''}`);
      recs = r.items ?? [];
    } catch (err) {
      store.fail(err);
    }
  }
  $effect(() => {
    if (rules.observe) {
      sceneKey;
      showSilent;
      loadObserve();
    } else recs = [];
  });
</script>

<h5 class="p-h">场景说明 <span class="muted p-small">每个成员都看得到；改了之后这个场景成员的前缀缓存失效一次</span></h5>
<div class="p-card">
  <textarea class="p-input" rows="3" maxlength="2000" placeholder="例如：这是一场谋杀之夜，晚宴上有人被毒死……" value={rules.brief ?? ''} onchange={(e) => onsave({ brief: str(e) })}></textarea>
</div>

<h5 class="p-h">角色提示词 <span class="muted p-small">每个成员在这里扮演什么</span></h5>
<div class="p-card">
  {#each members as id (id)}
    <label class="p-label">{agentName(id)}
      <textarea class="p-input" rows="2" maxlength="2000" value={rules.roles?.[id]?.prompt ?? ''} onchange={(e) => setRole(id, 'prompt', str(e))}></textarea>
    </label>
    <label class="p-label">{agentName(id)} 的积极度
      <select class="p-select" value={rules.roles?.[id]?.eagerness ?? ''} onchange={(e) => setRole(id, 'eagerness', str(e))}>
        {#each Object.entries(eagerLabels) as [v, label] (v)}<option value={v}>{label}</option>{/each}
      </select>
    </label>
  {/each}
</div>

{#if external}
  <h5 class="p-h">团队 <span class="muted p-small">名单里的人点名 agent，agent 才会建工单；发言固定为点名，主动插话只记录</span></h5>
  <div class="p-card" data-testid="team-card">
    <label class="p-row"><input type="checkbox" checked={!!team} data-testid="team-on" onchange={(e) => (e.target as HTMLInputElement).checked ? saveTeam({ workspace: workspaces[0]?.id ?? '' }) : onsave({ team: null })} />
      <span class="p-grow">这个群是一个团队</span></label>
    {#if team}
      <div class="p-label">准入名单（管理员总算在内）
        {#each speakerList as [name, who] (who)}
          <label class="p-row"><input type="checkbox" checked={team.roster.includes(who)} onchange={(e) => toggleRoster(who, (e.target as HTMLInputElement).checked)} />
            <span>{name} <span class="tiny">{who}</span></span></label>
        {:else}
          <p class="p-note p-small">这个群里还没有人说过话。有人说话以后，在这里勾选。</p>
        {/each}
        {#each team.roster.filter((w) => !speakerList.some(([, x]) => x === w)) as who (who)}
          <label class="p-row"><input type="checkbox" checked onchange={(e) => toggleRoster(who, (e.target as HTMLInputElement).checked)} />
            <span>{speakerName(who)} <span class="tiny">不在最近说过话的人里</span></span></label>
        {/each}
      </div>
      <label class="p-label">仓库所在的工作目录
        <select class="p-select" value={team.workspace} data-testid="team-workspace" onchange={(e) => saveTeam({ workspace: str(e) })}>
          <option value="">（选一个）</option>
          {#each workspaces as w (w.id)}<option value={w.id}>{w.path}（{w.host || '本机'}）</option>{/each}
        </select>
      </label>
      <label class="p-label">验收命令（在工单的工作区里跑，如 make test；空表示只看有没有改动）
        <input class="p-input" maxlength="500" value={team.check ?? ''} onchange={(e) => saveTeam({ check: str(e) })} /></label>
      <label class="p-label">每个工单最多用多少 token（0 用默认 2000000）
        <input class="p-input" inputmode="numeric" value={team.budget ?? 0} onchange={(e) => saveTeam({ budget: num(e) })} /></label>
    {/if}
  </div>
{/if}

<details class="p-more">
  <summary>更多规则</summary>
  <div class="p-card">
    <label class="p-label">说话方式
      <select class="p-select" value={rules.mode ?? ''} onchange={(e) => {
        const mode = str(e);
        // 轮流必须设轮数或这一场的预算：两者都没设就一起发 5 轮（之后可以改）
        onsave(mode === 'turns' && !rules.rounds && !rules.budget_total ? { mode, rounds: 5 } : { mode });
      }}>
        <option value="">默认（{external ? '自由' : '点名'}）</option>
        <option value="mention">点名：只有被 @ 或者叫到名字才说</option>
        <option value="free">自由：安静下来按积极度自己插话</option>
        {#if !external}
          <option value="turns">轮流：人或旁白说一句之后，按顺序每人说一句（要设轮数）</option>
          <option value="director">导演：只有导演点到的那一个会说话</option>
        {/if}
      </select>
    </label>
    {#if rules.stage}
      <label class="p-row"><input type="checkbox" checked={!!rules.inner} onchange={(e) => onsave({ inner: (e.target as HTMLInputElement).checked })} />
        <span class="p-grow">心里话：成员每次先写一句心里想的，别人听不到，记进时间线（下一轮生效）</span></label>
    {/if}
    <label class="p-row"><input type="checkbox" checked={!!rules.observe} onchange={(e) => onsave({ observe: (e.target as HTMLInputElement).checked })} />
      <span class="p-grow">observe：主动插话只记录，不发出（被点名的回复不受影响）</span></label>

    <div class="p-label">工具
      {#if !rules.stage}
        <label class="p-row"><input type="checkbox" checked={!customTools} onchange={(e) => onsave({ tools: (e.target as HTMLInputElement).checked ? null : [] })} /> <span>沿用默认（agent 的群聊工具，没设就是搜索、技能、记忆、发文件）</span></label>
      {:else}
        <p class="p-note p-small">这是一场戏：场内的事不进记忆，所以没设就一个工具都不给，只能选联网查资料的。</p>
      {/if}
      {#if customTools || rules.stage}
        {#each sceneTools as t (t)}
          <label class="p-row"><input type="checkbox" checked={(rules.tools ?? []).includes(t)} onchange={(e) => setTool(t, (e.target as HTMLInputElement).checked)} /> <span>{toolLabel(t)} <span class="tiny">{t}</span></span></label>
        {/each}
        {#if !(rules.tools ?? []).length && !rules.stage}<p class="p-note p-small">一个都没选：这个场景不给工具。</p>{/if}
      {/if}
    </div>

    <label class="p-label">安静多久以后才可能插话（如 20s、2m；空表示沿用）
      <input class="p-input" value={rules.style?.quiet ?? ''} onchange={(e) => onsave({ style: { ...rules.style, quiet: str(e) } })} /></label>
    <label class="p-label">两次发言至少隔多久（如 10s）
      <input class="p-input" value={rules.style?.min_gap ?? ''} onchange={(e) => onsave({ style: { ...rules.style, min_gap: str(e) } })} /></label>
    {#if external}
      <label class="p-label">一条最多多少字（0 沿用）
        <input class="p-input" inputmode="numeric" value={rules.style?.max_len ?? 0} onchange={(e) => onsave({ style: { ...rules.style, max_len: num(e) } })} /></label>
      <label class="p-label">最多拆成几条（0 沿用，最多 5）
        <input class="p-input" inputmode="numeric" value={rules.style?.parts ?? 0} onchange={(e) => onsave({ style: { ...rules.style, parts: num(e) } })} /></label>
    {:else if rules.mode !== 'turns' && rules.mode !== 'director'}
      <label class="p-label">连续几条 agent 发言就暂停（0 默认 8，范围 2–50）
        <input class="p-input" inputmode="numeric" value={rules.guard?.run ?? 0} onchange={(e) => onsave({ guard: { ...rules.guard, run: num(e) } })} /></label>
      <label class="p-label">两个 agent 来回几轮就暂停（0 默认 6，范围 2–50）
        <input class="p-input" inputmode="numeric" value={rules.guard?.ping_pong ?? 0} onchange={(e) => onsave({ guard: { ...rules.guard, ping_pong: num(e) } })} /></label>
    {/if}

    <label class="p-label">每天最多用多少 token（0 不限；{external ? '超了当天不再调用模型，被点名时一天回一次说明' : '超了自动暂停'}）
      <input class="p-input" inputmode="numeric" value={rules.budget ?? 0} onchange={(e) => onsave({ budget: num(e) })} /></label>
    {#if !external}
      <label class="p-label">这一场最多用多少 token（0 不限）
        <input class="p-input" inputmode="numeric" value={rules.budget_total ?? 0} onchange={(e) => onsave({ budget_total: num(e) })} /></label>
    {/if}
    {#if use}
      <p class="p-note p-small">
        今天用了 {use.day_tokens} token，这一场累计 {use.total}{#if use.since}（{ago(use.since * 1000)}开始）{/if}{#if use.over}，<b class="warn">已超预算</b>{/if}。
        {#if onreset}<button class="p-lnk" onclick={onreset}>累计清零</button>{/if}
      </p>
    {/if}
  </div>
</details>

{#if rules.observe}
  <h5 class="p-h">observe 记录 <span class="muted p-small">主动插话本来会说的话（没有发出）</span></h5>
  <div class="p-card">
    <label class="p-row"><input type="checkbox" bind:checked={showSilent} /> <span>也显示“[不回复]”的判断</span></label>
    {#each recs as r (r.at + r.sid + (r.say ?? ''))}
      <div class="p-note p-small">
        <b>{agentName(r.agent)}</b> · {ago(r.at * 1000)}
        {#if r.silent}<span class="muted">本来也不会说话</span>{:else}
          {#if r.say}<div>说：{r.say}</div>{/if}
          {#each r.tools ?? [] as t}<div class="muted">调用 {t.name} {t.args ?? ''}</div>{/each}
        {/if}
        <div class="muted">安静 {r.why.quiet_sec} 秒 · {r.why.pending} 条新消息 · 门槛 {r.why.gate.toFixed(2)} · {r.prompt_tokens + r.completion_tokens} token</div>
      </div>
    {:else}
      <p class="p-note p-small">还没有记录。</p>
    {/each}
  </div>
{/if}
