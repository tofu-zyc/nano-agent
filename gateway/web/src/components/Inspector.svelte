<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { ago, channelMeta, endpointLabel, homeShort, modeMeta, num, pct, since, stateLabel } from '../lib/format';
  import type { EndpointInfo, SessionInfo, Usage } from '../lib/types';
  import FieldGroup from './FieldGroup.svelte';
  import DirInput from './DirInput.svelte';
  import FeishuQR from './FeishuQR.svelte';
  import Icon from './Icon.svelte';

  const target = $derived(store.inspector!);
  const ov = $derived(store.ov!);

  const head = $derived.by(() => {
    switch (target.type) {
      case 'gateway':
        return { icon: 'hub', color: 'var(--c-gateway)', title: '网关', sub: `已运行 ${since(ov.gateway.started)}` };
      case 'model':
        return { icon: 'sparkles', color: 'var(--c-model)', title: ov.model.model || '模型', sub: ov.model.base_url || '还没配置接口' };
      case 'kernel': {
        if (!target.node) return { icon: 'shield', color: 'var(--c-kernel)', title: '内核', sub: ov.kernels.length ? `${ov.kernels.length} 个在线` : '没有连接' };
        const k = ov.kernels.find((x) => x.node === target.node);
        return { icon: 'shield', color: k ? 'var(--c-kernel)' : 'var(--muted)', title: `内核 · ${target.node}`, sub: k ? `已连接 ${since(k.since)}` : '不在线' };
      }
      case 'workspace':
        return { icon: 'folder', color: 'var(--c-dir)', title: '工作目录与沙箱', sub: '工具能碰到的范围' };
      case 'dir':
        return { icon: 'folder', color: 'var(--c-dir)', title: homeShort(target.path), sub: '工作目录' };
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

  // ---- 模型测试 ----
  let testing = $state(false);
  let testResult = $state<{ ok: boolean; reply?: string; error?: string; ms?: number; first_ms?: number; usage?: Usage } | null>(null);
  async function testModel() {
    testing = true;
    testResult = null;
    try {
      testResult = await api.post('/api/test/model');
    } catch (err) {
      testResult = { ok: false, error: err instanceof Error ? err.message : String(err) };
    } finally {
      testing = false;
    }
  }

  // ---- 会话 ----
  const sess = $derived(target.type === 'session' ? store.session(target.sid) : undefined);
  // 列表里还没有这个会话（比如刚新建）：去网关问一下，确实没有才说不存在
  let gone = $state<string | null>(null);
  $effect(() => {
    if (target.type !== 'session' || sess) return;
    const sid = target.sid;
    store.refreshSession(sid).then((ok) => {
      if (!ok) gone = sid;
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
  // 换机器：原来的工作目录是旧机器上的路径，网关会清掉
  async function setNode(el: HTMLSelectElement) {
    if (!sess) return;
    const v = el.value;
    const to = v || ov.workspace.node || '默认的机器';
    if (sess.dirs?.length && !confirm(`换到 ${to} 上工作？原来的工作目录是 ${sess.node || '原来那台机器'} 上的路径，会清掉，需要重新选 ${to} 上的目录。`)) {
      el.value = sess.node_set ? sess.node : '';
      return;
    }
    await patchSession({ node: v });
  }
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

  const dirSessions = $derived(
    target.type === 'dir'
      ? (ov.sessions ?? []).filter((s) => (s.dirs ?? []).includes(target.path) || (s.ro ?? []).includes(target.path))
      : [],
  );
</script>

<aside class="insp" style="--c: {head.color}">
  <header>
    <div class="disc"><Icon name={head.icon} size={20} /></div>
    <div class="ht">
      <h3>{head.title}</h3>
      <div class="sub">{head.sub}</div>
    </div>
    <button class="btn ghost sm" onclick={() => (store.inspector = null)} aria-label="关闭"><Icon name="x" size={15} /></button>
  </header>

  <div class="body">
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
      <div class="card">
        <div class="row">
          <span class="muted">接口</span>
          <code class="grow">{ov.model.base_url || '—'}</code>
        </div>
        <div class="row">
          <span class="muted">思考</span>
          <span class="grow">{ov.model.thinking === 'none' ? '关闭' : ov.model.thinking || '模型默认'} · 作为 reasoning_effort 传给模型</span>
        </div>
        <div class="row">
          <span class="muted">上下文</span>
          <span class="grow">窗口 {num(ov.model.context_window)} · 历史上限 {num(ov.model.max_history)}</span>
        </div>
        <button class="btn primary" onclick={testModel} disabled={testing}>
          <Icon name={testing ? 'refresh' : 'zap'} size={14} class={testing ? 'spin' : ''} />
          {testing ? '正在测试…' : '测试连接'}
        </button>
        {#if testResult}
          <div class="result" class:bad={!testResult.ok}>
            {#if testResult.ok}
              <div class="rt"><Icon name="check" size={14} /> 通了 · 首字 {testResult.first_ms} ms · 总共 {testResult.ms} ms</div>
              <p>{testResult.reply}</p>
              {#if testResult.usage}
                <div class="muted small">输入 {testResult.usage.prompt_tokens} · 缓存 {testResult.usage.cached_tokens ?? 0} · 输出 {testResult.usage.completion_tokens}</div>
              {/if}
            {:else}
              <div class="rt"><Icon name="alert" size={14} /> 没通</div>
              <p>{testResult.error}</p>
            {/if}
          </div>
        {/if}
      </div>
      <FieldGroup group="model" focus={store.focus} />
    {:else if target.type === 'kernel'}
      {#if target.node}
        {@const m = store.machines.find((x) => x.name === target.node)}
        {#if m?.server}
          <div class="card" class:warnc={!m.online}>
            <div class="row"><span class="muted">SSH</span><code class="grow">{m.server.target}</code></div>
            <div class="row"><span class="muted">经由</span><button class="lnk grow" onclick={() => store.open({ type: 'kernel', node: m.server!.via })}>{m.server.via} 的内核</button></div>
            {#if m.online}
              <div class="row">
                <span class="muted">沙箱</span>
                {#if m.server.landlock > 0}<span class="tag ok"><Icon name="shield" size={11} />Landlock v{m.server.landlock}</span>{:else}<span class="tag warn">没有沙箱</span>{/if}
              </div>
            {:else}
              <div class="errbox">{m.server.detail || '用不了'}</div>
            {/if}
            <p class="note">
              服务器上不跑内核：循环在 {m.server.via} 上，每次调用工具时通过复用的 SSH 连接在服务器上临时运行一次执行器，工作目录是服务器上的路径。
              {#if m.server.landlock === 0 && m.online}服务器不支持 Landlock，自动模式下的 Shell 也会每次要求审批。{/if}
            </p>
          </div>
        {:else if m?.info}
          <div class="card">
            <div class="row">
              <span class="muted">沙箱</span>
              {#if m.info.landlock > 0}
                <span class="tag ok"><Icon name="shield" size={11} />Landlock v{m.info.landlock}</span>
              {:else}
                <span class="tag warn">没有沙箱</span>
              {/if}
              {#if m.info.local}<span class="tag">和网关同一台机器</span>{/if}
              {#if m.isDefault}<span class="tag accent">默认机器</span>{/if}
            </div>
            <p class="note">
              {#if m.info.landlock > 0}
                每次调用工具都在沙箱里执行：工作目录可读写，系统目录只读，其他地方（包括 HOME）访问不了；不能连 unix socket、不能给沙箱外的进程发信号。
              {:else}
                这台机器不支持 Landlock，文件工具仍然只能访问工作目录，但命令没有沙箱，自动模式下也会每次要求审批。
              {/if}
            </p>
          </div>
          <h5>内置工具</h5>
          <div class="tools">
            {#each m.info.tools as name}
              <div class="tool"><code>{name}</code><span>{toolDesc[name] ?? ''}</span></div>
            {/each}
          </div>
          {#if m.info.skills?.length}
            <h5>技能</h5>
            <div class="tools">
              {#each m.info.skills as skill}
                <div class="tool skill"><code>{skill.name}</code><span>{skill.description}</span></div>
              {/each}
            </div>
          {/if}
        {:else}
          <div class="card warnc">
            <p class="note">{target.node} 现在不在线。它上线之前，在它上面工作的会话由别的内核顶上，只能聊天；它连回来之后自动恢复。</p>
          </div>
        {/if}
        {@const users = (ov.sessions ?? []).filter((s) => s.node === target.node)}
        {#if users.length}
          <h5>在这台机器上工作的会话</h5>
          <div class="card">
            {#each users as s (s.sid)}
              <button class="srow" onclick={() => store.open({ type: 'session', sid: s.sid })}>
                <span class="grow">{s.title}</span>
                {#if s.run_node && s.run_node !== s.node}<span class="tag warn">由 {s.run_node} 顶着</span>{/if}
                <Icon name="right" size={13} />
              </button>
            {/each}
          </div>
        {/if}
        <button class="btn link" onclick={() => store.open({ type: 'kernel' })}><Icon name="hub" size={14} />所有内核</button>
      {:else}
        <p class="note lead">每个内核是一个节点：一台能干活的机器。内核没有状态，上下文都在网关，哪个内核在线都能接手；但文件在具体的机器上，会话的机器不在线时，别的内核顶上只能聊天。</p>
        <div class="card">
          {#each store.machines as m (m.name)}
            <button class="srow" onclick={() => store.open({ type: 'kernel', node: m.name })}>
              <span class="dot {m.online ? 'online' : 'stopped'}"></span>
              <span class="grow">{m.name}</span>
              {#if m.isDefault}<span class="tag accent">默认</span>{/if}
              {#if m.info?.local}<span class="tag">本机</span>{/if}
              {#if m.server}<span class="tag">服务器 · 经由 {m.server.via}</span>{/if}
              {#if !m.online}<span class="tag">离线</span>{:else if !m.info?.landlock}<span class="tag warn">没有沙箱</span>{/if}
              <Icon name="right" size={13} />
            </button>
          {:else}
            <p class="note">还没有内核连上来。在内核所在的机器上运行 <code>./kernel/nano-kernel -c kernel.conf</code>，两边配置里的 token 要一致。</p>
          {/each}
        </div>
        <details class="guide">
          <summary>在另一台机器上加一个内核</summary>
          <ol>
            <li>在那台机器上编译（<code>make kernel</code>），或者拷贝 <code>nano-kernel</code> 和 <code>nano-exec</code> 过去；</li>
            <li>写 <code>kernel.conf</code>：<code>gateway = tcp:网关地址:端口</code>，<code>token</code> 和网关一致，<code>node = 起个名字</code>；</li>
            <li>网关的“内核监听地址”要是那台机器能连到的地址，比如 <code>tcp:0.0.0.0:7700</code>。内核和网关之间的连接没有加密，跨网络请走 Tailscale、WireGuard 或者 SSH 隧道；</li>
            <li>运行 <code>./nano-kernel -c kernel.conf</code>，连上之后这里会多一个节点。在会话设置里选它，会话就在那台机器上工作。</li>
          </ol>
        </details>
        <details class="guide">
          <summary>通过 SSH 在服务器上干活（服务器上不用跑内核）</summary>
          <ol>
            <li>确保运行内核的那台机器能免密 <code>ssh</code> 到服务器（密钥登录，<code>~/.ssh/config</code> 里的别名也可以）；</li>
            <li>在那台机器的 <code>kernel.conf</code> 里加一行：<code>remote = prod deploy@prod.example.com</code>（名字、ssh 目标）；</li>
            <li>重启内核。它会自动把执行器传到服务器的 <code>~/.cache/nano-agent/</code>（架构不同、glibc 太老时，服务器有 <code>cc</code> 就现编），准备好之后这里会多一个服务器节点；</li>
            <li>在会话设置里选它，工作目录填服务器上的路径。工具同样关在服务器上的沙箱里（服务器支持 Landlock 的话）。</li>
          </ol>
        </details>
      {/if}
      <button class="btn link" onclick={() => store.open({ type: 'workspace' })}><Icon name="folder" size={14} />工作目录与沙箱的默认设置</button>
      <FieldGroup group="kernel" title="连接" focus={store.focus} />
    {:else if target.type === 'workspace'}
      <p class="note lead">工作目录是工具能碰到的范围，目录外的读写和命令一律不允许。这里是新会话的默认值，每个会话可以在它自己的面板里单独改。</p>
      <FieldGroup group="workspace" focus={store.focus} />
    {:else if target.type === 'dir'}
      <div class="card">
        <code class="path">{target.path}</code>
        <p class="note">
          {#if (ov.workspace.dirs ?? []).includes(target.path)}这是默认工作目录之一。{/if}
          {dirSessions.length ? `${dirSessions.length} 个会话在用它：` : '没有会话单独设置它。'}
        </p>
        {#each dirSessions as s}
          <button class="srow" onclick={() => store.open({ type: 'session', sid: s.sid })}>
            <span class="dot" style="background: {channelMeta[s.channel]?.color}"></span>
            <span class="grow">{s.title}</span>
            {#if (s.ro ?? []).includes(target.path)}<span class="tag">只读</span>{/if}
            <Icon name="right" size={13} />
          </button>
        {/each}
      </div>
      <button class="btn link" onclick={() => store.open({ type: 'workspace' })}><Icon name="sliders" size={14} />默认工作目录设置</button>
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
              <li>在“事件与回调”里选“使用长连接接收事件”，添加 <code>im.message.receive_v1</code>；</li>
              <li>开通 <code>im:message</code>、<code>im:message:send_as_bot</code> 权限，发布版本。</li>
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

        {#if !sess.dirs?.length}
          <div class="chatonly">
            <Icon name="chat" size={16} />
            <div>
              <strong>只聊天</strong>
              <p>还没有工作目录，这个会话不能读写文件、执行命令。在下面加一个工作目录就能用工具了。</p>
            </div>
          </div>
        {/if}
        <h5>权限模式 {#if !sess.dirs?.length}<span class="muted small">有工作目录之后生效</span>{/if}</h5>
        <div class="modes" class:inactive={!sess.dirs?.length}>
          {#each Object.entries(modeMeta) as [m, meta]}
            <button class:active={sess.mode === m} onclick={() => sess.mode !== m && patchSession({ mode: m })}>
              <strong>{meta.label}</strong>
              <span>{meta.desc}</span>
            </button>
          {/each}
        </div>

        <h5>机器 <span class="muted small">工作目录是这台机器上的路径</span></h5>
        <select class="machine" value={sess.node_set ? sess.node : ''} onchange={(e) => setNode(e.currentTarget)}>
          <option value="">默认（{ov.workspace.node || '和网关同一台机器'}）</option>
          {#each store.machines as m (m.name)}
            <option value={m.name}>{m.name}{m.online ? '' : '（离线）'}</option>
          {/each}
        </select>
        {#if sess.node && !sess.node_online}
          <p class="note warnline"><Icon name="alert" size={13} />{sess.node} 现在不在线，这个会话由别的内核顶上，只能聊天。</p>
        {/if}
        <h5>工作目录 <span class="muted small">第一个是默认目录，命令从这里开始执行</span></h5>
        <DirInput node={sess.node} values={sess.dirs ?? []} onchange={(v) => patchSession({ dirs: v })} />
        <h5>只读目录</h5>
        <DirInput node={sess.node} values={sess.ro ?? []} first={false} placeholder="只能读不能写的目录" onchange={(v) => patchSession({ ro: v })} />

        {#if ov.model.levels?.length}
          <h5>思考强度</h5>
          <div class="seg">
            <button class:active={!sess.thinking || sess.thinking === ov.model.thinking} onclick={() => patchSession({ thinking: '' })}>默认</button>
            {#each ov.model.levels as l}
              <button class:active={sess.thinking === l && l !== ov.model.thinking} onclick={() => patchSession({ thinking: l })}>{l === 'none' ? '关闭' : l}</button>
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
  .result {
    padding: 10px 12px;
    border-radius: 10px;
    background: color-mix(in srgb, var(--ok) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--ok) 30%, transparent);
    animation: rise 0.2s ease;
  }
  .result.bad {
    background: color-mix(in srgb, var(--err) 8%, transparent);
    border-color: color-mix(in srgb, var(--err) 30%, transparent);
  }
  .rt {
    display: flex;
    align-items: center;
    gap: 6px;
    font-weight: 600;
    font-size: 13px;
    color: var(--ok);
  }
  .result.bad .rt {
    color: var(--err);
  }
  .result p {
    margin: 6px 0;
    font-size: 13px;
    word-break: break-word;
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
  .tools {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .tool {
    display: flex;
    gap: 10px;
    align-items: center;
    padding: 7px 10px;
    border-radius: 9px;
    background: var(--panel-2);
    font-size: 12.5px;
  }
  .tool code {
    width: 46px;
    color: var(--c-kernel);
  }
  .tool span {
    color: var(--text-2);
  }
  .tool.skill {
    flex-direction: column;
    align-items: flex-start;
    gap: 4px;
  }
  .tool.skill code {
    width: auto;
    overflow-wrap: anywhere;
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
  select.machine {
    width: 100%;
    height: 34px;
    padding: 0 10px;
    border-radius: 10px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    font-size: 13px;
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
  .chatonly {
    display: flex;
    gap: 10px;
    padding: 11px 13px;
    border-radius: 12px;
    color: var(--c-session);
    background: color-mix(in srgb, var(--c-session) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--c-session) 30%, transparent);
  }
  .chatonly strong {
    color: var(--text);
    font-size: 13px;
  }
  .chatonly p {
    margin: 2px 0 0;
    font-size: 12.5px;
    color: var(--text-2);
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
