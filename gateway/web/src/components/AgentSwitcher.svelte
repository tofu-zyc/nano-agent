<script lang="ts">
  // 对话 agent 选择器：聊天顶部和会话设置共用同一份校验和提示。
  // 改的是“谁来接着聊”，会话 ID、历史、窗口接线、会话级设置都不动。
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import type { SessionInfo } from '../lib/types';

  let { sid, compact = false }: { sid: string; compact?: boolean } = $props();
  const sess = $derived(store.session(sid));
  const agents = $derived(store.agents);
  const current = $derived(sess ? (store.agent(sess.agent) ?? undefined) : undefined);
  let saving = $state(false);
  let error = $state('');

  const blocked = $derived(sess?.switch_blocked ?? '');
  const disabled = $derived(!sess || !sess.can_switch_agent || saving);

  async function pick(ev: Event) {
    const el = ev.currentTarget as HTMLSelectElement;
    const id = el.value;
    if (!sess || !id || id === sess.agent || saving) return;
    const before = sess.agent;
    saving = true;
    error = '';
    try {
      // 用服务端返回的会话信息更新（不要只改本地）
      store.upsertSession(await api.patch<SessionInfo>(sidPath(sid), { agent: id }));
      const name = store.agent(id)?.name ?? id;
      store.toast({ kind: 'ok', text: `这个会话现在由「${name}」接着聊，聊天记录和历史都保留` }, 3000);
    } catch (err) {
      el.value = before;
      error = err instanceof Error ? err.message : String(err);
      store.fail(err);
    } finally {
      saving = false;
    }
  }
</script>

<div class="agent-switch" class:compact>
  {#if !compact}<span class="lbl">对话 agent</span>{/if}
  {#if sess?.agent_fixed}
    <span class="fixed" title="这是 agent 自己的内部会话（内心活动、整理记忆、定时任务或房间成员），身份固定">
      {current?.emoji ?? '🤖'} {current?.name ?? sess.agent}
      <em>固定</em>
    </span>
  {:else}
    <select
      class="pick"
      value={sess?.agent ?? ''}
      disabled={disabled}
      onchange={pick}
      title={blocked || '换一个 agent 接着聊：保留聊天记录，下一条消息按新 agent 的人格、模型、工具和记忆'}
      aria-label="这个会话用哪个 agent"
    >
      {#if !sess}<option value="">读取中…</option>{/if}
      {#each agents as ag (ag.id)}
        <option value={ag.id}>{ag.emoji ? ag.emoji + ' ' : ''}{ag.name}{ag.id === 'default' ? '（默认）' : ''}</option>
      {/each}
    </select>
  {/if}
  {#if saving}<span class="spinner"></span>{/if}
  {#if compact && blocked && !saving && !sess?.agent_fixed}
    <span class="why" title={blocked}>忙碌中</span>
  {/if}
</div>
{#if !compact && blocked && !saving}
  <p class="hint">{blocked}</p>
{/if}
{#if !compact && error}
  <p class="hint err">{error}</p>
{/if}

<style>
  .agent-switch {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .lbl {
    font-size: 12px;
    color: var(--muted);
    white-space: nowrap;
  }
  select.pick {
    height: 30px;
    max-width: 220px;
    padding: 0 8px;
    border-radius: 9px;
    border: 1px solid var(--line);
    background: var(--panel-solid);
    color: var(--text);
    font-size: 12.5px;
    cursor: pointer;
  }
  select.pick:disabled {
    opacity: 0.62;
    cursor: not-allowed;
  }
  .compact select.pick {
    height: 28px;
    max-width: 190px;
  }
  .fixed {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 12.5px;
    color: var(--text-2);
  }
  .fixed em {
    font-style: normal;
    font-size: 11px;
    color: var(--muted);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 1px 6px;
  }
  .why {
    font-size: 11px;
    color: var(--warn);
    white-space: nowrap;
  }
  .hint {
    margin: 6px 2px 0;
    font-size: 12px;
    color: var(--muted);
    line-height: 1.5;
  }
  .hint.err {
    color: var(--err);
  }
  .spinner {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--line);
    border-top-color: var(--accent);
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>