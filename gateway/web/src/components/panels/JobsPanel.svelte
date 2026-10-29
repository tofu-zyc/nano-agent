<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { api } from '../../lib/api';
  import { store } from '../../lib/state.svelte';
  import { finalJob, JobLog, jobKey, jobStates, mergeJob } from '../../lib/jobs';
  import type { JobInfo, JobResponse } from '../../lib/types';

  let { node, job_id }: { node?: string; job_id?: string } = $props();
  let rows = $state<JobInfo[]>([]);
  let selected = $state<JobInfo>();
  let next = $state('');
  let loading = $state(false);
  let busy = $state(false);
  let error = $state('');
  let note = $state('');
  let logText = $state('');
  let cursor = $state(0);
  let gap = $state(false);
  let log = new JobLog();
  let disposed = false;
  let generation = 0;
  let loadAgain = false;
  const updates = new Map<string, JobInfo>();
  let reloadTimer: ReturnType<typeof setTimeout>;
  // Only a bounded page is retained; events update visible rows, never an unbounded cache.
  async function load(more = false) {
    if (loading) { if (!more) loadAgain = true; return; }
    loading = true;
    error = '';
    try {
      const params = new URLSearchParams({ limit: '50' });
      if (node) params.set('node', node);
      if (more && next) params.set('after', next);
      const out = await api.get<JobResponse>('/api/jobs?' + params);
      if (disposed) return;
      const current = new Map(rows.map(j => [jobKey(j), j]));
      for (const [key, job] of updates) current.set(key, mergeJob(current.get(key), job));
      const page = (out.jobs ?? []).map(j => mergeJob(current.get(jobKey(j)), j));
      rows = (more ? [...rows, ...page.filter(j => !rows.some(r => jobKey(r) === jobKey(j)))] : page).slice(0, 200);
      next = out.next ?? '';
      if (selected) selected = rows.find(j => jobKey(j) === jobKey(selected!)) ?? selected;
    } catch (err) {
      if (!disposed) error = String(err instanceof Error ? err.message : err);
    } finally {
      loading = false;
      if (loadAgain && !disposed) { loadAgain = false; void load(); }
    }
  }
  function select(job: JobInfo) {
    generation++;
    selected = job;
    busy = false;
    error = '';
    note = '';
    log = new JobLog();
    logText = '';
    cursor = 0;
    gap = false;
  }
  async function act(action: string) {
    if (!selected || busy) return;
    const job = selected, token = generation;
    busy = true; error = ''; note = '';
    try {
      const out = await api.post<JobResponse>('/api/jobs/' + encodeURIComponent(job.job_id) + '/control',
        { node: job.node, action, cursor: log.cursor, max_bytes: 8192, wait_ms: 10000 });
      if (disposed || token !== generation) return;
      if (out.job) selected = mergeJob(selected, out.job);
      if (out.output) {
        log.append(out.output, finalJob(selected!.status));
        logText = log.text; cursor = log.cursor; gap = log.gap;
        if (out.output.error) error = out.output.error;
      }
      if (out.wait_expired) note = '任务仍未结束，可稍后查看。';
      if (out.accepted && (action === 'cancel' || action === 'detach')) note = '操作已受理，最终状态以执行节点保存后的回执为准。';
      if (selected) {
        const key = jobKey(selected);
        rows = rows.map(j => jobKey(j) === key ? mergeJob(j, selected!) : j);
      }
    } catch (err) {
      if (!disposed && token === generation) error = String(err instanceof Error ? err.message : err);
    } finally {
      if (!disposed && token === generation) busy = false;
    }
  }
  function update(job: JobInfo) {
    if (disposed || node && job.node !== node) return;
    const key = jobKey(job);
    updates.set(key, mergeJob(updates.get(key), job));
    if (updates.size > 200) updates.delete(updates.keys().next().value!);
    if (selected && jobKey(selected) === key) selected = mergeJob(selected, job);
    if (rows.some(j => jobKey(j) === key)) {
      rows = rows.map(j => jobKey(j) === key ? mergeJob(j, job) : j);
    } else if (!next) {
      clearTimeout(reloadTimer);
      reloadTimer = setTimeout(() => void load(), 250);
    }
  }
  $effect(() => {
    const job = store.lastJob;
    if (job) untrack(() => update(job));
  });
  onMount(() => {
    void load();
    const off = store.on(e => {
      if (e.t !== 'kernel') return;
      clearTimeout(reloadTimer);
      reloadTimer = setTimeout(() => void load(), 250);
    });
    if (job_id) {
      void api.post<JobResponse>('/api/jobs/' + encodeURIComponent(job_id) + '/control', { node, action: 'status' })
        .then(out => { if (!disposed && out.job && !selected) select(out.job); })
        .catch(err => { if (!disposed) error = String(err.message ?? err); });
    }
    return () => { disposed = true; generation++; clearTimeout(reloadTimer); off(); updates.clear(); };
  });
</script>

<div class="toolbar">
  <button class="btn sm" disabled={loading} onclick={() => load()}>{loading ? '正在读取…' : '刷新任务'}</button>
  <span class="muted">{rows.length} 条{node ? ' · ' + node : ''}</span>
</div>
{#if error}<p class="error" role="alert">{error}</p>{/if}
<div class="list">
  {#each rows as job (jobKey(job))}
    <button class="job" class:chosen={selected && jobKey(selected) === jobKey(job)} onclick={() => select(job)}>
      <span><strong>{jobStates[job.status] ?? job.status}</strong> · {job.background ? '后台' : '前台'}{job.silent ? ' · 安静' : ''}</span>
      <span class="muted">{job.job_id}</span>
      <span class="muted">{job.node} · {job.agent} · {job.origin_sid}</span>
    </button>
  {:else}
    {#if !loading}<p class="muted">还没有可查看的执行任务。</p>{/if}
  {/each}
</div>
{#if next && rows.length < 200}
  <button class="btn sm" disabled={loading} onclick={() => load(true)}>更多任务</button>
{:else if next}
  <p class="muted">当前显示 200 条任务，可刷新列表或通过任务编号直接打开。</p>
{/if}

{#if selected}
  <section class="detail">
    <h4>{jobStates[selected.status] ?? selected.status}{!selected.online ? ' · 节点离线' : ''}</h4>
    <dl>
      <dt>任务</dt><dd>{selected.job_id}</dd>
      <dt>来源</dt><dd>{selected.agent} · {selected.origin_sid}</dd>
      <dt>调用</dt><dd>{selected.origin_run_id} / {selected.origin_turn_id} / {selected.origin_call_id}</dd>
      <dt>执行</dt><dd>{selected.node} · {selected.background ? '后台' : '前台'} · {selected.execution}</dd>
      <dt>实际隔离</dt><dd>{selected.sandbox_effective || '尚无执行回执'}</dd>
    </dl>
    {#if selected.silent}<p class="muted">安静任务：状态更新不会切换聊天窗口或向主聊天推送。</p>{/if}
    {#if selected.status === 'unknown'}<p class="error">执行结果未知，相关执行名额和资源占用仍保留。</p>{/if}
    {#if selected.error}<p class="error">{selected.error}</p>{/if}
    <div class="actions">
      <button class="btn sm" disabled={busy} onclick={() => act('status')}>查询状态</button>
      <button class="btn sm" disabled={busy || !selected.online} onclick={() => act('output')}>读取日志</button>
      <button class="btn sm" disabled={busy || finalJob(selected.status)} onclick={() => act('wait')}>等待最多 10 秒</button>
      <button class="btn sm" disabled={busy || !selected.online || finalJob(selected.status) || selected.background} onclick={() => act('detach')}>转后台</button>
      <button class="btn sm" disabled={busy || !selected.online || finalJob(selected.status)} onclick={() => act('cancel')}>取消任务</button>
    </div>
    {#if busy}<p class="muted" role="status">正在处理…</p>{/if}
    {#if note}<p class="muted" role="status">{note}</p>{/if}
    {#if gap || selected.output_gap}<p class="error">部分旧日志已轮转或未保存，以下输出可能不完整。</p>{/if}
    <p class="muted">日志游标 {cursor} · 本页最多保留 128K 字符</p>
    {#if logText}<pre aria-label="任务输出">{logText}</pre>{/if}
  </section>
{/if}

<style>
  .toolbar, .actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
  .list { display: grid; gap: 6px; margin: 12px 0; }
  .job { display: grid; gap: 3px; text-align: left; padding: 9px; background: var(--panel); border: 1px solid var(--line); border-radius: 9px; overflow-wrap: anywhere; }
  .job.chosen { border-color: var(--accent); }
  .muted { color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
  .error { color: var(--err); overflow-wrap: anywhere; }
  .detail { border-top: 1px solid var(--line); margin-top: 16px; padding-top: 8px; }
  dl { display: grid; grid-template-columns: auto 1fr; gap: 6px 10px; font-size: 12px; }
  dt { color: var(--muted); } dd { margin: 0; overflow-wrap: anywhere; }
  pre { max-height: 440px; overflow: auto; padding: 10px; background: var(--bg-2); border-radius: 8px; font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
