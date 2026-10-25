<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import Icon from './Icon.svelte';

  // 扫码绑定 QQ / 微信机器人：网关向平台要一个二维码，用手机扫码确认后，网关自动加好机器人（重新扫码时更新它），
  // 扫码的人成为管理员。进展通过 bot_reg 事件推过来。
  let {
    kind,
    botId = '',
    name = '',
    agent = '',
    proxy = '',
    onDone,
  }: { kind: 'qq' | 'weixin'; botId?: string; name?: string; agent?: string; proxy?: string; onDone?: (id: string) => void } = $props();

  let phase = $state<'idle' | 'loading' | 'waiting' | 'done' | 'error'>('idle');
  let qr = $state('');
  let link = $state('');
  let scanned = $state(false);
  let needCode = $state(false);
  let wrongCode = $state(false);
  let code = $state('');
  let msg = $state('');
  let seen = 0; // 处理过的 bot_reg 事件

  const app = $derived(kind === 'qq' ? '手机 QQ' : '微信');
  const color = $derived(kind === 'qq' ? 'var(--c-qq)' : 'var(--c-weixin)');

  async function start() {
    phase = 'loading';
    msg = '';
    scanned = needCode = wrongCode = false;
    seen = store.botReg?.n ?? 0;
    try {
      const r = await api.post<{ url: string; qr_svg: string }>('/api/bots/register', { kind, id: botId, name: name.trim(), agent, proxy: proxy.trim() });
      qr = r.qr_svg;
      link = r.url;
      phase = 'waiting';
    } catch (err) {
      phase = 'error';
      msg = err instanceof Error ? err.message : String(err);
    }
  }

  async function cancel() {
    phase = 'idle';
    await api.del('/api/bots/register').catch(() => {});
  }

  async function sendCode() {
    if (!code.trim()) return;
    needCode = false;
    try {
      await api.post('/api/bots/register/verify', { code: code.trim() });
    } catch (err) {
      store.fail(err);
    }
    code = '';
  }

  // 网关通过事件告诉我们进展
  $effect(() => {
    const r = store.botReg;
    if (!r || r.n <= untrack(() => seen)) return;
    seen = r.n;
    if (untrack(() => phase) !== 'waiting') return;
    switch (r.kind) {
      case 'scanned':
        scanned = true;
        break;
      case 'verify':
      case 'verify_wrong':
        scanned = true;
        needCode = true;
        wrongCode = r.kind === 'verify_wrong';
        break;
      case 'refreshed':
        if (r.data?.qr_svg) qr = r.data.qr_svg;
        if (r.data?.url) link = r.data.url;
        scanned = needCode = false;
        break;
      case 'done':
        phase = 'done';
        msg = r.text;
        if (r.data?.id) onDone?.(r.data.id);
        break;
      case 'error':
        phase = 'error';
        msg = r.text;
        break;
    }
  });

  onDestroy(() => {
    if (phase === 'waiting') api.del('/api/bots/register').catch(() => {});
  });
</script>

<div class="qrbox" style="--c: {color}">
  {#if phase === 'idle' || phase === 'loading'}
    <div class="intro">
      <div class="ic"><Icon name={kind} size={18} /></div>
      <div>
        <strong>{botId ? '重新扫码' : '扫码' + (kind === 'qq' ? '创建机器人' : '连接微信')}</strong>
        {#if kind === 'qq'}
          <p>用手机 QQ 扫一下，在手机上新建（或者选一个已有的）机器人，AppID 和 AppSecret 自动填好。每个 QQ 号最多建 5 个机器人；扫码的人会成为管理员。</p>
        {:else}
          <p>用微信扫一下，连上你自己的微信 ClawBot（微信里的一个联系人）。只有扫码的你能和它聊，不能拉群；要较新版本的微信，鸿蒙版暂时不支持。</p>
        {/if}
      </div>
    </div>
    <button class="btn primary" onclick={start} disabled={phase === 'loading'}>
      <Icon name={phase === 'loading' ? 'refresh' : 'plus'} size={14} class={phase === 'loading' ? 'spin' : ''} />
      {phase === 'loading' ? '正在生成二维码…' : '生成二维码'}
    </button>
  {:else if phase === 'waiting'}
    <div class="qr">{@html qr}</div>
    {#if needCode}
      <p class="center">{wrongCode ? '数字不对，再看一下手机：' : '手机微信上显示了一串数字，填在这里：'}</p>
      <form class="code" onsubmit={(e) => { e.preventDefault(); sendCode(); }}>
        <input class="p-input" bind:value={code} inputmode="numeric" placeholder="手机上的数字" />
        <button class="btn sm primary" type="submit">确定</button>
      </form>
    {:else if scanned}
      <p class="center"><strong>扫好了</strong>，在手机上确认</p>
    {:else}
      <p class="center"><strong>用{app}扫一扫</strong>{kind === 'qq' ? '，在手机上创建机器人' : '，在手机上确认'}</p>
    {/if}
    <p class="center muted small">二维码过期会自动换新的 · 也可以 <a href={link} target="_blank" rel="noopener">在手机浏览器里打开</a></p>
    {#if !needCode}<div class="center"><span class="spinner"></span><span class="muted small">{scanned ? '等手机上确认…' : '等待扫码…'}</span></div>{/if}
    <button class="btn sm ghost" onclick={cancel}>取消</button>
  {:else if phase === 'done'}
    <div class="ok"><Icon name="check" size={16} />{botId ? '凭据更新好了' : `加好了「${msg}」`}，正在连接。</div>
    <p class="muted small">
      {#if kind === 'qq'}私聊这个机器人试试。要拉进群：群主在群设置里添加机器人（需要新版手机 QQ）。{:else}在微信里找到「微信ClawBot」，发条消息试试。{/if}
      扫码的你已经是管理员了。
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
    border: 1px solid color-mix(in srgb, var(--c) 35%, var(--line));
    background: color-mix(in srgb, var(--c) 6%, var(--panel-2));
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
    color: var(--c);
    background: color-mix(in srgb, var(--c) 16%, transparent);
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
    text-align: center;
  }
  .code {
    align-self: center;
    display: flex;
    gap: 8px;
  }
  .code .p-input {
    width: 140px;
    text-align: center;
    letter-spacing: 2px;
  }
  .small {
    font-size: 12px;
  }
  .spinner {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--c);
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
