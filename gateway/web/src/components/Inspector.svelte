<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { ago, channelMeta, homeShort, modeMeta, num, pct, since, stateLabel } from '../lib/format';
  import type { SessionInfo, Usage } from '../lib/types';
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
      case 'kernel':
        return { icon: 'shield', color: 'var(--c-kernel)', title: ov.kernel ? `内核 · ${ov.kernel.node}` : '内核', sub: ov.kernel ? `已连接 ${since(ov.kernel.since)}` : '没有连接' };
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
      {#if ov.kernel}
        <div class="card">
          <div class="row">
            <span class="muted">沙箱</span>
            {#if ov.kernel.landlock > 0}
              <span class="tag ok"><Icon name="shield" size={11} />Landlock v{ov.kernel.landlock}</span>
            {:else}
              <span class="tag warn">没有沙箱</span>
            {/if}
          </div>
          <p class="note">
            {#if ov.kernel.landlock > 0}
              每次调用工具都在沙箱里执行：工作目录可读写，系统目录只读，其他地方（包括 HOME）访问不了；不能连 unix socket、不能给沙箱外的进程发信号。
            {:else}
              这台机器不支持 Landlock，文件工具仍然只能访问工作目录，但命令没有沙箱，自动模式下也会每次要求审批。
            {/if}
          </p>
        </div>
        <h5>内置工具</h5>
        <div class="tools">
          {#each ov.kernel.tools as name}
            <div class="tool"><code>{name}</code><span>{toolDesc[name] ?? ''}</span></div>
          {/each}
        </div>
      {:else}
        <div class="card warnc">
          <p class="note">内核还没连上来。在内核所在的机器上运行：</p>
          <pre>./kernel/nano-kernel -c kernel.conf</pre>
          <p class="note">两边配置里的 token 要一致。连上之后这里会自动刷新。</p>
        </div>
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
            <p class="note">在网页里直接和机器人聊天，网页用户都是管理员。</p>
            <a class="btn primary" href="#/sessions/{encodeURIComponent('web:main')}" onclick={() => (store.inspector = null)}><Icon name="chat" size={14} />开始聊天</a>
          {:else if target.name === 'cli'}
            <p class="note">启动网关的终端就是一个聊天窗口，身份是 <code>cli:local</code>，始终是管理员。</p>
          {/if}
        </div>
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
        <div class="grid">
          <div class="stat"><b>{sess.messages}</b><span>条消息</span></div>
          <div class="stat"><b class="acc">{pct(sess.usage.cached_tokens, sess.usage.prompt_tokens)}</b><span>缓存命中</span></div>
          <div class="stat"><b>{num(sess.usage.prompt_tokens)}</b><span>累计输入</span></div>
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

        <h5>工作目录 <span class="muted small">第一个是默认目录，命令从这里开始执行</span></h5>
        <DirInput values={sess.dirs ?? []} onchange={(v) => patchSession({ dirs: v })} />
        <h5>只读目录</h5>
        <DirInput values={sess.ro ?? []} first={false} placeholder="只能读不能写的目录" onchange={(v) => patchSession({ ro: v })} />

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
      {:else}
        <p class="note">会话不存在了。</p>
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
  pre {
    margin: 0;
    padding: 9px 11px;
    border-radius: 9px;
    background: var(--bg-2);
    font-size: 12px;
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
