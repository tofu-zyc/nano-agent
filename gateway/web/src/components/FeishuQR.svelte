<script lang="ts">
  import { onDestroy } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import Icon from './Icon.svelte';

  // 扫码创建飞书机器人：网关向飞书要一个注册二维码，用飞书扫码确认后，
  // 网关自动拿到 App ID / Secret、写进配置、启动飞书渠道。
  let { configured = false }: { configured?: boolean } = $props();

  let phase = $state<'idle' | 'loading' | 'waiting' | 'done' | 'error'>('idle');
  let qr = $state('');
  let link = $state('');
  let left = $state(0);
  let msg = $state('');
  let timer = 0;

  async function start() {
    if (configured && !confirm('已经配置了飞书机器人。重新扫码会创建一个新的机器人，并替换现在的 App ID 和 Secret，继续吗？')) return;
    phase = 'loading';
    msg = '';
    store.feishuReg = null;
    try {
      const r = await api.post<{ url: string; qr_svg: string; expire_in: number }>('/api/feishu/register');
      qr = r.qr_svg;
      link = r.url;
      left = r.expire_in;
      phase = 'waiting';
      clearInterval(timer);
      timer = window.setInterval(() => {
        left = Math.max(0, left - 1);
        if (left === 0) {
          clearInterval(timer);
          if (phase === 'waiting') {
            phase = 'error';
            msg = '二维码过期了，请重新生成';
          }
        }
      }, 1000);
    } catch (err) {
      phase = 'error';
      msg = err instanceof Error ? err.message : String(err);
    }
  }

  async function cancel() {
    clearInterval(timer);
    phase = 'idle';
    await api.del('/api/feishu/register').catch(() => {});
  }

  // 网关通过事件告诉我们结果
  $effect(() => {
    const r = store.feishuReg;
    if (!r || phase !== 'waiting') return;
    clearInterval(timer);
    if (r.kind === 'done') {
      phase = 'done';
      msg = r.text;
    } else {
      phase = 'error';
      msg = r.text;
    }
  });

  onDestroy(() => clearInterval(timer));
  const mmss = $derived(`${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`);
</script>

<div class="qrbox">
  {#if phase === 'idle' || phase === 'loading'}
    <div class="intro">
      <div class="ic"><Icon name="zap" size={18} /></div>
      <div>
        <strong>扫码创建机器人</strong>
        <p>用飞书扫一下，确认后自动创建一个机器人应用，凭证自动填好，不用去开放平台手动配置。</p>
      </div>
    </div>
    <button class="btn primary" onclick={start} disabled={phase === 'loading'}>
      <Icon name={phase === 'loading' ? 'refresh' : 'plus'} size={14} class={phase === 'loading' ? 'spin' : ''} />
      {phase === 'loading' ? '正在生成二维码…' : configured ? '重新扫码创建' : '生成二维码'}
    </button>
  {:else if phase === 'waiting'}
    <div class="qr">{@html qr}</div>
    <p class="center"><strong>用飞书扫一扫</strong>，在手机上确认创建</p>
    <p class="center muted small">{mmss} 后过期 · 也可以 <a href={link} target="_blank" rel="noopener">在浏览器里打开</a></p>
    <div class="center"><span class="spinner"></span><span class="muted small">等待扫码确认…</span></div>
    <button class="btn sm ghost" onclick={cancel}>取消</button>
  {:else if phase === 'done'}
    <div class="ok"><Icon name="check" size={16} />创建好了（App ID {msg}），飞书渠道正在连接。</div>
    <p class="muted small">
      私聊机器人试试。如果没反应，到<a href="https://open.feishu.cn/app" target="_blank" rel="noopener">飞书开放平台</a>看一下这个应用：
      事件订阅方式要是“长连接”，并且已经发布了版本。
    </p>
  {:else}
    <div class="bad"><Icon name="alert" size={15} />{msg}</div>
    <button class="btn" onclick={start}><Icon name="refresh" size={14} />重新生成</button>
  {/if}
</div>

<style>
  .qrbox {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 10px;
    padding: 14px;
    margin-top: 12px;
    border-radius: 14px;
    border: 1px solid color-mix(in srgb, var(--c-feishu) 35%, var(--line));
    background: color-mix(in srgb, var(--c-feishu) 6%, var(--panel-2));
  }
  .intro {
    display: flex;
    gap: 11px;
  }
  .intro .ic {
    width: 34px;
    height: 34px;
    flex: none;
    border-radius: 10px;
    display: grid;
    place-items: center;
    color: var(--c-feishu);
    background: color-mix(in srgb, var(--c-feishu) 16%, transparent);
  }
  .intro p {
    margin: 3px 0 0;
    font-size: 12.5px;
    color: var(--text-2);
    line-height: 1.55;
  }
  .qr {
    align-self: center;
    width: 210px;
    height: 210px;
    padding: 6px;
    border-radius: 12px;
    background: #fff;
  }
  .qr :global(svg) {
    width: 100%;
    height: 100%;
    display: block;
  }
  .center {
    align-self: center;
    display: flex;
    gap: 8px;
    align-items: center;
    margin: 0;
    font-size: 13px;
  }
  .small {
    font-size: 12px;
  }
  .spinner {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--c-feishu);
    border-top-color: transparent;
    animation: spin 0.8s linear infinite;
  }
  .btn.ghost {
    align-self: center;
  }
  .ok,
  .bad {
    display: flex;
    gap: 7px;
    align-items: center;
    font-size: 13px;
  }
  .ok {
    color: var(--ok);
  }
  .bad {
    color: var(--err);
  }
  p.muted {
    margin: 0;
    line-height: 1.6;
  }
</style>
