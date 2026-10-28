<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api, ApiError, sidPath, uploadImage } from '../lib/api';
  import { buildBlocks, groupEpisodes, type Block } from '../lib/conv';
  import { markdown } from '../lib/markdown';
  import { channelMeta, clock, endpointLabel, homeShort, modeMeta, pct, shortPath, viaChannel } from '../lib/format';
  import type { Attachment, CommandInfo, Msg, SessionInfo } from '../lib/types';
  import ToolCard from './ToolCard.svelte';
  import WorkspacePicker from './WorkspacePicker.svelte';
  import Icon from './Icon.svelte';
  import HistoryManager from './HistoryManager.svelte';
  import AgentSwitcher from './AgentSwitcher.svelte';

  let { sid }: { sid: string } = $props();
  const isRoom = $derived(sid.startsWith('room:'));
  const room = $derived(isRoom ? store.room(sid.slice(5)) : undefined);
  const roomMembers = $derived((room?.members ?? []).map((id) => store.agent(id)).filter((a) => !!a));
  const roomSpeaker = (id?: string, name?: string) => store.agent(id) ?? roomMembers.find((a) => a.name === name);
  async function toggleRoom() {
    if (!room) return;
    try { await api.patch('/api/rooms/' + encodeURIComponent(room.id), { paused: !room.paused }); await store.refresh(); }
    catch (err) { store.fail(err); }
  }
  // 拟人型的内心活动、整理记忆：agent 自己的会话，只能看
  const innerKind = $derived(/^@[^/]+\/life$/.test(sid) ? 'life' : /^@[^/]+\/dream$/.test(sid) ? 'dream' : '');
  let expandedEpisodes = $state<Set<number>>(new Set());
  function toggleEpisode(start: number) {
    const next = new Set(expandedEpisodes);
    if (next.has(start)) next.delete(start); else next.add(start);
    expandedEpisodes = next;
  }
  const innerAgent = $derived(store.agent(sid.replace(/^@([^/]+)\/.*$/, '$1')));
  const noteTitle: Record<string, string> = { memory: '带上了长期记忆', recall: '想起来的几条记忆', life: '最近的经历（已结束）' };
  async function wakeInner() {
    try {
      await api.post('/api/agents/' + encodeURIComponent(innerAgent?.id ?? '') + '/wake', {});
      store.toast({ kind: 'ok', text: '叫醒了，它在想事情' }, 1800);
    } catch (err) {
      store.fail(err);
    }
  }

  let blocks = $state<Block[]>([]);
  const episodeGroups = $derived(groupEpisodes(blocks));
  let summary = $state('');
  let loading = $state(true);
  let missing = $state(false);
  let text = $state('');
  let sending = $state(false);
  let scroller = $state<HTMLDivElement>();
  let stick = true;

  // 每个会话各自保留没发出去的草稿：切换会话不会把文字和图片带过去
  const drafts = new Map<string, { text: string; pics: Attachment[] }>();
  let activeSid = untrack(() => sid);
  let uploadGen = 0;
  let loadSeq = 0;

  // ---- 图片：选好（或粘贴、拖进来）先上传，发送时只带 ID ----
  const maxPics = 4;
  const noModel = $derived((store.ov?.models?.models?.length ?? 0) === 0);
  let pics = $state<Attachment[]>([]);
  let uploading = $state(0);
  let dragging = $state(false);
  let fileInput = $state<HTMLInputElement>();

  async function addFiles(files: FileList | File[]) {
    if (isRoom) return;
    const gen = uploadGen;
    for (const f of Array.from(files)) {
      if (!f.type.startsWith('image/')) continue;
      if (pics.length + uploading >= maxPics) {
        store.toast({ kind: 'err', text: `一条消息最多 ${maxPics} 张图片` }, 2400);
        break;
      }
      uploading++;
      try {
        const pic = await uploadImage(f);
        if (gen === uploadGen) pics = [...pics, pic]; // 上传期间切了会话：不要接到新会话里
      } catch (err) {
        store.fail(err);
      } finally {
        if (gen === uploadGen) uploading--;
      }
    }
  }

  function onPaste(e: ClipboardEvent) {
    const files = Array.from(e.clipboardData?.files ?? []).filter((f) => f.type.startsWith('image/'));
    if (!files.length) return;
    e.preventDefault();
    addFiles(files);
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragging = false;
    if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
  }

  function pickFiles(e: Event) {
    const el = e.currentTarget as HTMLInputElement;
    if (el.files?.length) addFiles(el.files);
    el.value = '';
  }

  function removePic(id: string) {
    pics = pics.filter((p) => p.id !== id);
  }

  const info = $derived(store.session(sid));
  const live = $derived(store.live[sid] ?? []);
  const phase = $derived(store.phase[sid]);
  const approvals = $derived(store.ov?.approvals.filter((a) => a.sid === sid) ?? []);
  const isMain = $derived(sid === 'main');
  const meta = $derived(isMain ? { label: '主会话', color: 'var(--accent)', icon: 'hub' } : channelMeta[info?.channel ?? 'web']);
  const eps = $derived(info?.endpoints ?? []);

  async function load() {
    const gen = ++loadSeq;
    const want = sid;
    try {
      const r = await api.get<{ info: SessionInfo; history: Msg[]; summary: string }>(sidPath(want));
      if (gen !== loadSeq) return; // 已经切到别的会话，旧结果不要写进来
      let hist = r.history;
      // 这一轮还在跑：它的内容由实时事件显示，历史只取到这一轮开始之前，免得重复
      if (r.info.running && (store.live[want] ?? []).some((x) => x.kind === 'user')) {
        const lastUser = hist.map((m) => m.role).lastIndexOf('user');
        if (lastUser >= 0) hist = hist.slice(0, lastUser);
      }
      blocks = buildBlocks(hist, !!innerKind, isRoom);
      summary = r.summary;
      missing = false;
      store.upsertSession(r.info);
      if (!r.info.running) delete store.live[want];
    } catch (err) {
      if (gen !== loadSeq) return;
      if (err instanceof ApiError && err.status === 404) missing = true;
      else store.fail(err);
    } finally {
      if (gen === loadSeq) {
        loading = false;
        await tick();
        toBottom(true);
      }
    }
  }

  // 切换会话：先把当前草稿存回上一个会话，再恢复这个会话的草稿
  $effect(() => {
    const next = sid;
    if (next === activeSid) return;
    drafts.set(activeSid, untrack(() => ({ text, pics })));
    const d = drafts.get(next);
    text = d?.text ?? '';
    pics = d?.pics ?? [];
    uploading = 0;
    sending = false;
    uploadGen++;
    activeSid = next;
  });

  $effect(() => {
    sid;
    loading = true;
    blocks = [];
    missing = false;
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
    if (scroller) stick = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
  }

  async function send() {
    const msg = text.trim();
    if (noModel) {
      store.toast({ kind: 'err', text: '还没有配置模型，先去模型节点添加接口和模型' }, 2600);
      return;
    }
    if ((!msg && !pics.length) || sending || uploading > 0) return;
    const forSid = sid;
    sending = true;
    try {
      await api.post('/api/chat', { sid: forSid, text: msg, images: pics.map((p) => ({ id: p.id, name: p.name })) });
      if (forSid !== sid) return; // 发送期间切了会话：不要清掉新会话的草稿
      text = '';
      pics = [];
      stick = true;
    } catch (err) {
      store.fail(err);
    } finally {
      sending = false;
    }
  }

  // ---- 命令提示：输入框里打 / 时列出命令，上下键选，Tab 补全，Enter 补全（不用参数的直接发送），Esc 收起 ----
  let ta = $state<HTMLTextAreaElement>();
  let cursor = $state(0);
  let mentionIndex = $state(0);
  let mentionHidden = $state(false);
  const mentionWord = $derived(isRoom && !mentionHidden ? text.slice(0, cursor).match(/(?:^|\s)@([^\s@]*)$/)?.[1] : undefined);
  const mentionMatches = $derived(mentionWord === undefined ? [] : roomMembers.filter((a) =>
    a.name.toLowerCase().includes(mentionWord.toLowerCase()) || a.id.toLowerCase().includes(mentionWord.toLowerCase())));
  function trackCursor() { cursor = ta?.selectionStart ?? text.length; mentionHidden = false; mentionIndex = 0; }
  async function mention(name: string, replace = false) {
    const at = replace ? text.lastIndexOf('@', cursor) : cursor;
    const end = replace ? cursor : at;
    const prefix = text.slice(0, at);
    const added = (prefix && !/\s$/.test(prefix) ? ' ' : '') + '@' + name + ' ';
    text = prefix + added + text.slice(end);
    cursor = prefix.length + added.length;
    mentionHidden = true;
    await tick();
    ta?.focus(); ta?.setSelectionRange(cursor, cursor);
  }
  let menuIdx = $state(0);
  let menuHidden = $state(false);
  const cmdWord = $derived(text.startsWith('/') ? text.slice(1).split(/\s/)[0].toLowerCase() : null);
  const cmdMatches = $derived(
    cmdWord !== null && !/\s/.test(text) && !menuHidden ? (store.ov?.commands ?? []).filter((c) => c.name.startsWith(cmdWord)) : [],
  );
  // 已经打完命令、开始写参数时，提示参数的写法
  const cmdHint = $derived(cmdWord !== null && /\s/.test(text) ? store.ov?.commands?.find((c) => c.name === cmdWord) : undefined);
  $effect(() => {
    cmdWord;
    menuIdx = 0;
    if (!text.startsWith('/')) menuHidden = false;
  });
  function pickCmd(c: CommandInfo, sendNow: boolean) {
    const optional = !c.args || c.args.startsWith('[');
    if (sendNow && optional) {
      text = '/' + c.name;
      send();
      return;
    }
    text = '/' + c.name + ' ';
    ta?.focus();
  }

  function onkey(e: KeyboardEvent) {
    if (mentionMatches.length && !e.isComposing) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault(); mentionIndex = (mentionIndex + (e.key === 'ArrowDown' ? 1 : mentionMatches.length - 1)) % mentionMatches.length; return;
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault(); mention(mentionMatches[mentionIndex]?.name ?? mentionMatches[0].name, true); return;
      }
      if (e.key === 'Escape') { e.preventDefault(); mentionHidden = true; return; }
    }
    if (cmdMatches.length && !e.isComposing) {
      const n = cmdMatches.length;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        menuIdx = (menuIdx + (e.key === 'ArrowDown' ? 1 : n - 1)) % n;
        return;
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault();
        pickCmd(cmdMatches[menuIdx], e.key === 'Enter');
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        menuHidden = true;
        return;
      }
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  }

  // 撤回最近一条：排队的直接拿掉，正在处理的先停下，处理完的连同回复从上下文里删掉。撤回的话放回输入框
  const lastLive = $derived(lastIndex(live, (x) => x.kind === 'user'));
  const lastBlock = $derived(lastLive >= 0 ? -1 : lastIndex(blocks, (b) => b.type === 'user'));
  function lastIndex<T>(list: T[], ok: (x: T) => boolean): number {
    for (let i = list.length - 1; i >= 0; i--) if (ok(list[i])) return i;
    return -1;
  }
  // 挂载、取下工作目录：直接在对话头部操作，不用先打开会话设置
  let picking = $state(false);
  $effect(() => {
    sid;
    picking = false;
  });
  async function mountWorkspaces(ids: string[]) {
    try {
      store.upsertSession(await api.patch<SessionInfo>(sidPath(sid), { workspaces: ids }));
    } catch (err) {
      store.fail(err);
    }
  }
  // 点头部的工作目录标签就是取下它（以前只打开选择框，和“点击取下”的提示不符）
  async function unmountWorkspace(id: string) {
    const mt = info?.workspaces.find((m) => m.id === id);
    try {
      store.upsertSession(await api.patch<SessionInfo>(sidPath(sid), { workspaces: (info?.workspaces ?? []).filter((m) => m.id !== id).map((m) => m.id) }));
      store.toast({ kind: 'ok', text: mt ? `已取下 ${mt.host}:${shortPath(homeShort(mt.path), 30)}` : '已取下' }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }

  let undoing = $state(false);
  async function undo() {
    if (undoing) return;
    undoing = true;
    try {
      const r = await api.post<{ text: string }>(`${sidPath(sid)}/undo`);
      if (r.text && !text.trim()) text = r.text;
      store.toast({ kind: 'ok', text: info?.running ? '正在停下，这一轮结束后从上下文里删掉' : '已撤回，内容放回输入框了' });
    } catch (err) {
      store.fail(err);
    } finally {
      undoing = false;
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

{#if missing}
  <div class="conv gone">
    <Icon name="chat" size={26} />
    <p>没有这个会话，可能已经删掉了。</p>
    <a class="btn primary" href="#/sessions/main"><Icon name="hub" size={14} />回到主会话</a>
  </div>
{:else}
<div class="conv" class:room-chat={isRoom}>
  <header>
    <div class="avatar" style="--c: {meta?.color ?? 'var(--c-session)'}"><Icon name={meta?.icon ?? 'chat'} size={17} /></div>
    <div class="ht">
      <h3>{info?.title ?? sid}</h3>
      <div class="chips">
        {#each eps as e (e.key)}
          <span class="tag ep" style="--c: {channelMeta[e.channel]?.color ?? 'var(--muted)'}" title="这个窗口接在这个会话上：{e.key}"><Icon name={channelMeta[e.channel]?.icon ?? 'link'} size={11} />{endpointLabel(e)}</span>
        {/each}
        {#if info}
          {#if isRoom}
            <span class="tag">{room?.members.length ?? 0} 位 agent · {room?.mode === 'free' ? '自由讨论' : '点名发言'}</span>
          {:else if info.workspaces.length}
            <button class="tag" onclick={() => store.open({ type: 'session', sid })}>{modeMeta[info.mode]?.label ?? info.mode}模式</button>
            {#each info.workspaces as mt, i (`${mt.id}:${i}`)}
              <button class="tag mono" class:warnt={mt.how === 'down'} title="{mt.host}:{mt.path}{mt.how === 'down' ? '（这台机器现在用不了）' : ''} · 点击取下" onclick={() => unmountWorkspace(mt.id)}><Icon name="folder" size={11} />{mt.host}:{shortPath(homeShort(mt.path), info.workspaces.length > 1 ? 22 : 32)}</button>
            {/each}
            <button class="tag" onclick={() => (picking = true)} title="再挂一个工作目录"><Icon name="plus" size={11} />挂载</button>
          {:else if innerKind}
            <span class="tag">{innerKind === 'life' ? '内心活动 · 可以留言' : '整理记忆 · 只能看'}</span>
          {:else}
            <button class="tag" onclick={() => (picking = true)} title="没有挂载工作目录：在 HOME 下工作，只能写临时目录；写外面要提权。点这里挂一个">未挂目录</button>
          {/if}
          {#if info.thinking}<span class="tag">思考 {info.thinking === 'none' ? '关闭' : info.thinking}</span>{/if}
          {#if info.usage.prompt_tokens}<span class="tag accent">缓存命中 {pct(info.usage.cached_tokens, info.usage.prompt_tokens)}</span>{/if}
        {/if}
      </div>
    </div>
    {#if room}<button class="btn sm" onclick={toggleRoom}>{room.paused ? '恢复讨论' : '暂停讨论'}</button>{/if}
    {#if info && !innerKind && !isRoom}<AgentSwitcher {sid} compact />{/if}
    {#if !innerKind && !isRoom}<button class="btn sm ghost" onclick={() => act('reset')} title="清空上下文"><Icon name="trash" size={14} /></button>{/if}
    <button class="btn sm ghost" onclick={() => room ? store.open({ type: 'room', id: room.id }) : store.open({ type: 'session', sid })} title={room ? '群成员和设置' : '会话设置'}><Icon name="sliders" size={14} /></button>
  </header>
  {#if room}
    <div class="room-members" aria-label="群成员">
      {#each roomMembers as member (member.id)}
        <button class="member-chip" title="@{member.name}" onclick={() => mention(member.name)}><span class="speaker-avatar">{member.emoji || '🙂'}</span>{member.name}{#if room.muted.includes(member.id)}<small>已禁言</small>{/if}</button>
      {/each}
      {#if room.paused}<span class="room-paused">讨论已暂停，发言或恢复后继续</span>{/if}
    </div>
  {/if}
  {#if info?.context && !isRoom}
    {@const c = info.context}
    <details class="context-meter">
      <summary>上下文 {c.total.toLocaleString()} / {c.window.toLocaleString()} · {Math.round(c.total / c.window * 100)}% · {c.model}
        <progress max={c.window} value={c.total}></progress>
      </summary>
      <div>人格 {c.persona} · 工具定义 {c.tools} · 摘要 {c.summary}（已整理 {c.compacted} 条）</div>
      <div>对话 {c.dialogue}（{c.messages} 条，工具结果 {c.tool_results}）</div>
      <div>上次输入 {c.last.prompt_tokens} · 缓存命中 {Math.round((c.last.cached_tokens ?? 0) / Math.max(1, c.last.prompt_tokens) * 100)}% · 输出 {c.last.completion_tokens}</div>
      <div>到 {c.auto_at} 时自动整理 · 空闲 {c.idle_for} 后整理 · /compact 现在整理</div>
    </details>
  {/if}

  <HistoryManager {sid} {summary} busy={!!phase} onchanged={load} />
  <div class="scroll" bind:this={scroller} onscroll={onScroll}>
    <div class="inner">
      {#if summary}
        <div class="sys"><Icon name="layers" size={13} />{summary.replace(/^\[系统\] /, '').split('\n')[0]}</div>
      {/if}
      {#if !loading && !blocks.length && !live.length}
        <div class="empty">
          <Icon name="chat" size={26} />
          <p>说点什么开始对话吧。</p>
          {#if eps.some((e) => e.channel !== 'web')}
            <p class="muted">{eps.filter((e) => e.channel !== 'web').map(endpointLabel).join('、')}也接在这个会话上，在哪边说话都是同一段上下文。</p>
          {/if}
          {#if info && !info.workspaces.length && !isRoom}
            <p class="muted">想让它读写文件、执行命令？先给它 <button class="lnk" onclick={() => (picking = true)}>挂一个工作目录</button>，本机、别的电脑或者 SSH 服务器上的都行，可以挂好几个。</p>
          {/if}
        </div>
      {/if}

      {#each episodeGroups as episode}
        <section class="episode">
          {#if innerKind === 'life'}
            <button class="btn sm ghost" onclick={() => toggleEpisode(episode.start)}>
              <Icon name="sparkles" size={13} />{episode.title} · {episode.latest || expandedEpisodes.has(episode.start) ? '收起 / 展开' : '展开'}
            </button>
          {/if}
          <div hidden={innerKind === 'life' && (episode.latest ? expandedEpisodes.has(episode.start) : !expandedEpisodes.has(episode.start))}>
      {#each episode.blocks as b, localIndex}
        {@const i = episode.start + localIndex}
        {#if b.type === 'env'}
          <div class="sys"><Icon name="folder" size={13} />{b.text}</div>
        {:else if b.type === 'wake'}
          <div class="wake"><Icon name={innerKind === 'dream' ? 'brain' : 'sparkles'} size={13} /><div>{b.text}</div></div>
        {:else if b.type === 'note'}
          <details class="memnote k-{b.kind}">
            <summary><Icon name={b.kind === 'life' ? 'sparkles' : 'brain'} size={12} />{noteTitle[b.kind]}<span class="muted">（网关放进这一轮的，点开看）</span></summary>
            <pre>{b.text}</pre>
          </details>
        {:else if b.type === 'user'}
          <div class="msg user">
            <div class="meta">
              {#if i === lastBlock && !isRoom}<button class="undo" onclick={undo} disabled={undoing} title="撤回这一轮：连同回复从上下文里删掉，内容放回输入框"><Icon name="undo" size={12} />撤回</button>{/if}
              {#if isRoom}<span class="speaker-avatar human">你</span>{/if}
              {#if b.via}<i class="via" style="--c: {channelMeta[viaChannel[b.via]]?.color ?? 'var(--muted)'}"></i>{b.sender ?? b.via}{:else}{b.sender ?? '用户'}{/if}
              {#if b.time}<span>{b.time}</span>{/if}
            </div>
            {#if b.images?.length}
              <div class="pics">
                {#each b.images as p, i (`${p.id}:${i}`)}
                  <a class="pic" href="/api/uploads/{p.id}" target="_blank" rel="noopener"><img src="/api/uploads/{p.id}" alt={p.name ?? '图片'} loading="lazy" /></a>
                {/each}
              </div>
            {/if}
            {#if b.text}<div class="bubble">{b.text}</div>{/if}
          </div>
        {:else}
          <div class="msg bot">
            {#if b.proactive}<div class="meta">主动发来 · {b.reason ?? '自己的时间'}{#if b.episode} · 片段 {b.episode}{/if}{#if b.agenda} · 日程 {b.agenda}{/if}</div>{/if}
            {#if isRoom}
              {@const speaker = roomSpeaker(b.agent, b.sender)}
              <div class="meta room-speaker"><span class="speaker-avatar">{speaker?.emoji || '🙂'}</span><b>{speaker?.name ?? b.sender ?? '成员'}</b>{#if b.ts}<span>{clock(b.ts * 1000)}</span>{/if}</div>
            {/if}
            {#if b.reasoning}
              <details class="think">
                <summary><Icon name="brain" size={13} />思考过程 · {Array.from(b.reasoning).length} 字</summary>
                <div class="think-body">{b.reasoning}</div>
              </details>
            {/if}
            {#if b.text}
              <div class="md">{@html markdown(b.text)}</div>
            {/if}
            {#if b.proposed?.length}
              <details class="think"><summary>未接受的工具提案（未执行）</summary><p>{b.reason || '未提交给执行内核。'}</p>
                {#each b.proposed as proposal}<p>{proposal.function.name} · {proposal.id}</p><pre>{proposal.function.arguments}</pre>{/each}
              </details>
            {/if}
            {#if b.tools.length}
              <div class="tools">
                {#each b.tools as t}
                  <ToolCard name={t.name} detail={t.detail} args={t.args} ok={t.ok} result={t.result} media={t.media} sandbox={t.sandbox} />
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

          </div>
        </section>
      {/each}

      <!-- 正在进行的这一轮：实时内容 -->
      {#each live as it, i}
        {#if it.kind === 'user'}
          {@const raw = it.text}
          <div class="msg user">
            <div class="meta">
              {#if i === lastLive}<button class="undo" onclick={undo} disabled={undoing} title="撤回：还没处理的直接拿掉，正在处理的先停下再删掉"><Icon name="undo" size={12} />撤回</button>{/if}
              {#if it.via}<i class="via" style="--c: {channelMeta[it.channel ?? '']?.color ?? 'var(--muted)'}"></i>{it.via}{:else}{it.name ?? '用户'}{/if}
              <span>{clock(it.at)}</span>
            </div>
            {#if it.images?.length}
              <div class="pics">
                {#each it.images as p, i (`${p.id}:${i}`)}
                  <a class="pic" href="/api/uploads/{p.id}" target="_blank" rel="noopener"><img src="/api/uploads/{p.id}" alt={p.name ?? '图片'} /></a>
                {/each}
              </div>
            {/if}
            {#if raw}<div class="bubble">{raw}</div>{/if}
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
            <ToolCard name={it.name} detail={it.detail} args={it.args} ok={it.ok} result={it.result} media={it.media} sandbox={it.sandbox} callState={it.state} running />
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

  {#if noModel}
    <div class="nomodel">
      <Icon name="sparkles" size={14} />
      <span>还没有配置模型，先去模型节点添加接口和模型。</span>
      <button class="btn sm primary" onclick={() => store.openOnMap({ type: 'model' })}>去配置模型</button>
    </div>
  {/if}
  {#if innerKind === 'dream'}
    <div class="innerbar">
      <Icon name="brain" size={14} />
      <span>这是{innerAgent?.name ?? '它'}最近一次整理记忆的过程，只能看。</span>
    </div>
  {:else}
  {#if innerKind === 'life'}
    <div class="innerbar">
      <Icon name="sparkles" size={14} />
      <span>这是{innerAgent?.name ?? '它'}的内心活动。在下面说话会叫醒它，它会把这句当成一轮任务；也可以只叫醒它。</span>
      <button class="btn sm" onclick={wakeInner}>现在叫醒它</button>
    </div>
  {/if}
  <div class="composer" class:dragging role="group" aria-label="输入区" onpaste={onPaste} ondragover={(e) => { e.preventDefault(); dragging = true; }} ondragleave={() => (dragging = false)} ondrop={onDrop}>
    {#if mentionMatches.length}
      <div class="cmdmenu mentions" role="listbox" aria-label="选择要点名的成员">
        {#each mentionMatches as member, i (member.id)}
          <button class="cmd" class:sel={i === mentionIndex} role="option" aria-selected={i === mentionIndex} onmousedown={(e) => { e.preventDefault(); mention(member.name, true); }} onmouseenter={() => (mentionIndex = i)}><span class="speaker-avatar">{member.emoji || '🙂'}</span><b>{member.name}</b><span class="desc">@ 点名发言</span></button>
        {/each}
        <div class="keys">↑↓ 选择 · Tab / Enter 点名 · Esc 收起</div>
      </div>
    {/if}
    {#if cmdMatches.length || cmdHint}
      <div class="cmdmenu" role="listbox">
        {#each cmdMatches as c, i (c.name)}
          <button class="cmd" class:sel={i === menuIdx} role="option" aria-selected={i === menuIdx} onmousedown={(e) => { e.preventDefault(); pickCmd(c, false); }} onmouseenter={() => (menuIdx = i)}>
            <code>/{c.name}</code>{#if c.args}<span class="args">{c.args}</span>{/if}
            <span class="desc">{c.desc}</span>
          </button>
        {/each}
        {#if cmdHint}
          <div class="cmd hint"><code>/{cmdHint.name}</code>{#if cmdHint.args}<span class="args">{cmdHint.args}</span>{/if}<span class="desc">{cmdHint.desc}</span></div>
        {:else}
          <div class="keys">↑↓ 选择 · Tab 补全 · Enter 确定 · Esc 收起</div>
        {/if}
      </div>
    {/if}
    {#if pics.length || uploading}
      <div class="pending">
        {#each pics as p (p.id)}
          <div class="pend">
            <img src="/api/uploads/{p.id}" alt={p.name ?? '图片'} />
            <button class="rm" onclick={() => removePic(p.id)} aria-label="去掉这张图片"><Icon name="x" size={11} /></button>
          </div>
        {/each}
        {#each Array(uploading) as _, i (i)}
          <div class="pend up"><span class="spinner"></span></div>
        {/each}
      </div>
    {/if}
    <button class="btn ghost attach" onclick={() => fileInput?.click()} title={isRoom ? '群聊目前支持文字' : '发图片（也可以粘贴或拖进来）'} disabled={isRoom || pics.length + uploading >= maxPics}>
      <Icon name="file" size={16} />
    </button>
    <input bind:this={fileInput} type="file" accept="image/*" multiple hidden onchange={pickFiles} />
    <textarea bind:this={ta} bind:value={text} onkeydown={onkey} oninput={trackCursor} onclick={trackCursor} onkeyup={(e) => { if (!['Enter', 'Tab', 'Escape', 'ArrowUp', 'ArrowDown'].includes(e.key)) trackCursor(); }} rows="1" placeholder={isRoom ? "对大家说一句，输入 @ 点名；Enter 发送，Shift+Enter 换行" : innerKind === 'life' ? "对它说一句：它会当作一轮任务，Enter 发送；也可以先只叫醒它" : "在这个会话里说话，Enter 发送，Shift+Enter 换行；可以粘贴或拖入图片。打 / 看命令"}></textarea>
    {#if info?.running}
      <button class="btn stop" onclick={() => act('stop')} title="停下正在进行的任务（聊天里发「停」也行）"><Icon name="stop" size={13} />停止</button>
    {/if}
    <button class="btn primary" onclick={send} disabled={noModel || (!text.trim() && !pics.length) || sending || uploading > 0}><Icon name="send" size={14} />发送</button>
  </div>
  {/if}
</div>
{/if}

{#if picking && info}
  <WorkspacePicker title="挂载工作目录" mounted={info.workspaces.map((m) => m.id)} onchange={mountWorkspaces} onclose={() => (picking = false)} />
{/if}

<style>
  .room-members { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 10px 22px; border-bottom: 1px solid var(--line); }
  .member-chip { display: inline-flex; align-items: center; gap: 6px; border: 1px solid var(--line); border-radius: 20px; padding: 4px 10px 4px 5px; background: var(--bg-1); color: var(--text); font: inherit; font-size: 12px; cursor: pointer; }
  .member-chip:hover { border-color: var(--accent); }
  .speaker-avatar { display: inline-flex; align-items: center; justify-content: center; width: 26px; height: 26px; flex-shrink: 0; border-radius: 9px; background: var(--bg-2); font-size: 18px; }
  .speaker-avatar.human { font-size: 11px; color: var(--accent); }
  .room-speaker b { color: var(--text-2); }
  .room-paused, .member-chip small { color: var(--muted); font-size: 12px; }
  .room-chat .msg.bot { padding: 10px 14px; border: 1px solid var(--line); border-radius: 14px; background: var(--bg-1); }
  .innerbar {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 12px 20px;
    border-top: 1px solid var(--line);
    color: var(--text-2);
    font-size: 13px;
    background: color-mix(in srgb, var(--accent) 6%, var(--bg-1));
  }
  .innerbar span {
    flex: 1;
  }
  .wake {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    padding: 10px 14px;
    border-radius: 12px;
    border: 1px dashed color-mix(in srgb, var(--accent-2) 40%, var(--line));
    background: color-mix(in srgb, var(--accent-2) 6%, transparent);
    color: var(--text-2);
    font-size: 12.5px;
    line-height: 1.65;
    white-space: pre-wrap;
  }
  .wake :global(svg) {
    flex: none;
    margin-top: 3px;
  }
  /* 网关放在用户消息前面的说明：跟着下面那条用户消息，靠右 */
  .memnote {
    margin: 4px 0 2px;
    font-size: 12px;
    color: var(--muted);
    align-self: flex-end;
    max-width: 85%;
    text-align: left;
  }
  .memnote summary {
    float: right;
  }
  .memnote[open] summary {
    float: none;
  }
  .memnote summary {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    cursor: pointer;
    padding: 3px 9px;
    border-radius: 8px;
    border: 1px dashed var(--line);
    list-style: none;
  }
  .memnote summary::-webkit-details-marker {
    display: none;
  }
  .memnote.k-life summary {
    border-color: color-mix(in srgb, var(--accent-2) 45%, var(--line));
  }
  .memnote pre {
    margin: 6px 0 0;
    padding: 10px 12px;
    white-space: pre-wrap;
    font: inherit;
    font-size: 12px;
    line-height: 1.6;
    background: var(--bg-2);
    border-radius: 10px;
    max-height: 320px;
    overflow: auto;
  }
  .context-meter { padding: 8px 20px; border-bottom: 1px solid var(--line); font-size: 12px; color: var(--muted); }
  .context-meter progress { width: 100%; height: 5px; accent-color: var(--accent); display: block; margin-top: 5px; }
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
  .chips .warnt {
    color: var(--warn);
    border-color: color-mix(in srgb, var(--warn) 40%, var(--line));
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
    align-items: center;
    gap: 6px;
  }
  .meta span {
    margin-left: 2px;
  }
  /* 撤回：只在最后一条用户消息上，鼠标移上去才明显 */
  .undo {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    padding: 1px 7px;
    border-radius: 7px;
    border: 1px solid transparent;
    background: none;
    color: var(--muted);
    font-size: 11.5px;
    cursor: pointer;
    opacity: 0.45;
    transition: opacity 0.15s, color 0.15s, border-color 0.15s;
  }
  .msg.user:hover .undo,
  .undo:focus-visible {
    opacity: 1;
  }
  .undo:hover {
    color: var(--text);
    border-color: var(--line-2);
  }
  .via {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--c);
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
  .nomodel {
    max-width: 900px;
    width: 100%;
    margin: 0 auto;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    border-radius: 10px;
    border: 1px solid color-mix(in srgb, var(--warn) 45%, var(--line));
    background: color-mix(in srgb, var(--warn) 8%, var(--panel-2));
    color: var(--text-2);
    font-size: 12px;
  }
  .nomodel span {
    flex: 1;
  }
  .composer {
    position: relative;
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
  .composer.dragging {
    outline: 2px dashed var(--accent);
    outline-offset: -6px;
    border-radius: 14px;
    background: color-mix(in srgb, var(--accent) 7%, transparent);
  }
  .attach {
    height: 44px;
    width: 44px;
    padding: 0;
    justify-content: center;
  }
  .pending {
    position: absolute;
    bottom: calc(100% - 4px);
    left: 22px;
    display: flex;
    gap: 7px;
    padding: 6px;
    border-radius: 12px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow);
  }
  .pend {
    position: relative;
    width: 64px;
    height: 64px;
    border-radius: 9px;
    overflow: hidden;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    display: grid;
    place-items: center;
  }
  .pend img {
    width: 100%;
    height: 100%;
    object-fit: cover;
  }
  .pend .rm {
    position: absolute;
    top: 2px;
    right: 2px;
    width: 18px;
    height: 18px;
    padding: 0;
    display: grid;
    place-items: center;
    border-radius: 50%;
    border: none;
    color: #fff;
    background: rgba(0, 0, 0, 0.55);
  }
  .pend .rm:hover {
    background: var(--err);
  }
  .pics {
    display: flex;
    flex-wrap: wrap;
    gap: 7px;
    margin: 2px 0 6px;
    max-width: min(560px, 100%);
  }
  .pic {
    display: block;
    border-radius: 11px;
    overflow: hidden;
    border: 1px solid var(--line-2);
    line-height: 0;
    background: var(--bg-2);
  }
  .pic img {
    max-width: 220px;
    max-height: 220px;
    object-fit: contain;
    display: block;
  }
  .cmdmenu {
    position: absolute;
    left: 22px;
    right: 22px;
    bottom: calc(100% - 6px);
    z-index: 20;
    display: flex;
    flex-direction: column;
    padding: 6px;
    border-radius: 13px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    box-shadow: var(--shadow);
    max-height: 340px;
    overflow: auto;
    animation: rise 0.12s ease;
  }
  .cmd {
    display: flex;
    align-items: baseline;
    gap: 9px;
    padding: 7px 10px;
    border-radius: 8px;
    border: 0;
    background: none;
    color: var(--text);
    text-align: left;
    font: inherit;
    font-size: 13px;
    cursor: pointer;
  }
  .cmd.sel {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .cmd code {
    font-family: var(--mono);
    color: var(--accent-2);
    flex: none;
  }
  .cmd .args {
    font-family: var(--mono);
    font-size: 11.5px;
    color: var(--muted);
    flex: none;
  }
  .cmd .desc {
    color: var(--text-2);
    font-size: 12.5px;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .cmd.hint {
    cursor: default;
    flex-wrap: wrap;
  }
  .cmd.hint .desc {
    white-space: normal;
  }
  .keys {
    padding: 5px 10px 2px;
    font-size: 11px;
    color: var(--muted);
  }
  .composer .stop {
    color: var(--err);
    border-color: color-mix(in srgb, var(--err) 35%, transparent);
  }
  .gone {
    align-items: center;
    justify-content: center;
    gap: 12px;
    color: var(--muted);
  }
  .gone p {
    margin: 0;
  }
  .tag.ep {
    color: var(--text-2);
    border-color: color-mix(in srgb, var(--c) 40%, var(--line));
    background: color-mix(in srgb, var(--c) 10%, transparent);
  }
  .tag.ep :global(svg) {
    color: var(--c);
  }
</style>
