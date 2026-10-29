<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Report } from '../lib/types';

  let { report }: { report: Report } = $props();
  const at = $derived(report.at ? new Date(report.at * 1000).toLocaleString() : '');
</script>

<div class="report-card">
  <div class="head">
    <Icon name="hub" size={15} />
    <span class="title">{report.title || report.branch_sid}</span>
    <span class="tag">分支报告</span>
    {#if report.status}<span class="muted small">{report.status}</span>{/if}
  </div>
  <p class="conclusion">{report.conclusion}</p>
  {#if report.evidence?.length}
    <ul class="evidence">
      {#each report.evidence as e, i (i)}<li>{e}</li>{/each}
    </ul>
  {/if}
  <div class="foot">
    <span class="muted small">
      来源会话：{report.parent_sid || '—'}{#if report.checkpoint} · 分叉点第 {report.checkpoint} 条{/if}{#if at} · {at}{/if}
    </span>
    {#if report.branch_sid}
      <a class="btn sm" href="#/sessions/{encodeURIComponent(report.branch_sid)}" title="只打开分支查看，不改绑任何聊天窗口">
        <Icon name="chat" size={13} />打开分支
      </a>
    {/if}
  </div>
</div>

<style>
  .report-card {
    border: 1px solid var(--line);
    border-left: 3px solid var(--accent);
    border-radius: 12px;
    padding: 10px 12px;
    background: var(--panel-2);
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .title {
    font-weight: 600;
  }
  .tag {
    font-size: 11px;
    padding: 1px 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent) 18%, transparent);
    color: var(--accent);
  }
  .conclusion {
    margin: 0;
    white-space: pre-wrap;
  }
  .evidence {
    margin: 0;
    padding-left: 18px;
    color: var(--text-3);
    font-size: 12px;
  }
  .foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }
</style>
