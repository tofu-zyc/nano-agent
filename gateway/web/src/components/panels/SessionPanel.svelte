<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api, sidPath } from '../../lib/api';
  import { ago, channelMeta, endpointLabel, homeShort, machineMeta, modeMeta, num, pct, shortPath } from '../../lib/format';
  import { portal } from '../../lib/portal';
  import type { BranchInfo, EndpointInfo, SessionInfo } from '../../lib/types';
  import WorkspacePicker from '../WorkspacePicker.svelte';
  import Icon from '../Icon.svelte';
  import AgentSwitcher from '../AgentSwitcher.svelte';

  let { sid }: { sid: string } = $props();
  // agent 自己的会话（拟人型的内心活动、整理记忆）：只能看，工作目录、权限这些对它没意义
  const innerKind = $derived(/^@[^/]+\/life$/.test(sid) ? 'life' : /^@[^/]+\/dream$/.test(sid) ? 'dream' : '');
  const innerAgent = $derived(store.agent(sid.replace(/^@([^/]+)\/.*$/, '$1')));

  const ov = $derived(store.ov!);
  const sess = $derived(store.session(sid));
  // 群记录会话（room:<id>）：显示群的名字、成员，能直接去房间节点
  const room = $derived(store.rooms.find((r) => r.sid === sid));
  // 列表里还没有这个会话（比如刚新建）：去网关问一下，确实没有才说不存在
  let gone = $state(false);
  $effect(() => {
    if (sess) return;
    let live = true;
    store.refreshSession(sid).then((ok) => {
      if (live && !ok) gone = true;
    });
    return () => {
      live = false;
    };
  });
  // 其他会话上的窗口，可以改接到这个会话
  const otherEndpoints = $derived(
    (ov.sessions ?? []).flatMap((s) => (s.sid === sid ? [] : (s.endpoints ?? []).filter((e) => e.channel !== 'web').map((e) => ({ e, from: s.title })))),
  );
  let picking = $state(false);
  const howText: Record<string, string> = { local: '内核自己执行', gateway: '网关执行', down: '用不了' };

  async function patch(p: Record<string, unknown>, quiet = false) {
    try {
      store.upsertSession(await api.patch<SessionInfo>(sidPath(sid), p));
      if (!quiet) store.toast({ kind: 'ok', text: '已保存，下一条消息开始生效' }, 2000);
    } catch (err) {
      store.fail(err);
    }
  }
  async function rename(title: string) {
    if (!sess || title.trim() === sess.title) return;
    await patch({ title }, true);
  }
  async function moveEndpoint(e: EndpointInfo) {
    try {
      await api.put(`/api/endpoints/${encodeURIComponent(e.key)}`, { session: sid });
      store.toast({ kind: 'ok', text: `${endpointLabel(e)} 已接到${sid === 'main' ? '主会话' : '这个会话'}` }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
  async function makeAdmin(e: EndpointInfo) {
    const who = e.name ? `${e.name}（${e.who}）` : e.who;
    if (!confirm(`把 ${who} 设为管理员？\n管理员能让机器人在工作目录里读写文件、执行命令，能看到和切换所有会话。这个窗口会接到主会话。`)) return;
    try {
      await api.post(`/api/endpoints/${encodeURIComponent(e.key)}/admin`);
      store.toast({ kind: 'ok', text: `${e.name || e.who} 现在是管理员了，窗口已接到主会话` }, 3000);
    } catch (err) {
      store.fail(err);
    }
  }
  async function action(a: 'reset' | 'stop') {
    if (a === 'reset' && !confirm('归档当前历史并清空上下文？工作目录等设置会保留。')) return;
    try {
      await api.post(`${sidPath(sid)}/${a}`);
      store.toast({ kind: 'ok', text: a === 'reset' ? '上下文已清空' : '已停止' });
      store.refreshSession(sid);
    } catch (err) {
      store.fail(err);
    }
  }
  // ---- 安静分支：看得到、能继续、能把结论带回来源会话（设计 §8.8） ----
  const branchStatus: Record<string, string> = {
    preparing: '准备中', pending_boundary: '等边界', ready: '就绪', running: '运行中', paused: '已暂停',
    needs_attention: '需要处理', completed: '已完成', archived: '已归档', cancelled: '已取消',
  };
  let branches = $state<BranchInfo[]>([]);
  let branchPrompt = $state('');
  let branchBusy = $state(false);
  $effect(() => {
    const id = sid;
    let live = true;
    api
      .get<{ branches: BranchInfo[] }>(`${sidPath(id)}/branches`)
      .then((out) => {
        if (live) branches = out.branches ?? [];
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  });
  async function branchAct(target: string, kind: string) {
    if (branchBusy) return;
    branchBusy = true;
    try {
      const prompt = kind === 'continue' ? branchPrompt.trim() || '接着上一次没做完的事继续' : '';
      await api.post(`/api/branches/${encodeURIComponent(target)}`, { action: kind, prompt });
      store.toast(
        {
          kind: 'ok',
          text: kind === 'continue' ? '分支在它自己的上下文里继续跑，结果留在分支里' : kind === 'report' ? '分支结论已经放进来源会话的下一个边界' : kind === 'pause' ? '已暂停这个分支' : kind === 'archive' ? '已归档这个分支' : '已提交',
        },
        2600,
      );
      store.refreshSession(sid);
    } catch (err) {
      store.fail(err);
    } finally {
      branchBusy = false;
    }
  }
  async function remove() {
    if (!sess || !confirm(`删除会话「${sess.title}」？它的历史和设置都会删掉，接着它的窗口下次说话时重新接（管理员接回主会话）。`)) return;
    try {
      await api.del(sidPath(sid));
      store.toast({ kind: 'ok', text: '会话已删除' });
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="pnl">
  {#if sess}
    {@const levels = sess.thinking_levels ?? ov.model.levels ?? []}
    <div class="p-actions">
      <a class="btn primary" href="#/sessions/{encodeURIComponent(sess.sid)}" onclick={() => (store.inspector = null)}><Icon name="chat" size={14} />打开对话</a>
      {#if sess.running}<button class="btn" onclick={() => action('stop')}><Icon name="stop" size={14} />停止</button>{/if}
      {#if !innerKind}<button class="btn" onclick={() => action('reset')}><Icon name="undo" size={14} />归档并重置上下文</button>{/if}
    </div>
    {#if innerKind}
      <div class="p-card">
        <p class="p-note">{innerKind === 'life' ? `这是${innerAgent?.name ?? '它'}的内心活动：它自己醒来时想的、做的、上网看到的。可以看，也可以在聊天框里给它留言；留言会叫醒它。多久醒一次、醒着的时间、主动找谁，在它的节点里改。` : `这是${innerAgent?.name ?? '它'}最近一次整理记忆的过程，只能看。整理出来的记忆和梦境日志在「上下文与记忆」节点里。`}</p>
        <div class="p-actions tight">
          {#if innerAgent}<button class="btn sm" onclick={() => store.open({ type: 'agent', id: innerAgent!.id })}><Icon name="persona" size={13} />去{innerAgent.name}的节点</button>{/if}
          {#if innerAgent}<button class="btn sm" onclick={() => store.open({ type: 'memory', agent: innerAgent!.id })}><Icon name="brain" size={13} />看它的记忆</button>{/if}
        </div>
      </div>
    {:else}
    <input class="p-input title" value={sess.title} maxlength="40" placeholder="会话的名字" title="改名字，回车保存"
      onchange={(e) => rename(e.currentTarget.value)} onkeydown={(e) => e.key === 'Enter' && e.currentTarget.blur()} />
    <div class="p-grid">
      <div class="p-stat"><b>{sess.messages}</b><span>条消息</span></div>
      <div class="p-stat"><b class="acc">{pct(sess.usage.cached_tokens, sess.usage.prompt_tokens)}</b><span>缓存命中</span></div>
      <div class="p-stat"><b>{num(sess.usage.prompt_tokens)}</b><span>累计输入</span></div>
    </div>

    {#if room}
      <div class="p-card">
        <div class="p-row">
          <Icon name="chat" size={15} />
          <span class="p-grow">群「{room.name}」</span>
          {#if room.paused}<span class="tag">已暂停</span>{/if}
          <button class="btn sm" onclick={() => store.open({ type: 'room', id: room!.id })}>去群节点</button>
        </div>
        <p class="p-note p-small">这里的每一条都会转给群里的 {room.members.map((m) => store.agent(m)?.name ?? m).join('、')}；被 @ 或叫到名字的会回。</p>
      </div>
    {/if}
    {#if sess.branch}
      <div class="p-card">
        <div class="p-row">
          <Icon name="hub" size={15} />
          <span class="p-grow">安静分支「{sess.branch.title || sess.sid}」</span>
          <span class="tag">{branchStatus[sess.branch.status] ?? sess.branch.status}</span>
        </div>
        <p class="p-note p-small">分叉点第 {sess.branch.fork_checkpoint} 条 · 结果默认留在这里，不会发回来源会话，也不会弹提醒。</p>
        <div class="p-actions tight">
          <a class="btn sm" href="#/sessions/{encodeURIComponent(sess.branch.parent_sid)}" onclick={() => (store.inspector = null)}>去来源会话</a>
          <button class="btn sm" disabled={branchBusy} onclick={() => branchAct(sess.sid, 'report')}>把结论带回来源会话</button>
        </div>
      </div>
    {:else}
      <div class="p-card">
        <div class="p-row">
          <Icon name="hub" size={15} />
          <span class="p-grow">安静分支</span>
          <span class="tag">{branches.length}</span>
        </div>
        {#if branches.length === 0}
          <p class="p-note p-small">还没有分支。需要保留完整上下文、跨多次继续做一件事时，模型会用 Session 工具在这里分一个安静分支。</p>
        {:else}
          {#each branches as b (b.sid)}
            <div class="branch-row">
              <div class="p-row">
                <a class="p-grow" href="#/sessions/{encodeURIComponent(b.sid)}" onclick={() => (store.inspector = null)}>{b.title || b.sid}</a>
                <span class="tag">{branchStatus[b.status] ?? b.status}{b.running ? ' · 正在跑' : ''}</span>
              </div>
              <p class="p-note p-small">分叉点第 {b.fork_checkpoint} 条 · {b.delivery_policy === 'silent' ? '安静运行' : b.delivery_policy}{b.last_active ? ' · 最近活动 ' + ago(b.last_active * 1000) : ''}</p>
              <div class="p-actions tight">
                <button class="btn sm" disabled={branchBusy} onclick={() => branchAct(b.sid, 'continue')}>继续</button>
                <button class="btn sm" disabled={branchBusy} onclick={() => branchAct(b.sid, 'pause')}>暂停</button>
                <button class="btn sm" disabled={branchBusy} onclick={() => branchAct(b.sid, 'report')}>引入结论</button>
              </div>
            </div>
          {/each}
          <label class="p-note p-small">继续时要做的事（可空）<input class="p-input" bind:value={branchPrompt} placeholder="空表示：接着上一次没做完的事继续" /></label>
        {/if}
      </div>
    {/if}
    {#if sess.group}
      <div class="p-warn">这是在群聊里的会话：每条群消息都记进历史，被 @ 或叫到名字才回；群里安静够了、额度也够时可以插一句。默认不挂工作目录，工具只有群聊白名单里的。</div>
    {/if}

    <h5 class="p-h">对话 agent <span class="muted p-small">保留聊天记录，下一条消息由新 agent 回复</span></h5>
    <div class="p-card">
      <div class="p-row">
        <AgentSwitcher {sid} />
        <button class="btn sm" onclick={() => store.open({ type: 'agent', id: sess.agent })}>去它的节点</button>
      </div>
      <p class="p-note p-small">
        换过去以后，人格、模型、工具、每条消息的上限、记忆和「在做什么」都按新 agent 算；这个会话里单独设过的人格、模型和思考强度仍然优先。
        会话 ID、历史、接入窗口和各处设置都不变。正在处理、排队或整理上下文时不能切换。
      </p>
      {#if sess.route_agent !== sess.agent}
        <p class="p-note p-small">会话 ID 的路由前缀仍属于 <code>{sess.route_agent}</code>：机器人窗口、主会话和内部会话的接线不跟着切换变。</p>
      {/if}
    </div>

    <h5 class="p-h">接入的窗口 <span class="muted p-small">在哪个窗口说话都是同一段上下文</span></h5>
    <div class="p-card">
      {#each sess.endpoints ?? [] as e (e.key)}
        <div class="p-row">
          <span class="dot" style="background: {channelMeta[e.channel]?.color ?? 'var(--muted)'}"></span>
          <span class="p-grow">{endpointLabel(e)}</span>
          {#if e.channel === 'web'}
            <span class="muted p-small">能接入任何会话</span>
          {:else if !e.admin}
            <span class="tag" title="不是管理员：只能聊天，不能用工具、不能切换会话">访客</span>
            {#if e.who}<button class="btn sm" onclick={() => makeAdmin(e)}>设为管理员</button>{/if}
          {:else}
            <code class="muted p-small">{e.key}</code>
          {/if}
        </div>
      {:else}
        <p class="p-note">还没有聊天窗口接在这里，只能在网页里聊。</p>
      {/each}
      {#if otherEndpoints.length}
        <select class="attach" onchange={(ev) => { const i = +ev.currentTarget.value; ev.currentTarget.value = ''; if (i >= 0) moveEndpoint(otherEndpoints[i].e); }}>
          <option value="">把其他窗口接到这里…</option>
          {#each otherEndpoints as o, i}
            <option value={i}>{endpointLabel(o.e)}（现在在「{o.from}」）</option>
          {/each}
        </select>
      {/if}
    </div>

    <div class="p-card"><div class="p-row"><span class="p-k">隔离请求</span><span class="p-grow">{sess.sandbox_mode || 'auto'}</span><button class="p-lnk" onclick={() => store.open({type:'agent',id:sess.agent})}>agent 设置</button></div><p class="p-note">工作目录可以覆盖，节点部署要求仍优先；每次调用结果记录实际状态。隔离与审批、文件工具权限分别生效。</p></div>
    <h5 class="p-h">工作目录 <span class="muted p-small">{sess.workspaces.length ? `挂了 ${sess.workspaces.length} 个${sess.kernel ? ` · 由内核 ${sess.kernel} 跑` : ''}` : '在哪些机器的哪些目录上干活'}</span></h5>
    <div class="mounts">
      {#each sess.workspaces as mt (mt.id)}
        {@const sm = store.machines.find((x) => x.name === mt.host)}
        <div class="mount" class:off={mt.how === 'down'}>
          <Icon name={machineMeta[sm?.kind ?? 'kernel'].icon} size={14} />
          <button class="mp" onclick={() => store.open({ type: 'workspace', id: mt.id })} title="{mt.host}:{mt.path}">
            <b>{mt.host}</b><code>{shortPath(homeShort(mt.path), 34)}</code>
          </button>
          <span class="tag" title="执行隔离请求">{mt.sandbox_mode || sess.sandbox_mode || 'auto'}</span><span class="how {mt.how}">{howText[mt.how]}</span>
          <button class="x" title="取下" aria-label="取下 {mt.host}:{mt.path}" onclick={() => patch({ workspaces: sess.workspaces.filter((x) => x.id !== mt.id).map((x) => x.id) })}><Icon name="x" size={12} /></button>
        </div>
      {:else}
        <div class="mount empty"><Icon name="chat" size={14} /><span>在 HOME 下工作；文件工具写目录外要提权，Shell 访问范围由执行隔离决定</span></div>
      {/each}
      <button class="btn sm" onclick={() => (picking = true)}><Icon name="plus" size={13} />{sess.workspaces.length ? '挂载 / 取下…' : '挂载工作目录…'}</button>
    </div>

    <h5 class="p-h">权限模式 {#if !sess.workspaces.length}<span class="muted p-small">挂了工作目录之后生效</span>{/if}</h5>
    <div class="modes" class:inactive={!sess.workspaces.length}>
      {#each Object.entries(modeMeta) as [m, meta]}
        <button class:active={sess.mode === m} onclick={() => sess.mode !== m && patch({ mode: m })}>
          <strong>{meta.label}</strong>
          <span>{meta.desc}</span>
        </button>
      {/each}
    </div>

    <h5 class="p-h">模型 <span class="muted p-small">{sess.model ? '这个会话单独选的' : `默认：${sess.model_label}`}</span></h5>
    <select class="p-select" value={sess.model} onchange={(ev) => patch({ model: ev.currentTarget.value })}>
      <option value="">{sess.model_label ? `默认模型（${sess.model_label}）` : '默认模型（还没有配置）'}</option>
      {#each ov.models?.models ?? [] as m (m.id)}
        <option value={m.id}>{m.label}</option>
      {/each}
    </select>

    {#if levels.length}
      <h5 class="p-h">思考强度 <span class="muted p-small">{sess.thinking ? sess.thinking : '模型默认'}</span></h5>
      <div class="p-seg">
        <button class:active={!sess.thinking} onclick={() => patch({ thinking: '' })}>默认</button>
        {#each levels as l}
          <button class:active={sess.thinking === l} onclick={() => patch({ thinking: l })}>{l === 'none' ? '关闭' : l}</button>
        {/each}
      </div>
    {/if}

    <h5 class="p-h">人格</h5>
    <div class="p-card">
      {#if sess.persona === sess.persona_file}
        <div class="p-row">
          <Icon name="persona" size={15} />
          <span class="p-grow">这个会话的专属人格</span>
          <button class="btn sm" onclick={() => (store.persona = { name: sess.persona_file, isNew: false })}>编辑</button>
        </div>
      {:else}
        <div class="p-row">
          <Icon name="persona" size={15} />
          <span class="p-grow">用这个 agent 自己的人格</span>
          <button class="btn sm" onclick={() => store.open({ type: 'agent', id: sess.agent })}>去 Agent</button>
        </div>
        <button class="btn sm ghost" onclick={() => (store.persona = { name: sess.persona_file, isNew: true })}><Icon name="plus" size={13} />新建一个只给这个会话用的人格</button>
        <p class="p-note p-small">新建的人格文件放在 `~/.nano-agent/personas/`，只在这个会话生效；别的会话、别的 agent 不受影响。想让人格长期用，就在拓扑里多加一个 agent。</p>
      {/if}
    </div>
    {#if sess.compacted}
      <p class="p-note" style="margin-top: 10px">这个会话的历史超过过上限，已经裁掉最早的 {sess.compacted} 条消息。</p>
    {/if}
    {#if sess.sid !== 'main'}
      <div class="p-danger-zone">
        <button class="btn sm danger" onclick={remove}><Icon name="trash" size={13} />删除这个会话</button>
      </div>
    {/if}
    {/if}
  {:else if gone}
    <p class="p-note">没有这个会话，可能已经删掉了。</p>
  {:else}
    <p class="p-note">正在读取…</p>
  {/if}
</div>

{#if picking && sess}
  <div use:portal>
    <WorkspacePicker title="挂载工作目录" mounted={sess.workspaces.map((m) => m.id)} onchange={(ids) => patch({ workspaces: ids })} onclose={() => (picking = false)} />
  </div>
{/if}

<style>
  .title {
    margin-bottom: 14px;
    font-size: 14px;
    font-weight: 600;
  }
  select.attach {
    height: 32px;
    padding: 0 9px;
    border-radius: 9px;
    border: 1px dashed var(--line-2);
    background: var(--panel-solid);
    font-size: 12.5px;
    color: var(--text-2);
    cursor: pointer;
  }
  .mounts {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .mounts > :global(.btn) {
    align-self: flex-start;
    margin-top: 2px;
  }
  .mount {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 8px 8px 11px;
    border-radius: 11px;
    border: 1px solid var(--line);
    background: var(--panel-2);
    font-size: 12.5px;
  }
  .mount > :global(svg) {
    color: var(--c-kernel);
    flex: none;
  }
  .mount.off {
    border-color: color-mix(in srgb, var(--warn) 45%, var(--line));
  }
  .mount.empty {
    color: var(--muted);
  }
  .mp {
    flex: 1;
    min-width: 0;
    display: flex;
    gap: 7px;
    align-items: baseline;
    border: 0;
    background: none;
    padding: 0;
    color: var(--text);
    cursor: pointer;
    text-align: left;
    font: inherit;
  }
  .mp b {
    flex: none;
    white-space: nowrap;
  }
  .mp code {
    color: var(--c-dir);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .how {
    flex: none;
    font-size: 11px;
    padding: 2px 7px;
    border-radius: 999px;
    color: var(--muted);
    background: var(--bg-2);
  }
  .how.local {
    color: var(--c-kernel);
  }
  .how.down {
    color: var(--warn);
  }
  .x {
    flex: none;
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border-radius: 6px;
    border: 0;
    background: none;
    color: var(--muted);
    cursor: pointer;
  }
  .x:hover {
    color: var(--err);
    background: var(--bg-2);
  }
  .modes {
    display: grid;
    grid-template-columns: repeat(2, 1fr);
    gap: 7px;
  }
  .modes.inactive {
    opacity: 0.6;
  }
  .modes button {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 10px;
    border-radius: 12px;
    text-align: left;
    background: var(--panel-2);
    border: 1px solid var(--line);
    transition: border-color 0.15s, background 0.15s;
  }
  .modes button span {
    font-size: 11px;
    color: var(--muted);
    line-height: 1.4;
  }
  .modes button:hover {
    border-color: var(--line-2);
  }
  .modes button.active {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 12%, var(--panel-2));
  }
</style>
