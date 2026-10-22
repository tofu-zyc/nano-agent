<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { ago, channelMeta, endpointLabel, homeShort, machineMeta, modeMeta, num, pct, shortPath, since, stateLabel } from '../lib/format';
  import type { EndpointInfo, MachineInfo, SessionInfo, WorkspaceInfo } from '../lib/types';
  import FieldGroup from './FieldGroup.svelte';
  import ModelPanel from './ModelPanel.svelte';
  import DirPicker from './DirPicker.svelte';
  import WorkspacePicker from './WorkspacePicker.svelte';
  import FeishuQR from './FeishuQR.svelte';
  import Icon from './Icon.svelte';

  const target = $derived(store.inspector!);
  const ov = $derived(store.ov!);
  const targetKey = $derived(target.type === 'session' ? `session:${target.sid}` : JSON.stringify(target));
  let bodyEl = $state<HTMLDivElement>();

  const head = $derived.by(() => {
    switch (target.type) {
      case 'gateway':
        return { icon: 'hub', color: 'var(--c-gateway)', title: '网关', sub: `已运行 ${since(ov.gateway.started)}` };
      case 'model': {
        const dm = ov.models?.models?.find((m) => m.default);
        const de = ov.models?.endpoints?.find((e) => e.id === dm?.endpoint);
        const n = ov.models?.models?.length ?? 0;
        const en = ov.models?.endpoints?.length ?? 0;
        return { icon: 'sparkles', color: 'var(--c-model)', title: '模型',
          sub: n ? `${n} 个模型 · ${en} 个接口${dm ? ' · 默认 ' + dm.label : ''}` : '还没有配置模型' };
      }
      case 'kernel': {
        if (target.node) {
          const k = ov.kernels.find((x) => x.node === target.node);
          return { icon: 'shield', color: k ? 'var(--c-kernel)' : 'var(--muted)', title: target.node,
            sub: k ? `内核 · ${k.managed ? '网关启动的本机内核' : k.local ? '在本机' : '在别的电脑上'}` : '内核 · 不在线' };
        }
        return { icon: 'shield', color: 'var(--c-kernel)', title: '内核', sub: ov.kernels.length ? `${ov.kernels.length} 个在线` : '没有连接' };
      }
      case 'machine': {
        if (!target.name) return { icon: 'hub', color: 'var(--c-kernel)', title: '机器', sub: '能放工作目录的地方' };
        const m = store.machines.find((x) => x.name === target.name);
        const kind = machineMeta[m?.kind ?? 'kernel'];
        return { icon: kind.icon, color: m?.online ? 'var(--c-kernel)' : 'var(--muted)', title: target.name,
          sub: (m?.kind === 'local' ? '网关所在的机器' : kind.label) + (m?.online ? ' · 在线' : ' · 现在用不了') };
      }
      case 'workspace': {
        if (!target.id) return { icon: 'folder', color: 'var(--c-dir)', title: '工作目录', sub: '机器 + 目录，会话从这里选' };
        const w = (ov.workspaces ?? []).find((x) => x.id === target.id);
        return { icon: 'folder', color: 'var(--c-dir)', title: w ? homeShort(w.path) : '工作目录', sub: w ? `${w.host} 上的工作目录` : '已经删掉了' };
      }
      case 'channel': {
        const m = channelMeta[target.name];
        const c = ov.channels.find((x) => x.name === target.name);
        return { icon: m?.icon ?? 'link', color: m?.color ?? 'var(--muted)', title: m?.label ?? target.name, sub: c ? (c.status.identity ? `${stateLabel[c.status.state]} · ${c.status.identity}` : stateLabel[c.status.state]) : '' };
      }
      case 'session': {
        const s = store.session(target.sid);
        if (target.sid === 'main') return { icon: 'hub', color: 'var(--accent)', title: s?.title ?? '主会话', sub: '你的所有窗口默认都接在这里' };
        return { icon: 'chat', color: channelMeta[s?.channel ?? '']?.color ?? 'var(--c-session)', title: s?.title ?? target.sid, sub: target.sid };
      }
    }
  });


  // ---- 会话 ----
  const sess = $derived(target.type === 'session' ? store.session(target.sid) : undefined);
  // 列表里还没有这个会话（比如刚新建）：去网关问一下，确实没有才说不存在
  let gone = $state<string | null>(null);
  let goneSeq = 0;
  $effect(() => {
    if (target.type !== 'session' || sess) return;
    const sid = target.sid;
    const seq = ++goneSeq;
    store.refreshSession(sid).then((ok) => {
      if (seq === goneSeq && !ok) gone = sid;
    });
  });
  // 其他会话上的窗口，可以改接到这个会话
  const otherEndpoints = $derived(
    target.type === 'session'
      ? (ov.sessions ?? []).flatMap((s) =>
          s.sid === target.sid ? [] : (s.endpoints ?? []).filter((e) => e.channel !== 'web').map((e) => ({ e, from: s.title })),
        )
      : [],
  );
  async function attach(e: EndpointInfo) {
    if (target.type === 'session') await moveEndpoint(e, target.sid);
  }
  async function moveEndpoint(e: EndpointInfo, sid: string) {
    try {
      await api.put(`/api/endpoints/${encodeURIComponent(e.key)}`, { session: sid });
      store.toast({ kind: 'ok', text: `${endpointLabel(e)} 已接到${sid === 'main' ? '主会话' : '这个会话'}` }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
  // 把在这个窗口说话的人设为管理员：写进管理员名单，窗口接到主会话
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
  // 某个渠道上聊过天的人（每个私聊是一个窗口）
  const people = $derived(
    target.type === 'channel'
      ? (ov.sessions ?? []).flatMap((s) => (s.endpoints ?? []).filter((e) => e.channel === target.name).map((e) => ({ e, s })))
      : [],
  );
  // 挂载、取下工作目录（可以同时挂好几个，在不同的机器上）
  let picking = $state(false);
  async function mountWorkspaces(ids: string[]) {
    await patchSession({ workspaces: ids });
  }
  const howText: Record<string, string> = { local: '内核自己执行', gateway: '网关执行', down: '用不了' };
  async function rename(title: string) {
    if (!sess || title.trim() === sess.title) return;
    try {
      store.upsertSession(await api.patch<SessionInfo>(sidPath(sess.sid), { title }));
    } catch (err) {
      store.fail(err);
    }
  }
  async function deleteSession() {
    if (!sess || !confirm(`删除会话「${sess.title}」？它的历史和设置都会删掉，接着它的窗口下次说话时重新接（管理员接回主会话）。`)) return;
    try {
      await api.del(sidPath(sess.sid));
      store.toast({ kind: 'ok', text: '会话已删除' });
    } catch (err) {
      store.fail(err);
    }
  }
  async function patchSession(p: Record<string, unknown>) {
    if (target.type !== 'session') return;
    try {
      const info = await api.patch<SessionInfo>(sidPath(target.sid), p);
      store.upsertSession(info);
      store.toast({ kind: 'ok', text: '已保存，下一条消息开始生效' }, 2000);
    } catch (err) {
      store.fail(err);
    }
  }
  async function sessionAction(action: 'reset' | 'stop') {
    if (target.type !== 'session') return;
    if (action === 'reset' && !confirm('清空这个会话的上下文？工作目录等设置会保留。')) return;
    try {
      await api.post(`${sidPath(target.sid)}/${action}`);
      store.toast({ kind: 'ok', text: action === 'reset' ? '上下文已清空' : '已停止' });
      store.refreshSession(target.sid);
    } catch (err) {
      store.fail(err);
    }
  }

  const toolDesc: Record<string, string> = {
    Read: '读文件、列目录',
    Grep: '搜索文件内容',
    Write: '新建或覆盖文件',
    Edit: '精确替换一段文本',
    Shell: '执行命令',
    Skill: '加载技能说明和附带文件',
  };

  // ---- 机器和工作目录 ----
  const machine = $derived(target.type === 'machine' && target.name ? store.machines.find((m) => m.name === target.name) : undefined);
  const ws = $derived(target.type === 'workspace' && target.id ? (ov.workspaces ?? []).find((w) => w.id === target.id) : undefined);
  const wsSessions = $derived(ws ? (ov.sessions ?? []).filter((s) => s.workspaces.some((m) => m.id === ws.id)) : []);
  let browseOn = $state<MachineInfo | null>(null);
  let addingWs = $state(false);
  // 切换对象：面板重新从顶部开始，也不保留上一个对象的“正在读取 / 已经删掉”状态
  $effect(() => {
    targetKey;
    goneSeq++;
    gone = null;
    picking = false;
    addingWs = false;
    browseOn = null;
    bodyEl?.scrollTo({ top: 0 });
  });
  async function addOn(path: string) {
    const m = browseOn!;
    browseOn = null;
    try {
      const w = await api.post<WorkspaceInfo>('/api/workspaces', { host: m.name, path });
      store.open({ type: 'workspace', id: w.id });
    } catch (err) {
      store.fail(err);
    }
  }
  async function setDefault(w: WorkspaceInfo) {
    try {
      await api.put('/api/workspaces/default', { id: w.id, default: !w.default });
      store.toast({ kind: 'ok', text: w.default ? '新会话不再默认挂它' : '新会话会默认挂上它' }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
  async function removeWorkspace(w: WorkspaceInfo) {
    if (!confirm(`从列表里删掉 ${w.host}:${w.path}？${w.sessions ? `挂着它的 ${w.sessions} 个会话会取下它。` : ''}机器上的文件不会动。`)) return;
    try {
      await api.del(`/api/workspaces/${w.id}`);
      store.open({ type: 'workspace' });
    } catch (err) {
      store.fail(err);
    }
  }
  function machineStatus(m: MachineInfo | undefined) {
    if (!m) return '不在线';
    if (!m.online) return m.detail || '现在用不了';
    if (m.kind === 'local') return m.kernels.length ? `内核 ${m.kernels.join('、')} 在这台机器上，工具由它直接执行` : '这台机器上没有在线的内核，工具由网关直接执行';
    if (m.kind === 'ssh') return '网关通过 SSH 连着，工具由网关在服务器上执行（服务器上不跑内核）';
    return `内核 ${m.name} 在这台电脑上，工具由它直接执行`;
  }
</script>

<aside class="insp" class:wide={target.type === 'model'} style="--c: {head.color}">
  <header>
    <div class="disc"><Icon name={head.icon} size={20} /></div>
    <div class="ht">
      <h3>{head.title}</h3>
      <div class="sub">{head.sub}</div>
    </div>
    <button class="btn ghost sm" onclick={() => (store.inspector = null)} aria-label="关闭"><Icon name="x" size={15} /></button>
  </header>

  <div class="body" bind:this={bodyEl}>
    {#if target.type === 'gateway'}
      {@const t = ov.totals}
      <div class="grid">
        <div class="stat"><b>{t.runs}</b><span>处理轮数</span></div>
        <div class="stat"><b>{t.model_calls}</b><span>模型调用</span></div>
        <div class="stat"><b class="acc">{pct(t.cached_tokens, t.prompt_tokens)}</b><span>前缀缓存命中</span></div>
        <div class="stat"><b>{num(t.prompt_tokens)}</b><span>输入 tokens</span></div>
        <div class="stat"><b>{num(t.completion_tokens)}</b><span>输出 tokens</span></div>
        <div class="stat"><b>{num(t.reasoning_tokens)}</b><span>其中思考</span></div>
      </div>
      <div class="kv">
        <div><span>内核监听</span><code>{ov.gateway.kernel_listen}</code></div>
        <div><span>网页地址</span><code>{ov.gateway.web_listen}</code></div>
        <div><span>数据目录</span><code>{homeShort(ov.gateway.data_dir)}</code></div>
      </div>
      <FieldGroup group="gateway" focus={store.focus} />
    {:else if target.type === 'model'}
      <ModelPanel />
    {:else if target.type === 'kernel'}
      {@const list = target.node ? ov.kernels.filter((k) => k.node === target.node) : ov.kernels}
      {#if !target.node}
        <p class="note lead">内核是 agent 本身：对话循环和权限判断，没有状态，可以装在好几台设备上，哪个在线都能接手。网关所在的机器上的内核由网关自己启动。工作目录在内核那台机器上就由它直接执行，不在的（比如 SSH 服务器）交给网关去执行。</p>
      {/if}
      <div class="card">
        {#each list as k (k.node)}
          {@const busy = (ov.sessions ?? []).filter((s) => s.run_node === k.node)}
          <div class="krow">
            <div class="row">
              <span class="dot online"></span>
              <strong class="grow">{k.node}</strong>
              <span class="tag">{k.managed ? '网关启动' : k.local ? '在本机' : '在别的电脑上'}</span>
              {#if k.landlock > 0}<span class="tag ok"><Icon name="shield" size={11} />Landlock v{k.landlock}</span>{:else}<span class="tag warn">没有沙箱</span>{/if}
            </div>
            <div class="muted small">已连接 {since(k.since)} · 机器 {k.local ? '本机' : k.node} · 工具 {k.tools.join('、')}</div>
            {#if busy.length}<div class="muted small">正在跑：{busy.map((s) => s.title).join('、')}</div>{/if}
          </div>
        {:else}
          <p class="note">{target.node ? '这个内核现在不在线。' : '还没有内核连上来。网关会在本机启动一个；别的电脑用安装脚本装好后连过来（见“设备”页）。'}</p>
        {/each}
      </div>
      <h5>技能 <span class="muted small">放在网关上，所有内核共用</span></h5>
      <div class="card">
        {#each ov.skills?.list ?? [] as sk (sk.name)}
          <div class="skill">
            <code>{sk.name}</code>
            <span>{sk.description}</span>
          </div>
        {:else}
          <p class="note">还没有技能。</p>
        {/each}
        <p class="note small">技能目录：<code>{homeShort(ov.skills?.dir ?? '')}</code>。每个技能一个子目录，里面是 SKILL.md 和附带的脚本、资料；改动下一轮自动生效。调用时附带文件放到每台用到的机器的 <code>~/.nano-agent/skills/</code> 下。</p>
      </div>
      <button class="btn link" onclick={() => store.open({ type: 'machine' })}><Icon name="hub" size={14} />机器和工作目录</button>
      <FieldGroup group="kernel" title="连接" focus={store.focus} />
    {:else if target.type === 'machine'}
      {#if target.name}
        {@const m = machine}
        <div class="card" class:warnc={!m?.online}>
          <div class="row">
            <span class="muted">状态</span>
            <span class="grow">{machineStatus(m)}</span>
          </div>
          {#if m?.online}
            <div class="row">
              <span class="muted">沙箱</span>
              {#if m.landlock > 0}<span class="tag ok"><Icon name="shield" size={11} />Landlock v{m.landlock}</span>{:else}<span class="tag warn">没有沙箱，自动模式下的命令也要审批</span>{/if}
            </div>
          {/if}
          {#if m?.kind === 'ssh'}
            <a class="btn sm" href="#/devices" onclick={() => (store.inspector = null)}><Icon name="server" size={13} />在“设备”里管理这台服务器</a>
          {/if}
        </div>
        <h5>这台机器上的工作目录</h5>
        <div class="card">
          {#each store.workspacesOn(target.name) as w (w.id)}
            <button class="srow" onclick={() => store.open({ type: 'workspace', id: w.id })}>
              <Icon name="folder" size={14} />
              <code class="grow">{homeShort(w.path)}</code>
              {#if w.default}<span class="tag accent">新会话默认</span>{/if}
              <span class="muted small">{w.sessions} 个会话</span>
              <Icon name="right" size={13} />
            </button>
          {:else}
            <p class="note">还没有。</p>
          {/each}
          <button class="btn sm" disabled={!m?.online} onclick={() => (browseOn = m!)} title={m?.online ? '' : '这台机器现在用不了'}><Icon name="plus" size={13} />浏览这台机器，添加工作目录</button>
        </div>
      {:else}
        <p class="note lead">工作目录可以放在三种机器上：网关所在的本机、装了内核的电脑、网关通过 SSH 连的服务器（服务器上不跑内核）。</p>
        <div class="card">
          {#each store.machines as m (m.name)}
            <button class="srow" onclick={() => store.open({ type: 'machine', name: m.name })}>
              <span class="dot {m.online ? 'online' : 'stopped'}"></span>
              <Icon name={machineMeta[m.kind].icon} size={14} />
              <span class="grow">{m.name}</span>
              <span class="tag">{m.kind === 'local' ? '网关所在的机器' : machineMeta[m.kind].label}</span>
              {#if !m.online}<span class="tag warn">用不了</span>{:else if !m.landlock}<span class="tag warn">没有沙箱</span>{/if}
              <Icon name="right" size={13} />
            </button>
          {/each}
        </div>
        <button class="btn primary" onclick={() => { store.inspector = null; store.go('#/devices'); }}><Icon name="plus" size={14} />连接电脑或 SSH 服务器</button>
      {/if}
    {:else if target.type === 'workspace'}
      {#if target.id}
        {#if ws}
          {@const m = store.machines.find((x) => x.name === ws.host)}
          <div class="card" class:warnc={!ws.online}>
            <code class="path">{ws.path}</code>
            <div class="row">
              <span class="muted">机器</span>
              <button class="lnk grow" onclick={() => store.open({ type: 'machine', name: ws.host })}>{ws.host}</button>
              <span class="dot {ws.online ? 'online' : 'stopped'}"></span>
            </div>
            <p class="note">{machineStatus(m)}</p>
            <div class="actions">
              <button class="btn sm" onclick={() => setDefault(ws)}><span class="star" class:on={ws.default}>★</span>{ws.default ? '取消新会话默认' : '设为新会话的默认'}</button>
              <button class="btn sm danger" onclick={() => removeWorkspace(ws)}><Icon name="trash" size={13} />从列表删掉</button>
            </div>
          </div>
          <h5>在这里工作的会话</h5>
          <div class="card">
            {#each wsSessions as s (s.sid)}
              <button class="srow" onclick={() => store.open({ type: 'session', sid: s.sid })}>
                <span class="dot" style="background: {channelMeta[s.channel]?.color}"></span>
                <span class="grow">{s.title}</span>
                {#if s.running}<span class="tag">由 {s.run_node} 跑</span>{/if}
                <Icon name="right" size={13} />
              </button>
            {:else}
              <p class="note">还没有会话在这里工作。</p>
            {/each}
          </div>
        {:else}
          <p class="note">这个工作目录已经从列表里删掉了。</p>
        {/if}
      {:else}
        <p class="note lead">工作目录是“机器 + 目录”，工具只能碰到它。每个会话可以挂好几个（在不同的机器上也行），标了 ★ 的新会话默认挂上。</p>
        <div class="card">
          {#each ov.workspaces ?? [] as w (w.id)}
            <button class="srow" onclick={() => store.open({ type: 'workspace', id: w.id })}>
              <span class="dot {w.online ? 'online' : 'stopped'}"></span>
              <span class="mname">{w.host}</span>
              <code class="grow">{homeShort(w.path)}</code>
              {#if w.default}<span class="star on">★</span>{/if}
              <span class="muted small">{w.sessions} 个会话</span>
              <Icon name="right" size={13} />
            </button>
          {:else}
            <p class="note">还没有工作目录，会话都只能聊天。</p>
          {/each}
          <button class="btn sm" onclick={() => (addingWs = true)}><Icon name="plus" size={13} />添加工作目录</button>
        </div>
        <FieldGroup group="workspace" focus={store.focus} />
      {/if}
    {:else if target.type === 'channel'}
      {@const c = ov.channels.find((x) => x.name === target.name)}
      {#if c}
        <div class="card">
          <div class="row">
            <span class="dot {c.status.state}"></span>
            <strong class="grow">{stateLabel[c.status.state] ?? c.status.state}</strong>
            {#if c.status.since}<span class="muted small">{ago(c.status.since)}</span>{/if}
          </div>
          {#if c.status.identity}<div class="row"><span class="muted">机器人</span><span class="grow">{c.status.identity}</span></div>{/if}
          {#if c.status.error}<div class="errbox">{c.status.error}</div>{/if}
          {#if target.name === 'web'}
            <p class="note">网页能看到所有会话，也能往任何一个会话里发消息，网页用户都是管理员。</p>
            <a class="btn primary" href="#/sessions/main" onclick={() => (store.inspector = null)}><Icon name="chat" size={14} />打开主会话</a>
          {:else if target.name === 'cli'}
            <p class="note">
              在另一个终端运行 <code>make chat</code>（或 <code>nano-gateway chat</code>）就能和网关聊天，随时进出，网关照常运行；连上时会先显示最近的对话。
              终端的身份是 <code>cli:local</code>，始终是管理员，默认接在主会话上（用 <code>/session</code> 切换）。同一个会话里别的窗口说的话和回复，终端里也会同步显示。
            </p>
          {:else}
            <p class="note">私聊机器人的每个人是一个窗口。管理员默认接在主会话上，和终端、网页共享上下文；其他人各自一个会话。管理员可以发 <code>/session</code> 切换。</p>
          {/if}
        </div>
        {#if target.name !== 'web' && target.name !== 'cli'}
          {#if people.length}
            <h5>聊过天的人 <span class="muted small">每个私聊是一个窗口</span></h5>
            <div class="people">
              {#each people as { e, s } (e.key)}
                <div class="person" class:guest={!e.admin}>
                  <div class="row">
                    <strong class="grow">{e.name || e.key}</strong>
                    {#if e.admin}<span class="tag accent">管理员</span>{:else}<span class="tag">访客</span>{/if}
                  </div>
                  <div class="where">
                    接在<button class="lnk" onclick={() => store.open({ type: 'session', sid: s.sid })}>「{s.title}」</button>
                    {#if e.who}<code>{e.who}</code>{/if}
                  </div>
                  {#if !e.admin && !e.who}
                    <p class="note">还不知道这个窗口里是谁在说话（升级前的记录）。在{channelMeta[e.channel]?.label ?? ''}里给机器人再发一条消息，这里就能设为管理员。</p>
                  {:else if !e.admin}
                    <p class="note">访客只能聊天，不能用工具，也看不到别的会话。如果这是你自己，设为管理员后这个窗口会接到主会话，和终端、网页共用上下文。</p>
                    <button class="btn sm primary" onclick={() => makeAdmin(e)}><Icon name="shield" size={13} />设为管理员并接到主会话</button>
                  {:else if e.session !== 'main'}
                    <button class="btn sm" onclick={() => moveEndpoint(e, 'main')}><Icon name="hub" size={13} />接到主会话</button>
                  {/if}
                </div>
              {/each}
            </div>
          {:else if c.status.state === 'online'}
            <p class="note pad">还没有人私聊过机器人。私聊它说一句话，这里就会出现，可以把你自己设为管理员。</p>
          {/if}
        {/if}
        {#if target.name === 'telegram' && c.status.state === 'disabled'}
          <details class="guide" open>
            <summary>怎么接入 Telegram</summary>
            <ol>
              <li>找 <a href="https://t.me/BotFather" target="_blank" rel="noopener">@BotFather</a> 发 <code>/newbot</code>，拿到 token，填到下面；</li>
              <li>国内一般要设代理，比如 <code>http://127.0.0.1:7890</code>；</li>
              <li>打开“启用”，几秒后这里会显示机器人的名字；</li>
              <li>私聊机器人就能用了。目前只支持私聊，群聊以后再做。</li>
            </ol>
          </details>
        {/if}
        {#if target.name === 'feishu'}
          <FeishuQR configured={!!store.field('feishu.app_id')?.value} />
          <details class="guide">
            <summary>或者手动接入</summary>
            <ol>
              <li>在 <a href="https://open.feishu.cn/app" target="_blank" rel="noopener">飞书开放平台</a> 创建企业自建应用，添加“机器人”能力；</li>
              <li>把 App ID 和 App Secret 填到下面，打开“启用”；</li>
              <li>在“事件与回调”里选“使用长连接接收事件”，添加 <code>im.message.receive_v1</code>、<code>im.message.recalled_v1</code>；</li>
              <li>“回调配置”里同样选长连接，添加 <code>card.action.trigger</code>（卡片上的停止、批准 / 拒绝按钮要用）；</li>
              <li>开通 <code>im:message</code>、<code>im:message:send_as_bot</code>、<code>cardkit:card:write</code> 权限，发布版本。</li>
            </ol>
          </details>
        {/if}
      {/if}
      <FieldGroup group={target.name} focus={store.focus} />
    {:else if target.type === 'session'}
      {#if sess}
        <div class="actions">
          <a class="btn primary" href="#/sessions/{encodeURIComponent(sess.sid)}" onclick={() => (store.inspector = null)}><Icon name="chat" size={14} />打开对话</a>
          {#if sess.running}<button class="btn" onclick={() => sessionAction('stop')}><Icon name="stop" size={14} />停止</button>{/if}
          <button class="btn danger" onclick={() => sessionAction('reset')}><Icon name="trash" size={14} />清空上下文</button>
        </div>
        <input class="title" value={sess.title} maxlength="40" placeholder="会话的名字" title="改名字，回车保存"
          onchange={(e) => rename(e.currentTarget.value)} onkeydown={(e) => e.key === 'Enter' && e.currentTarget.blur()} />
        <div class="grid">
          <div class="stat"><b>{sess.messages}</b><span>条消息</span></div>
          <div class="stat"><b class="acc">{pct(sess.usage.cached_tokens, sess.usage.prompt_tokens)}</b><span>缓存命中</span></div>
          <div class="stat"><b>{num(sess.usage.prompt_tokens)}</b><span>累计输入</span></div>
        </div>

        <h5>接入的窗口 <span class="muted small">在哪个窗口说话都是同一段上下文</span></h5>
        <div class="card">
          {#each sess.endpoints ?? [] as e (e.key)}
            <div class="row">
              <span class="dot" style="background: {channelMeta[e.channel]?.color ?? 'var(--muted)'}"></span>
              <span class="grow">{endpointLabel(e)}</span>
              {#if e.channel === 'web'}
                <span class="epkey">能接入任何会话</span>
              {:else if !e.admin}
                <span class="tag" title="不是管理员：只能聊天，不能用工具、不能切换会话">访客</span>
                {#if e.who}<button class="btn sm" onclick={() => makeAdmin(e)}>设为管理员</button>{/if}
              {:else}
                <code class="epkey">{e.key}</code>
              {/if}
            </div>
          {:else}
            <p class="note">还没有聊天窗口接在这里，只能在网页里聊。</p>
          {/each}
          {#if otherEndpoints.length}
            <select class="attach" onchange={(ev) => { const i = +ev.currentTarget.value; ev.currentTarget.value = ''; if (i >= 0) attach(otherEndpoints[i].e); }}>
              <option value="">把其他窗口接到这里…</option>
              {#each otherEndpoints as o, i}
                <option value={i}>{endpointLabel(o.e)}（现在在「{o.from}」）</option>
              {/each}
            </select>
          {/if}
        </div>

        <h5>工作目录 <span class="muted small">{sess.workspaces.length ? `挂了 ${sess.workspaces.length} 个${sess.kernel ? ` · 由内核 ${sess.kernel} 跑` : ''}` : '在哪些机器的哪些目录上干活'}</span></h5>
        <div class="mounts">
          {#each sess.workspaces as mt (mt.id)}
            {@const sm = store.machines.find((x) => x.name === mt.host)}
            <div class="mount" class:off={mt.how === 'down'}>
              <Icon name={machineMeta[sm?.kind ?? 'kernel'].icon} size={14} />
              <button class="mp" onclick={() => store.open({ type: 'workspace', id: mt.id })} title="{mt.host}:{mt.path}">
                <b>{mt.host}</b><code>{shortPath(homeShort(mt.path), 34)}</code>
              </button>
              <span class="how {mt.how}">{howText[mt.how]}</span>
              <button class="x" title="取下" aria-label="取下 {mt.host}:{mt.path}" onclick={() => mountWorkspaces(sess.workspaces.filter((x) => x.id !== mt.id).map((x) => x.id))}><Icon name="x" size={12} /></button>
            </div>
          {:else}
            <div class="mount empty"><Icon name="chat" size={14} /><span>只读：没有挂载工作目录，只能读文件，不能写、不能执行命令</span></div>
          {/each}
          <button class="btn sm" onclick={() => (picking = true)}><Icon name="plus" size={13} />{sess.workspaces.length ? '挂载 / 取下…' : '挂载工作目录…'}</button>
        </div>
        <h5>权限模式 {#if !sess.workspaces.length}<span class="muted small">挂了工作目录之后生效</span>{/if}</h5>
        <div class="modes" class:inactive={!sess.workspaces.length}>
          {#each Object.entries(modeMeta) as [m, meta]}
            <button class:active={sess.mode === m} onclick={() => sess.mode !== m && patchSession({ mode: m })}>
              <strong>{meta.label}</strong>
              <span>{meta.desc}</span>
            </button>
          {/each}
        </div>

        {@const levels = sess.thinking_levels ?? ov.model.levels ?? []}
        <h5>模型 <span class="muted small">{sess.model ? '这个会话单独选的' : `默认：${sess.model_label}`}</span></h5>
        <select class="modelpick" value={sess.model} onchange={(ev) => patchSession({ model: ev.currentTarget.value })}>
          <option value="">{sess.model_label ? `默认模型（${sess.model_label}）` : '默认模型（还没有配置）'}</option>
          {#each ov.models?.models ?? [] as m (m.id)}
            <option value={m.id}>{m.label}</option>
          {/each}
        </select>

        {#if levels.length}
          <h5>思考强度 <span class="muted small">{sess.thinking ? sess.thinking : '模型默认'}</span></h5>
          <div class="seg">
            <button class:active={!sess.thinking} onclick={() => patchSession({ thinking: '' })}>默认</button>
            {#each levels as l}
              <button class:active={sess.thinking === l} onclick={() => patchSession({ thinking: l })}>{l === 'none' ? '关闭' : l}</button>
            {/each}
          </div>
        {/if}

        <h5>人格</h5>
        <div class="card">
          <div class="row">
            <Icon name="persona" size={15} />
            <span class="grow">{sess.persona === sess.persona_file ? '这个会话的专属人格' : '默认人格'}</span>
            <a class="btn sm" href="#/personas/{encodeURIComponent(sess.persona === sess.persona_file ? sess.persona_file : 'default.md')}" onclick={() => (store.inspector = null)}>编辑</a>
          </div>
          {#if sess.persona !== sess.persona_file}
            <a class="btn sm ghost" href="#/personas/{encodeURIComponent(sess.persona_file)}?new=1" onclick={() => (store.inspector = null)}><Icon name="plus" size={13} />给这个会话单独设定人格</a>
          {/if}
        </div>
        {#if sess.compacted}
          <p class="note">这个会话的历史超过过上限，已经裁掉最早的 {sess.compacted} 条消息。</p>
        {/if}
        {#if sess.sid !== 'main'}
          <button class="btn link danger-text" onclick={deleteSession}><Icon name="trash" size={14} />删除这个会话</button>
        {/if}
      {:else if gone === target.sid}
        <p class="note">没有这个会话，可能已经删掉了。</p>
      {:else}
        <p class="note">正在读取…</p>
      {/if}
    {/if}
  </div>
</aside>

{#if picking && sess}
  <WorkspacePicker title="挂载工作目录" mounted={sess.workspaces.map((m) => m.id)} onchange={mountWorkspaces} onclose={() => (picking = false)} />
{/if}
{#if addingWs}
  <WorkspacePicker title="添加工作目录" onpick={(id) => { addingWs = false; store.open({ type: 'workspace', id }); }} onclose={() => (addingWs = false)} />
{/if}
{#if browseOn}
  <DirPicker host={browseOn.kind === 'local' ? '' : browseOn.name} onpick={addOn} onclose={() => (browseOn = null)} />
{/if}

<style>
  .insp {
    position: relative;
    width: 430px;
    flex: none;
    display: flex;
    flex-direction: column;
    background: var(--panel-solid);
    border-left: 1px solid var(--line-2);
    box-shadow: -10px 0 30px rgba(0, 0, 0, 0.12);
    z-index: 10;
    animation: slide 0.22s ease;
  }
  .insp.wide {
    width: 780px;
  }
  /* 窄屏：盖在内容上面 */
  @media (max-width: 1000px) {
    .insp {
      position: absolute;
      top: 0;
      right: 0;
      bottom: 0;
      width: min(430px, 100%);
      box-shadow: -18px 0 50px rgba(0, 0, 0, 0.3);
    }
    .insp.wide {
      width: min(780px, 100%);
    }
  }
  @keyframes slide {
    from {
      transform: translateX(24px);
      opacity: 0;
    }
  }
  header {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 16px 16px 14px 18px;
    border-bottom: 1px solid var(--line);
    background: linear-gradient(180deg, color-mix(in srgb, var(--c) 10%, transparent), transparent);
  }
  .disc {
    width: 40px;
    height: 40px;
    border-radius: 12px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 16%, transparent);
    border: 1px solid color-mix(in srgb, var(--c) 35%, transparent);
  }
  .ht {
    flex: 1;
    min-width: 0;
  }
  h3 {
    margin: 0;
    font-size: 15.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sub {
    font-size: 12px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .body {
    flex: 1;
    overflow: auto;
    padding: 16px 16px 40px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 8px;
    margin-bottom: 14px;
  }
  .stat {
    padding: 10px 12px;
    border-radius: 12px;
    background: var(--panel-2);
    border: 1px solid var(--line);
    display: flex;
    flex-direction: column;
  }
  .stat b {
    font-size: 18px;
    font-variant-numeric: tabular-nums;
  }
  .stat b.acc {
    color: var(--accent-2);
  }
  .stat span {
    font-size: 11.5px;
    color: var(--muted);
  }
  .kv {
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 12.5px;
  }
  .kv div {
    display: flex;
    gap: 10px;
  }
  .kv span {
    width: 64px;
    color: var(--muted);
    flex: none;
  }
  .kv code {
    word-break: break-all;
    font-size: 12px;
  }
  .card {
    padding: 13px 14px;
    border-radius: 14px;
    background: var(--panel-2);
    border: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    gap: 9px;
    margin-bottom: 4px;
  }
  .card.warnc {
    border-color: color-mix(in srgb, var(--err) 40%, var(--line));
  }
  .row {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 13px;
  }
  .row > .muted {
    width: 48px;
    flex: none;
    font-size: 12.5px;
  }
  .grow {
    flex: 1;
    min-width: 0;
    word-break: break-all;
  }
  code {
    font-size: 12.5px;
  }
  .note {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-2);
    line-height: 1.6;
  }
  .note.lead {
    margin-bottom: 4px;
  }
  .small {
    font-size: 11.5px;
  }

  .errbox {
    padding: 8px 10px;
    border-radius: 9px;
    font-size: 12.5px;
    color: var(--err);
    background: color-mix(in srgb, var(--err) 8%, transparent);
    word-break: break-word;
  }
  h5 {
    margin: 18px 0 8px 2px;
    font-size: 12.5px;
    font-weight: 600;
    color: var(--text-2);
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .btn.link {
    margin-top: 14px;
    width: 100%;
    justify-content: center;
  }
  .card .btn {
    align-self: flex-start;
  }
  .path {
    word-break: break-all;
    font-size: 13px;
  }
  .srow {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 7px 9px;
    border-radius: 9px;
    border: 1px solid var(--line);
    background: var(--panel-solid);
    text-align: left;
  }
  .srow:hover {
    border-color: var(--line-2);
  }
  .srow code,
  .srow .grow {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mname {
    flex: none;
    font-size: 12px;
    color: var(--text-2);
  }
  .star {
    color: var(--line-2);
  }
  .star.on {
    color: var(--warn);
  }
  .krow {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 9px 11px;
    border-radius: 10px;
    background: var(--panel-2);
  }
  .krow .small {
    line-height: 1.6;
  }
  /* 会话挂载的工作目录 */
  .mounts {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .mounts > .btn {
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
  .skill {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 7px 10px;
    border-radius: 9px;
    background: var(--panel-2);
    font-size: 12.5px;
  }
  .skill code {
    color: var(--c-kernel);
  }
  .skill span {
    color: var(--text-2);
    line-height: 1.5;
  }
  .guide {
    margin-top: 12px;
    padding: 12px 14px;
    border-radius: 14px;
    border: 1px dashed var(--line-2);
    font-size: 12.5px;
    color: var(--text-2);
  }
  .guide summary {
    cursor: pointer;
    color: var(--text);
    font-weight: 600;
  }
  .guide ol {
    margin: 10px 0 0;
    padding-left: 20px;
    line-height: 1.75;
  }
  .actions {
    display: flex;
    gap: 8px;
    margin-bottom: 14px;
    flex-wrap: wrap;
  }
  input.title {
    width: 100%;
    height: 34px;
    padding: 0 11px;
    margin-bottom: 14px;
    border-radius: 10px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    font-size: 14px;
    font-weight: 600;
    outline: none;
  }
  input.title:focus {
    border-color: var(--accent);
  }
  .people {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .person {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
    padding: 11px 13px;
    border-radius: 13px;
    border: 1px solid var(--line);
    background: var(--panel-2);
  }
  .person .row {
    width: 100%;
  }
  .person.guest {
    border-color: color-mix(in srgb, var(--warn) 35%, var(--line));
  }
  .where {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted);
  }
  .where code {
    font-size: 11.5px;
    word-break: break-all;
  }
  .lnk {
    border: none;
    background: none;
    padding: 0;
    color: var(--accent-2);
    cursor: pointer;
    font-size: inherit;
  }
  .note.pad {
    margin-top: 12px;
  }
  .epkey {
    flex: none;
    font-size: 11.5px;
    color: var(--muted);
  }
  .warnline {
    display: flex;
    gap: 6px;
    align-items: center;
    margin-top: 8px;
    color: var(--warn);
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
  .danger-text {
    color: var(--err);
  }
  .modes.inactive {
    opacity: 0.6;
  }
  .modes {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 7px;
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
  .seg {
    display: inline-flex;
    flex-wrap: wrap;
    padding: 3px;
    gap: 2px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
  }
  .modelpick {
    width: 100%;
    height: 34px;
    padding: 0 9px;
    border-radius: 9px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    outline: none;
  }
  .modelpick:focus {
    border-color: var(--accent);
  }
  .seg button {
    height: 28px;
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
  :global(.spin) {
    animation: spin 0.9s linear infinite;
  }
</style>
