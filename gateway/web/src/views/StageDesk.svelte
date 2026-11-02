<script lang="ts">
  // 导演台（设计 §14.3）：一场戏的整页——时间线、控制、导出和放映。
  import { onMount } from 'svelte';
  import { api } from '../lib/api';
  import { store } from '../lib/state.svelte';
  import { buildCues, buildItems, forkable, type Cue, type DocItem, type StageRec } from '../lib/stage';
  import Icon from '../components/Icon.svelte';
  import RoomStage from '../components/RoomStage.svelte';

  let { id }: { id: string } = $props();
  const r = $derived(store.room(id));

  const name = (aid: string) => store.agents.find((a) => a.id === aid)?.name ?? aid;
  const channelName = (cid: string) => r?.channels?.find((c) => c.id === cid)?.name ?? cid;

  // ---- 时间线：按序号增量取 ----
  let recs = $state<StageRec[]>([]);
  let last = 0;
  let loadErr = $state('');
  let listEl: HTMLElement | undefined = $state();
  let stick = true; // 在最底下时，新的一句自动滚到底

  // 同一时刻只读一次；读的时候又来了新事件，读完再读一遍（不然两次读会取到同一段，时间线里每行出现两次）
  let loading = false;
  let again = false;
  async function load(reset = false) {
    if (!id) return;
    if (reset) {
      recs = [];
      last = 0;
    }
    if (loading) {
      again = true;
      return;
    }
    loading = true;
    try {
      do {
        again = false;
        for (;;) {
          const mine = id;
          const res = await api.get<{ items: StageRec[]; last: number }>(`/api/rooms/${encodeURIComponent(mine)}/timeline?after=${last}&n=500`);
          if (mine !== id) break; // 读的时候换了群：这一段作废，按新的群再读
          loadErr = '';
          const fresh = res.items.filter((x) => (x.seq ?? 0) > last);
          if (!fresh.length) break;
          recs = [...recs, ...fresh];
          last = fresh[fresh.length - 1].seq ?? last;
          if (res.items.length < 500) break;
        }
      } while (again);
    } catch (err) {
      loadErr = String((err as Error).message ?? err);
    } finally {
      loading = false;
    }
    if (stick) queueMicrotask(() => listEl?.scrollTo({ top: listEl.scrollHeight }));
  }

  onMount(() => store.on((e) => {
    if (e.t === 'stage' && e.data?.room === id) load();
  }));
  $effect(() => {
    id; // 换了群就重新读
    load(true);
  });

  const canFork = $derived(forkable(recs));
  const items = $derived(buildItems(recs, channelName, name));
  const runOf = $derived.by(() => {
    const m = new Map<number, StageRec>();
    for (const x of recs) if (x.kind === 'run' && x.said) m.set(x.said, x);
    return m;
  });
  const modelsOf = $derived.by(() => {
    const m = new Map<string, StageRec[]>();
    for (const x of recs) if (x.kind === 'model' && x.run) (m.get(x.run) ?? m.set(x.run, []).get(x.run)!).push(x);
    return m;
  });

  // 显示和隐藏
  let show = $state({ inner: true, channels: true, cue: true, system: true });
  const rows = $derived(
    items.filter((it) => {
      if (it.kind === 'inner') return show.inner && (!it.channel || show.channels);
      if (it.channel && !show.channels) return false;
      if (it.kind === 'cue') return show.cue;
      if (it.kind === 'system') return show.system;
      return true;
    }),
  );
  let open = $state<Record<number, boolean>>({}); // 点开一轮看推理和用量

  const tokens = (u?: StageRec['usage']) => (u ? `${u.prompt_tokens ?? 0} + ${u.completion_tokens ?? 0}` : '—');

  // ---- 分出 ----
  let busy = $state(false);
  async function fork(line: number, rewind: boolean) {
    if (busy) return;
    if (rewind && !confirm(`从第 ${line} 句重来？这一场会先暂停，新的一场从这里接着演。`)) return;
    busy = true;
    try {
      const info = await api.post<{ id: string; name: string }>(`/api/rooms/${encodeURIComponent(id)}/fork`, { at: line, rewind });
      await store.refresh();
      store.toast({ kind: 'ok', text: `分出了「${info.name}」` }, 2600);
      location.hash = '#/stage/' + encodeURIComponent(info.id);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }

  // ---- 控制 ----
  async function patch(body: Record<string, unknown>) {
    try {
      await api.patch('/api/rooms/' + encodeURIComponent(id), body);
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
  let cueNote = $state('');
  let cueChan = $state('');
  async function cue(agent: string) {
    try {
      await api.post(`/api/rooms/${encodeURIComponent(id)}/cue`, { agent, note: cueNote, channel: cueChan });
      cueNote = '';
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
  function toggleMuted(aid: string) {
    if (!r) return;
    patch({ muted: r.muted.includes(aid) ? r.muted.filter((m) => m !== aid) : [...r.muted, aid] });
  }

  // ---- 导出 ----
  let fmt = $state<'srt' | 'md' | 'json'>('srt');
  let exInner = $state(false);
  let exChannels = $state(false);
  let exFrom = $state('');
  let exTo = $state('');
  const exportURL = $derived(
    `/api/rooms/${encodeURIComponent(id)}/export?format=${fmt}&inner=${exInner ? 1 : 0}&channels=${exChannels ? 1 : 0}` +
      (exFrom ? `&from=${exFrom}` : '') + (exTo ? `&to=${exTo}` : ''),
  );

  // ---- 放映：按字幕的时间一句一句放出来 ----
  let playing = $state(false);
  let pInner = $state(false);
  let pChannels = $state(false);
  let speed = $state(1);
  let cues = $state<Cue[]>([]);
  let at = $state(-1);
  let ended = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let playItems: DocItem[] = []; // 开始那一刻的时间线：放映中新来的句子不插进这一遍

  function step(i: number) {
    clearTimeout(timer);
    if (i >= cues.length) {
      ended = true;
      at = cues.length - 1;
      return;
    }
    at = i;
    const dur = cues[i].end - cues[i].start;
    const gap = i + 1 < cues.length ? cues[i + 1].start - cues[i].end : 0;
    timer = setTimeout(() => step(i + 1), (dur + gap) / speed);
  }
  function startPlay() {
    playItems = items;
    cues = buildCues(playItems, { inner: pInner, channels: pChannels });
    ended = false;
    at = -1;
    playing = true;
    if (!cues.length) {
      ended = true;
      return;
    }
    step(0);
  }
  // 放映中切换心里话和频道：按新的开关重新排字幕，从句子号不小于当前这一条的第一条接着放
  function togglePlay(kind: 'inner' | 'channels', on: boolean) {
    if (kind === 'inner') pInner = on;
    else pChannels = on;
    if (!playing) return;
    const line = at >= 0 && cues[at] ? cues[at].line : 0;
    cues = buildCues(playItems, { inner: pInner, channels: pChannels });
    if (ended) {
      at = cues.length - 1; // 放完了：停在新字幕的最后一条，不指着旧的下标
      return;
    }
    const next = cues.findIndex((c) => c.line >= line);
    if (next < 0) {
      clearTimeout(timer);
      ended = true;
      at = cues.length - 1;
    } else {
      step(next);
    }
  }
  function stopPlay() {
    clearTimeout(timer);
    playing = false;
  }
  function changeSpeed(s: number) {
    speed = s;
    if (playing && !ended && at >= 0) step(at); // 当前这条按新的速度重新计时
  }
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && playing) {
        e.preventDefault();
        stopPlay();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
      clearTimeout(timer);
    };
  });
  const shown = $derived(at >= 0 ? cues[at] : undefined);

  const modeLabel: Record<string, string> = { mention: '点名', free: '自由', turns: '轮流', director: '导演' };
  function tag(it: DocItem) {
    if (it.kind === 'say') return it.who ?? '';
    if (it.kind === 'narrate') return '旁白';
    if (it.kind === 'cue') return '导演→' + it.to;
    if (it.kind === 'inner') return (it.who ?? '') + ' 心里';
    return '系统';
  }
</script>

<div class="desk">
  {#if !r}
    <p class="empty">没有这个群，或者还在读。<a href="#/">回到拓扑图</a></p>
  {:else if !r.stage}
    <p class="empty">这个群不是一场戏，没有导演台。<a href="#/">回到拓扑图</a></p>
  {:else}
    <header>
      <a class="back" href="#/" title="回到拓扑图">‹</a>
      <h2>{r.name}</h2>
      <span class="badge" class:warn={r.paused}>{r.paused ? '暂停' : '在演'}</span>
      <span class="badge">{modeLabel[r.mode ?? 'mention']}</span>
      {#if r.inner}<span class="badge">心里话</span>{/if}
      <span class="grow"></span>
      <button class="btn sm" onclick={() => patch({ paused: !r.paused })}>{r.paused ? '恢复' : '暂停'}</button>
      <button class="btn sm primary" onclick={startPlay} disabled={!items.length}><Icon name="play" size={13} />放映</button>
    </header>

    <div class="cols">
      <section class="tl">
        <div class="toggles">
          <label><input type="checkbox" bind:checked={show.inner} />心里话</label>
          <label><input type="checkbox" bind:checked={show.channels} />频道</label>
          <label><input type="checkbox" bind:checked={show.cue} />cue</label>
          <label><input type="checkbox" bind:checked={show.system} />系统提示</label>
          <span class="muted">{rows.length} 行</span>
        </div>
        {#if loadErr}<p class="err">读时间线失败：{loadErr}</p>{/if}
        <div class="list" bind:this={listEl} onscroll={() => { if (listEl) stick = listEl.scrollTop + listEl.clientHeight >= listEl.scrollHeight - 40; }}>
          {#each rows as it, i (i + ':' + it.kind + ':' + it.line)}
            {@const run = it.kind === 'say' ? runOf.get(it.line) : undefined}
            <div class="row {it.kind}" class:chan={!!it.channel}>
              <span class="no">{it.kind === 'inner' ? '' : it.line}</span>
              <div class="body">
                <div class="who">
                  <b>{tag(it)}</b>
                  {#if it.channel}<span class="chip">私下·{it.channel}</span>{/if}
                  {#if it.kind !== 'inner' && it.kind !== 'system'}
                    <span class="acts">
                      {#if canFork.has(it.line)}
                        <button class="lnk" disabled={busy} onclick={() => fork(it.line, false)} title="从这一句分出平行世界，这一场不受影响">从这里分出</button>
                        <button class="lnk" disabled={busy} onclick={() => fork(it.line, true)} title="从这一句分出新的一场，并暂停这一场">从这句重来</button>
                      {/if}
                    </span>
                  {/if}
                </div>
                <div class="text">{it.text}</div>
                {#if run}
                  <button class="lnk small" onclick={() => (open[it.line] = !open[it.line])}>{open[it.line] ? '收起' : '这一轮'}：用量 {tokens(run.usage)}</button>
                  {#if open[it.line]}
                    <div class="detail">
                      <div>状态 {run.status} · 用量 {tokens(run.usage)}（输入 + 输出）</div>
                      {#each modelsOf.get(run.run ?? '') ?? [] as m (m.seq)}
                        <div>模型 {m.model?.model}（{m.model?.endpoint}）· 结束 {m.model?.reason}{m.model?.err ? ' · ' + m.model.err : ''}</div>
                        {#if m.reasoning}<pre>{m.reasoning}</pre>{:else}<div class="muted">没有记下推理</div>{/if}
                      {/each}
                    </div>
                  {/if}
                {/if}
              </div>
            </div>
          {:else}
            <p class="empty">这场戏还没有内容。在右边用旁白开个头吧。</p>
          {/each}
        </div>
      </section>

      <aside class="ctl">
        <RoomStage {r} onsave={patch} desk />

        <h5>让 TA 说 <span class="muted">只对 TA 说一句提示，再叫 TA 开口</span></h5>
        <div class="card">
          <input class="in" bind:value={cueNote} placeholder="给 TA 的提示（只有 TA 看得到，可空）" />
          {#if r.channels?.length}
            <select class="in" bind:value={cueChan}>
              <option value="">公开</option>
              {#each r.channels as c (c.id)}<option value={c.id}>频道：{c.name}</option>{/each}
            </select>
          {/if}
          {#each r.members as m (m)}
            <div class="mrow">
              <span class="grow">{name(m)}{#if r.muted.includes(m)}<span class="badge warn">禁言</span>{/if}</span>
              <button class="btn sm" disabled={r.paused || r.muted.includes(m)} onclick={() => cue(m)}>让 TA 说</button>
              <button class="lnk" onclick={() => toggleMuted(m)}>{r.muted.includes(m) ? '解除禁言' : '禁言'}</button>
            </div>
          {/each}
        </div>

        <h5>用量</h5>
        <div class="card">
          <div>已转 {r.use?.round ?? 0} 轮{r.rounds ? ` / ${r.rounds}` : ''}</div>
          <div>这一场 {r.use?.total ?? 0} token{r.budget_total ? ` / ${r.budget_total}` : ''}</div>
          <div>今天 {r.use?.day_tokens ?? 0} token{r.budget ? ` / ${r.budget}` : ''}</div>
          {#if r.use?.over}<div class="warn">用量到了上限</div>{/if}
          <button class="lnk" onclick={() => patch({ reset: true })}>清零累计（含轮数）</button>
        </div>

        <h5>导出</h5>
        <div class="card">
          <div class="fmts">
            {#each [['srt', '字幕 .srt'], ['md', '剧本 .md'], ['json', '记录 .json']] as [v, label] (v)}
              <label><input type="radio" bind:group={fmt} value={v} />{label}</label>
            {/each}
          </div>
          <label><input type="checkbox" bind:checked={exInner} />带上心里话</label>
          <label><input type="checkbox" bind:checked={exChannels} />带上私下频道和 cue</label>
          <div class="range">
            <input class="in" inputmode="numeric" bind:value={exFrom} placeholder="从第几句（空为开头）" />
            <input class="in" inputmode="numeric" bind:value={exTo} placeholder="到第几句（空为结尾）" />
          </div>
          <a class="btn sm primary" href={exportURL} download>导出</a>
        </div>
      </aside>
    </div>

    {#if playing}
      <div class="screen" role="dialog" aria-label="放映">
        <div class="bar">
          <label><input type="checkbox" checked={pInner} onchange={(e) => togglePlay('inner', (e.target as HTMLInputElement).checked)} />心里话</label>
          <label><input type="checkbox" checked={pChannels} onchange={(e) => togglePlay('channels', (e.target as HTMLInputElement).checked)} />频道</label>
          {#each [1, 2] as s (s)}
            <button class="btn sm" class:primary={speed === s} onclick={() => changeSpeed(s)}>{s}×</button>
          {/each}
          <button class="btn sm" onclick={startPlay}>重放</button>
          <button class="btn sm danger" onclick={stopPlay}>停止</button>
        </div>
        <div class="sub" class:inner={shown?.kind === 'inner'}>
          {#if ended && !cues.length}
            <span class="muted">没有可以放的内容</span>
          {:else if shown}
            {shown.text}
          {/if}
        </div>
        <div class="progress">{ended ? '放映结束' : `${Math.max(at, 0) + 1} / ${cues.length}`}</div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .desk { flex: 1; min-width: 0; display: flex; flex-direction: column; background: var(--bg); }
  header { display: flex; align-items: center; gap: 10px; padding: 10px 16px; border-bottom: 1px solid var(--line); }
  header h2 { margin: 0; font-size: 16px; }
  .back { font-size: 22px; line-height: 1; color: var(--text-2); text-decoration: none; padding: 0 6px; }
  .grow { flex: 1; }
  .badge { font-size: 12px; padding: 2px 8px; border-radius: 999px; border: 1px solid var(--line-2); color: var(--text-2); }
  .badge.warn, .warn { color: var(--warn); border-color: var(--warn); }
  .cols { flex: 1; min-height: 0; display: flex; }
  .tl { flex: 1; min-width: 0; display: flex; flex-direction: column; border-right: 1px solid var(--line); }
  .toggles { display: flex; gap: 14px; align-items: center; padding: 8px 16px; border-bottom: 1px solid var(--line); font-size: 13px; }
  .toggles label, .card label { display: flex; align-items: center; gap: 5px; }
  .list { flex: 1; min-height: 0; overflow: auto; padding: 8px 16px 24px; }
  .row { display: flex; gap: 10px; padding: 6px 0; border-bottom: 1px dashed var(--line); }
  .row .no { width: 34px; flex: none; text-align: right; color: var(--muted); font-size: 12px; padding-top: 3px; }
  .body { flex: 1; min-width: 0; }
  .who { display: flex; gap: 8px; align-items: center; font-size: 13px; }
  .acts { margin-left: auto; display: flex; gap: 10px; opacity: 0; }
  .row:hover .acts, .row:focus-within .acts { opacity: 1; }
  @media (hover: none) { .acts { opacity: 1; } } /* 手机和平板没有悬停 */
  .text { white-space: pre-wrap; overflow-wrap: anywhere; margin-top: 2px; }
  .row.inner .text { font-style: italic; color: var(--text-2); }
  .row.narrate .text { color: var(--accent-2); }
  .row.cue .text, .row.system .text { color: var(--muted); }
  .row.chan { background: color-mix(in srgb, var(--accent) 7%, transparent); }
  .chip { font-size: 11px; padding: 1px 6px; border-radius: 6px; background: color-mix(in srgb, var(--accent) 18%, transparent); }
  .lnk { background: none; border: 0; color: var(--accent-2); cursor: pointer; padding: 0; font-size: 12px; }
  .lnk:disabled { opacity: 0.4; cursor: default; }
  .lnk.small { margin-top: 4px; color: var(--muted); }
  .detail { margin-top: 4px; padding: 6px 8px; border-radius: 8px; background: var(--panel-2); font-size: 12px; color: var(--text-2); }
  .detail pre { white-space: pre-wrap; margin: 4px 0 0; font-size: 12px; max-height: 220px; overflow: auto; }
  .ctl { width: 360px; flex: none; overflow: auto; padding: 8px 16px 24px; }
  .ctl h5 { margin: 14px 0 6px; font-size: 13px; }
  .card { border: 1px solid var(--line); border-radius: 10px; padding: 10px; display: flex; flex-direction: column; gap: 8px; background: var(--panel); font-size: 13px; }
  .in { width: 100%; box-sizing: border-box; background: var(--bg-2); color: var(--text); border: 1px solid var(--line-2); border-radius: 8px; padding: 6px 8px; }
  .mrow { display: flex; align-items: center; gap: 8px; }
  .fmts { display: flex; gap: 12px; flex-wrap: wrap; }
  .range { display: flex; gap: 8px; }
  .muted { color: var(--muted); }
  .err { color: var(--err); padding: 8px 16px; margin: 0; }
  .empty { color: var(--muted); padding: 24px; }
  .screen { position: fixed; inset: 0; z-index: 40; background: #05070c; display: flex; flex-direction: column; align-items: center; justify-content: center; }
  .screen .bar { position: absolute; top: 14px; right: 18px; display: flex; gap: 10px; align-items: center; color: var(--text-2); font-size: 13px; }
  .screen .sub { max-width: 80vw; text-align: center; font-size: clamp(22px, 3.4vw, 44px); line-height: 1.5; color: #fff; padding: 0 24px; min-height: 3em; }
  .screen .sub.inner { font-style: italic; color: var(--accent-2); }
  .screen .progress { position: absolute; bottom: 18px; color: var(--muted); font-size: 12px; }
  @media (max-width: 860px) {
    .cols { flex-direction: column; }
    .ctl { width: auto; border-top: 1px solid var(--line); max-height: 45%; }
    .tl { border-right: 0; }
  }
</style>
