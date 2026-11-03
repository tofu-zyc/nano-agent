<script lang="ts">
  // 等我处理：拓扑图上的标记和点开的面板。受阻、就绪的承诺，最新的在前；
  // 每件事可以回答（有问题时）、点“知道了”、打开它。底部展开“全部承诺”，按阶段分组。
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { ago } from '../lib/format';
  import type { CommitItem, PendingItem } from '../lib/types';

  let open = $state(false);
  let all = $state(false);
  let items = $state<CommitItem[]>([]);
  let answers = $state<Record<string, string>>({});
  let busy = $state('');
  let error = $state('');

  const kinds: Record<string, string> = { agenda: '日程', intention: '意图', followup: '回访', schedule: '定时任务', hook: 'Hook', branch: '分支' };
  const phases: Record<string, string> = { blocked: '受阻', ready: '就绪', running: '进行中', waiting: '等待', paused: '暂停', done: '完成', dropped: '无需处理', failed: '失败', stopped: '叫停' };
  const order = ['blocked', 'ready', 'running', 'waiting', 'paused', 'failed', 'done', 'dropped', 'stopped'];

  const key = (p: CommitItem) => `${p.kind}:${p.id}`;
  const agentName = (id: string) => store.ov?.agents?.find((a) => a.id === id)?.name ?? id;
  const n = $derived(store.pending.length);

  async function loadAll() {
    try {
      items = (await api.get<{ items: CommitItem[] }>('/api/commitments')).items ?? [];
    } catch {
      /* 下次再读 */
    }
  }
  $effect(() => {
    store.pendingTick; // 承诺变了，展开着的“全部承诺”跟着重读
    if (open && all) void loadAll();
  });

  const groups = $derived(
    order
      .map((ph) => ({ ph, list: items.filter((i) => i.phase === ph) }))
      .filter((g) => g.list.length),
  );

  async function act(p: PendingItem, what: 'seen' | 'answer') {
    busy = key(p);
    error = '';
    try {
      const body: Record<string, string> = { kind: p.kind, id: p.id, mark: p.mark ?? '' };
      if (what === 'answer') body.text = (answers[key(p)] ?? '').trim();
      await api.post(`/api/pending/${what}`, body);
      delete answers[key(p)];
      await store.loadPending();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
      await store.loadPending();
    } finally {
      busy = '';
    }
  }
  function openItem(p: CommitItem) {
    open = false;
    store.go(p.link);
  }
</script>

{#if n > 0 || open}
  <div class="pending" data-testid="pending">
    <button class="badge" class:on={open} onclick={() => (open = !open)} data-testid="pending-badge" title="受阻的、做完了等你看的事">
      等我处理{#if n > 0} <b data-testid="pending-count">{n}</b>{/if}
    </button>
    {#if open}
      <div class="card" data-testid="pending-panel">
        {#if error}<p class="err">{error}</p>{/if}
        {#if n === 0}<p class="muted">现在没有等你处理的事。</p>{/if}
        {#each store.pending as p (key(p))}
          <article class="row" data-testid="pending-row" data-kind={p.kind} data-id={p.id}>
            <div class="head">
              <strong>{agentName(p.agent)} · {kinds[p.kind] ?? p.kind}「{p.title}」</strong>
              <span class="ph {p.phase}">{phases[p.phase] ?? p.phase}</span>
            </div>
            {#if p.question}
              <p class="q">问：{p.question}</p>
            {:else if p.why}
              <p class="why">{p.why}</p>
            {/if}
            {#if p.at}<p class="muted small">{ago(p.at * 1000)} · 编号 {p.number}</p>{/if}
            <div class="act">
              {#if p.question}
                <input type="text" maxlength="2000" placeholder="你的回答" bind:value={answers[key(p)]} data-testid="pending-input" />
                <button class="btn sm primary" disabled={busy === key(p) || !(answers[key(p)] ?? '').trim()} onclick={() => act(p, 'answer')} data-testid="pending-answer">回答</button>
              {/if}
              <button class="btn sm" disabled={busy === key(p)} onclick={() => act(p, 'seen')} data-testid="pending-seen">知道了</button>
              <button class="btn sm" onclick={() => openItem(p)} data-testid="pending-open">打开</button>
            </div>
          </article>
        {/each}
        <button class="more" onclick={() => { all = !all; if (all) void loadAll(); }} data-testid="pending-all-toggle">{all ? '收起全部承诺' : '全部承诺'}</button>
        {#if all}
          <div class="all" data-testid="pending-all">
            {#each groups as g (g.ph)}
              <section data-phase={g.ph}>
                <h4>{phases[g.ph] ?? g.ph} · {g.list.length}</h4>
                {#each g.list as it (key(it))}
                  <button class="line" onclick={() => openItem(it)}>{agentName(it.agent)} · {kinds[it.kind] ?? it.kind}「{it.title}」{#if it.why}<span class="muted"> — {it.why}</span>{/if}</button>
                {/each}
              </section>
            {:else}
              <p class="muted">还没有承诺。</p>
            {/each}
          </div>
        {/if}
      </div>
    {/if}
  </div>
{/if}

<style>
  .pending { position: absolute; left: 18px; top: 82px; z-index: 5; max-width: min(460px, calc(100% - 36px)); }
  .badge { padding: 5px 12px; border-radius: 999px; border: 1px solid var(--line-2); background: var(--panel); color: var(--text); cursor: pointer; backdrop-filter: blur(10px); font-size: 12.5px; }
  .badge b { color: #fff; background: var(--warn, #d9822b); border-radius: 999px; padding: 0 7px; margin-left: 4px; }
  .badge.on { border-color: var(--accent-2); }
  .card { margin-top: 8px; max-height: 60vh; overflow: auto; padding: 10px 12px; border-radius: 14px; background: var(--panel); border: 1px solid var(--line-2); box-shadow: var(--shadow); backdrop-filter: blur(10px); }
  .row { padding: 8px 0; border-bottom: 1px solid var(--line); }
  .head { display: flex; gap: 8px; justify-content: space-between; align-items: baseline; }
  .ph { font-size: 11px; padding: 1px 8px; border-radius: 999px; background: var(--line); white-space: nowrap; }
  .ph.blocked { background: color-mix(in srgb, var(--warn, #d9822b) 30%, transparent); }
  .ph.ready { background: color-mix(in srgb, var(--accent-2) 28%, transparent); }
  .why, .q { margin: 4px 0 0; font-size: 12.5px; color: var(--text-2); }
  .q { color: var(--text); }
  .muted { color: var(--muted); }
  .small { font-size: 11px; margin: 3px 0 0; }
  .act { display: flex; gap: 6px; margin-top: 6px; flex-wrap: wrap; }
  .act input { flex: 1; min-width: 140px; }
  .err { color: var(--err, #d64545); font-size: 12.5px; margin: 0 0 6px; }
  .more { margin-top: 8px; background: none; border: 0; color: var(--accent-2); cursor: pointer; font-size: 12.5px; padding: 0; }
  .all h4 { margin: 10px 0 4px; font-size: 12px; color: var(--muted); }
  .line { display: block; width: 100%; text-align: left; background: none; border: 0; padding: 3px 0; cursor: pointer; color: var(--text); font-size: 12.5px; }
</style>
