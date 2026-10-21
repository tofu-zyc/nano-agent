<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { buildBlocks, type Block } from '../lib/conv';
  import { markdown } from '../lib/markdown';
  import { channelMeta, clock, homeShort, modeMeta, pct, shortPath } from '../lib/format';
  import type { Msg, SessionInfo } from '../lib/types';
  import ToolCard from './ToolCard.svelte';
  import Icon from './Icon.svelte';

  let { sid }: { sid: string } = $props();

  let blocks = $state<Block[]>([]);
  let summary = $state('');
  let loading = $state(true);
  let text = $state('');
  let sending = $state(false);
  let scroller: HTMLDivElement;
  let stick = true;

  const info = $derived(store.session(sid));
  const live = $derived(store.live[sid] ?? []);
  const phase = $derived(store.phase[sid]);
  const approvals = $derived(store.ov?.approvals.filter((a) => a.sid === sid) ?? []);
  const isWeb = $derived(sid.startsWith('web:'));
  const meta = $derived(channelMeta[sid.split(':')[0]]);

  async function load() {
    try {
      const r = await api.get<{ info: SessionInfo; history: Msg[]; summary: string }>(sidPath(sid));
      let hist = r.history;
      // 这一轮还在跑：它的内容由实时事件显示，历史只取到这一轮开始之前，免得重复
      if (r.info.running && (store.live[sid] ?? []).some((x) => x.kind === 'user')) {
        const lastUser = hist.map((m) => m.role).lastIndexOf('user');
        if (lastUser >= 0) hist = hist.slice(0, lastUser);
      }
      blocks = buildBlocks(hist);
      summary = r.summary;
      if (r.info.messages || store.session(sid)) store.upsertSession(r.info);
      if (!r.info.running) delete store.live[sid];
    } catch (err) {
      store.fail(err);
    } finally {
      loading = false;
      await tick();
      toBottom(true);
    }
  }

  $effect(() => {
    sid;
    loading = true;
    blocks = [];
    stick = true;
    load();
  });

  // 一轮结束：从历史里重新读完整记录，替换掉实时显示的部分
  let lastTick = 0;
  $effect(() => {
    const t = store.historyTick[sid] ?? 0;
    if (t !== lastTick) {
      lastTick = t;
      if (t) load();
    }
  });

  $effect(() => {
    live.length;
    const last = live[live.length - 1];
    if (last && 'text' in last) last.text.length;
    tick().then(() => toBottom(false));
  });

  function toBottom(force: boolean) {
    if (!scroller) return;
    if (force || stick) scroller.scrollTop = scroller.scrollHeight;
  }

  function onScroll() {
    stick = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
  }

  async function send() {
    const msg = text.trim();
    if (!msg || sending) return;
    sending = true;
    try {
      await api.post('/api/chat', { chat: sid.slice(4), text: msg });
      text = '';
      stick = true;
    } catch (err) {
      store.fail(err);
    } finally {
      sending = false;
    }
  }

  function onkey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  }

  async function act(action: 'stop' | 'reset') {
    if (action === 'reset' && !confirm('清空这个会话的上下文？')) return;
    try {
      await api.post(`${sidPath(sid)}/${action}`);
      if (action === 'reset') {
        blocks = [];
        summary = '';
      }
    } catch (err) {
      store.fail(err);
    }
  }

  const phaseText: Record<string, string> = { thinking: '正在思考', writing: '正在回复', tool: '正在执行工具' };
</script>

<div class="conv">
  <header>
    <div class="avatar" style="--c: {meta?.color ?? 'var(--c-session)'}"><Icon name={meta?.icon ?? 'chat'} size={17} /></div>
    <div class="ht">
      <h3>{info?.title ?? sid}</h3>
      <div class="chips">
        <span class="tag">{meta?.label ?? sid.split(':')[0]}</span>
        {#if info}
          <button class="tag" onclick={() => store.open({ type: 'session', sid })}>{modeMeta[info.mode]?.label ?? info.mode}模式</button>
          {#if info.dirs?.length}
            <button class="tag mono" title={info.dirs[0]} onclick={() => store.open({ type: 'session', sid })}><Icon name="folder" size={11} />{shortPath(homeShort(info.dirs[0]), 36)}{info.dirs.length > 1 ? ` +${info.dirs.length - 1}` : ''}</button>
          {:else}
            <button class="tag" onclick={() => store.open({ type: 'session', sid })}>没有工作目录</button>
          {/if}
          {#if info.thinking}<span class="tag">思考 {info.thinking}</span>{/if}
          {#if info.usage.prompt_tokens}<span class="tag accent">缓存命中 {pct(info.usage.cached_tokens, info.usage.prompt_tokens)}</span>{/if}
        {/if}
      </div>
    </div>
    {#if info?.running}
      <button class="btn sm" onclick={() => act('stop')}><Icon name="stop" size={13} />停止</button>
    {/if}
    <button class="btn sm ghost" onclick={() => act('reset')} title="清空上下文"><Icon name="trash" size={14} /></button>
    <button class="btn sm ghost" onclick={() => store.open({ type: 'session', sid })} title="会话设置"><Icon name="sliders" size={14} /></button>
  </header>

  <div class="scroll" bind:this={scroller} onscroll={onScroll}>
    <div class="inner">
      {#if summary}
        <div class="sys"><Icon name="layers" size={13} />{summary.replace(/^\[系统\] /, '').split('\n')[0]}</div>
      {/if}
      {#if !loading && !blocks.length && !live.length}
        <div class="empty">
          <Icon name="chat" size={26} />
          <p>{isWeb ? '说点什么开始对话吧。' : '这个会话还没有消息。'}</p>
          {#if isWeb && !info?.dirs?.length}
            <p class="muted">想让它读写文件、执行命令？先在 <button class="lnk" onclick={() => store.open({ type: 'session', sid })}>会话设置</button> 里给它一个工作目录。</p>
          {/if}
        </div>
      {/if}

      {#each blocks as b}
        {#if b.type === 'env'}
          <div class="sys"><Icon name="folder" size={13} />{b.text}</div>
        {:else if b.type === 'user'}
          <div class="msg user">
            <div class="meta">{b.sender ?? '用户'}{#if b.time}<span>{b.time}</span>{/if}</div>
            <div class="bubble">{b.text}</div>
          </div>
        {:else}
          <div class="msg bot">
            {#if b.reasoning}
              <details class="think">
                <summary><Icon name="brain" size={13} />思考过程 · {Array.from(b.reasoning).length} 字</summary>
                <div class="think-body">{b.reasoning}</div>
              </details>
            {/if}
            {#if b.text}
              <div class="md">{@html markdown(b.text)}</div>
            {/if}
            {#if b.tools.length}
              <div class="tools">
                {#each b.tools as t}
                  <ToolCard name={t.name} detail={t.detail} args={t.args} ok={t.ok} result={t.result} />
                {/each}
              </div>
            {/if}
            {#if b.usage && (b.text || !b.tools.length)}
              <div class="usage">
                输入 {b.usage.prompt_tokens} · 缓存 {pct(b.usage.cached_tokens, b.usage.prompt_tokens)} · 输出 {b.usage.completion_tokens}{b.usage.reasoning_tokens ? `（思考 ${b.usage.reasoning_tokens}）` : ''}
                {#if b.ts}· {clock(b.ts * 1000)}{/if}
              </div>
            {/if}
          </div>
        {/if}
      {/each}

      <!-- 正在进行的这一轮：实时内容 -->
      {#each live as it}
        {#if it.kind === 'user'}
          {@const raw = it.text}
          <div class="msg user">
            <div class="meta">{it.name ?? '用户'}<span>{clock(it.at)}</span></div>
            <div class="bubble">{raw}</div>
          </div>
        {:else if it.kind === 'reasoning'}
          <div class="msg bot">
            <details class="think" open={!live.some((x) => x.kind === 'content')}>
              <summary><Icon name="brain" size={13} />思考中 · {Array.from(it.text).length} 字</summary>
              <div class="think-body">{it.text}</div>
            </details>
          </div>
        {:else if it.kind === 'content'}
          <div class="msg bot">
            <div class="md">{@html markdown(it.text)}<span class="caret"></span></div>
          </div>
        {:else if it.kind === 'tool'}
          <div class="msg bot">
            <ToolCard name={it.name} detail={it.detail} args={it.args} ok={it.ok} result={it.result} running />
          </div>
        {:else if it.kind === 'say' && !it.text.startsWith('⚠️ 需要审批')}
          <div class="sys say"><Icon name="bot" size={13} />{it.text}</div>
        {/if}
      {/each}

      {#each approvals as ap (ap.n)}
        <div class="approval">
          <div class="at"><Icon name="shield" size={15} />需要审批 #{ap.n} · {ap.name}</div>
          <pre>{ap.detail}</pre>
          <div class="acts">
            <button class="btn sm primary" onclick={() => store.approve(ap.n, true)}><Icon name="check" size={13} />批准</button>
            <button class="btn sm" onclick={() => store.approve(ap.n, false)}>拒绝</button>
          </div>
        </div>
      {/each}

      {#if phase}
        <div class="phase"><span class="spinner"></span>{phaseText[phase] ?? '处理中'}…</div>
      {/if}
    </div>
  </div>

  {#if isWeb}
    <div class="composer">
      <textarea bind:value={text} onkeydown={onkey} rows="1" placeholder="发消息给机器人，Enter 发送，Shift+Enter 换行。/help 查看命令"></textarea>
      <button class="btn primary" onclick={send} disabled={!text.trim() || sending}><Icon name="send" size={14} />发送</button>
    </div>
  {:else}
    <div class="readonly">
      <Icon name="lock" size={13} />这是{meta?.label ?? ''}的会话，消息要在原渠道里发。这里可以查看过程、审批、修改会话设置。
    </div>
  {/if}
</div>

<style>
  .conv {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 12px 18px;
    border-bottom: 1px solid var(--line);
  }
  .avatar {
    width: 36px;
    height: 36px;
    border-radius: 11px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 16%, transparent);
  }
  .ht {
    flex: 1;
    min-width: 0;
  }
  h3 {
    margin: 0 0 3px;
    font-size: 15px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chips {
    display: flex;
    gap: 5px;
    flex-wrap: wrap;
  }
  .chips button.tag {
    cursor: pointer;
  }
  .chips button.tag:hover {
    color: var(--text);
    border-color: var(--line-2);
  }
  .chips .mono {
    font-family: var(--mono);
    font-size: 11px;
  }
  .scroll {
    flex: 1;
    overflow: auto;
  }
  .inner {
    max-width: 860px;
    margin: 0 auto;
    padding: 22px 22px 30px;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }
  .msg {
    display: flex;
    flex-direction: column;
    gap: 6px;
    animation: rise 0.25s ease;
  }
  .msg.user {
    align-items: flex-end;
  }
  .meta {
    font-size: 11.5px;
    color: var(--muted);
    display: flex;
    gap: 8px;
  }
  .bubble {
    max-width: 78%;
    padding: 9px 13px;
    border-radius: 14px 14px 4px 14px;
    background: color-mix(in srgb, var(--accent) 22%, var(--panel-2));
    border: 1px solid color-mix(in srgb, var(--accent) 30%, transparent);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .msg.bot {
    align-items: flex-start;
  }
  .md {
    max-width: 100%;
    line-height: 1.7;
    word-break: break-word;
  }
  .md :global(p) {
    margin: 0 0 8px;
  }
  .md :global(p:last-child) {
    margin-bottom: 0;
  }
  .md :global(pre) {
    position: relative;
    margin: 8px 0;
    padding: 11px 13px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    overflow: auto;
    font-size: 12.5px;
    line-height: 1.55;
  }
  .md :global(pre .lang) {
    position: absolute;
    top: 6px;
    right: 9px;
    font-size: 10.5px;
    color: var(--muted);
    font-family: var(--font);
  }
  .md :global(code) {
    font-size: 12.5px;
    padding: 1px 5px;
    border-radius: 5px;
    background: var(--panel-2);
  }
  .md :global(pre code) {
    padding: 0;
    background: none;
  }
  .md :global(ul),
  .md :global(ol) {
    margin: 4px 0 8px;
    padding-left: 22px;
  }
  .md :global(h4) {
    margin: 10px 0 6px;
    font-size: 14.5px;
  }
  .md :global(blockquote) {
    margin: 6px 0;
    padding: 4px 12px;
    border-left: 3px solid var(--line-2);
    color: var(--text-2);
  }
  .caret {
    display: inline-block;
    width: 7px;
    height: 15px;
    margin-left: 2px;
    vertical-align: -2px;
    background: var(--accent);
    animation: blink 0.9s infinite;
  }
  .think {
    max-width: 100%;
    font-size: 12.5px;
    color: var(--text-2);
  }
  .think summary {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    cursor: pointer;
    color: var(--c-model);
    font-size: 12px;
    padding: 3px 9px;
    border-radius: 8px;
    background: color-mix(in srgb, var(--c-model) 10%, transparent);
    list-style: none;
  }
  .think summary::-webkit-details-marker {
    display: none;
  }
  .think-body {
    margin-top: 6px;
    padding: 9px 12px;
    border-left: 2px solid color-mix(in srgb, var(--c-model) 50%, transparent);
    white-space: pre-wrap;
    line-height: 1.65;
    color: var(--muted);
    max-height: 280px;
    overflow: auto;
  }
  .tools {
    display: flex;
    flex-direction: column;
    gap: 6px;
    width: min(680px, 100%);
  }
  .usage {
    font-size: 11px;
    color: var(--muted);
  }
  .sys {
    align-self: center;
    display: flex;
    align-items: center;
    gap: 7px;
    max-width: 90%;
    padding: 5px 12px;
    border-radius: 9px;
    font-size: 12px;
    color: var(--text-2);
    background: var(--panel-2);
    border: 1px dashed var(--line-2);
  }
  .sys.say {
    border-style: solid;
    white-space: pre-wrap;
  }
  .approval {
    padding: 12px 14px;
    border-radius: 13px;
    border: 1px solid color-mix(in srgb, var(--warn) 50%, var(--line));
    background: color-mix(in srgb, var(--warn) 7%, var(--panel-2));
    animation: rise 0.25s ease;
  }
  .approval .at {
    display: flex;
    align-items: center;
    gap: 7px;
    font-weight: 600;
    color: var(--warn);
    font-size: 13px;
  }
  .approval pre {
    margin: 8px 0 10px;
    padding: 8px 10px;
    border-radius: 8px;
    background: var(--bg-2);
    white-space: pre-wrap;
    word-break: break-all;
    font-size: 12px;
  }
  .acts {
    display: flex;
    gap: 6px;
  }
  .phase {
    display: flex;
    align-items: center;
    gap: 9px;
    font-size: 12.5px;
    color: var(--accent-2);
  }
  .spinner {
    width: 13px;
    height: 13px;
    border-radius: 50%;
    border: 2px solid var(--accent);
    border-top-color: transparent;
    animation: spin 0.8s linear infinite;
  }
  .empty {
    margin: 12vh auto 0;
    text-align: center;
    color: var(--muted);
  }
  .empty p {
    margin: 10px 0 0;
  }
  .lnk {
    border: none;
    background: none;
    color: var(--accent-2);
    padding: 0;
    text-decoration: underline;
  }
  .composer {
    display: flex;
    gap: 10px;
    align-items: flex-end;
    max-width: 900px;
    width: 100%;
    margin: 0 auto;
    padding: 12px 22px 16px;
  }
  .composer textarea {
    flex: 1;
    min-height: 44px;
    max-height: 200px;
    padding: 11px 14px;
    border-radius: 13px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    outline: none;
    resize: none;
    field-sizing: content;
    line-height: 1.5;
  }
  .composer textarea:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 18%, transparent);
  }
  .composer .btn {
    height: 44px;
    border-radius: 13px;
  }
  .readonly {
    display: flex;
    align-items: center;
    gap: 8px;
    justify-content: center;
    padding: 11px;
    font-size: 12.5px;
    color: var(--muted);
    border-top: 1px solid var(--line);
  }
</style>
