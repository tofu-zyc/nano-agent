<script lang="ts">
  import { toolIcon } from '../lib/format';
  import { prettyArgs } from '../lib/conv';
  import type { Attachment, ExecutionIsolation } from '../lib/types';
  import Icon from './Icon.svelte';

  // media：工具读到的图片（交给模型看了）、SendFile 发出去的文件，一直显示在卡片下面
  let { name, detail, args = '', ok, result, running = false, media = [], sandbox, callState }: {
    callState?: string; sandbox?: ExecutionIsolation; name: string; detail: string; args?: string; ok?: boolean; result?: string; running?: boolean; media?: Attachment[];
  } = $props();
  const isImage = (a: Attachment) => /^image\/(png|jpeg|gif|webp)$/.test(a.mime);
  const sizeOf = (n: number) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`);
  let open = $state(false);
  const lines = $derived(result ? result.replace(/\n$/, '').split('\n').length : 0);
</script>

<div class="tool" class:bad={ok === false} class:open>
  <button class="head" onclick={() => (open = !open)}>
    <span class="ic"><Icon name={toolIcon[name] ?? 'zap'} size={14} /></span>
    <span class="name">{name}</span>
    <span class="detail">{detail}</span>
    {#if running && ok === undefined}
      {#if callState === 'waiting_approval'}<span class="lines">等待审批</span>{:else if callState === 'queued'}<span class="lines">排队中</span>{:else if callState === 'accepted'}<span class="lines">待调度</span>{:else}<span class="st run"><Icon name="refresh" size={13} /></span>{/if}
    {:else if ok === false}
      <span class="st err"><Icon name="x" size={13} /></span>
    {:else if ok}
      <span class="st ok"><Icon name="check" size={13} /></span>
    {/if}
    {#if lines > 0}<span class="lines">{lines} 行</span>{/if}
    <Icon name={open ? 'down' : 'right'} size={13} />
  </button>
  {#if media.length}
    <div class="media">
      {#each media as m, i (`${m.id}:${i}`)}
        {#if isImage(m)}
          <a class="pic" href="/api/uploads/{m.id}" target="_blank" rel="noopener" title={m.name}><img src="/api/uploads/{m.id}" alt={m.name ?? '图片'} loading="lazy" /></a>
        {:else}
          <a class="file" href="/api/uploads/{m.id}" target="_blank" rel="noopener"><Icon name="file" size={13} />{m.name || '文件'}<span>{sizeOf(m.size)}</span></a>
        {/if}
      {/each}
    </div>
  {/if}
  {#if open}
    <div class="more">
      {#if sandbox}
        <div class="lbl">执行隔离</div>
        <div>请求 {sandbox.requested || sandbox.mode || '未回报'} · 执行策略 {sandbox.mode || '未回报'} · 实际 {({full:'完整',partial:'部分',none:'无',unknown:'未回报'} as Record<string,string>)[sandbox.effective || 'unknown'] || sandbox.effective}{sandbox.reason ? ' · ' + sandbox.reason : ''}</div>
      {/if}
      {#if args}
        <div class="lbl">参数</div>
        <pre>{prettyArgs(args)}</pre>
      {/if}
      {#if result !== undefined}
        <div class="lbl">结果</div>
        <pre class="res">{result || '（空）'}</pre>
      {/if}
    </div>
  {/if}
</div>

<style>
  .media {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 0 12px 10px;
  }
  .media .pic img {
    display: block;
    max-width: 180px;
    max-height: 180px;
    border-radius: 10px;
    border: 1px solid var(--line);
    object-fit: contain;
    background: var(--panel-2);
  }
  .media .file {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    border-radius: 10px;
    border: 1px solid var(--line);
    font-size: 12.5px;
    color: var(--text);
  }
  .media .file span {
    color: var(--muted);
    font-size: 11.5px;
  }
  .tool {
    border-radius: 11px;
    border: 1px solid var(--line);
    background: color-mix(in srgb, var(--c-kernel) 4%, var(--panel-2));
    overflow: hidden;
    font-size: 12.5px;
    max-width: 100%;
  }
  .tool.bad {
    border-color: color-mix(in srgb, var(--err) 35%, var(--line));
  }
  .head {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    border: none;
    background: none;
    text-align: left;
    color: var(--text-2);
  }
  .ic {
    color: var(--c-kernel);
    display: grid;
  }
  .name {
    font-weight: 600;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
  }
  .detail {
    flex: 1;
    min-width: 0;
    font-family: var(--mono);
    font-size: 12px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .st {
    display: grid;
    place-items: center;
    width: 18px;
    height: 18px;
    border-radius: 50%;
  }
  .st.ok {
    color: var(--ok);
    background: color-mix(in srgb, var(--ok) 14%, transparent);
  }
  .st.err {
    color: var(--err);
    background: color-mix(in srgb, var(--err) 14%, transparent);
  }
  .st.run {
    color: var(--c-kernel);
    animation: spin 1s linear infinite;
  }
  .lines {
    font-size: 11px;
    color: var(--muted);
  }
  .more {
    padding: 0 10px 10px;
  }
  .lbl {
    font-size: 11px;
    color: var(--muted);
    margin: 4px 0;
  }
  pre {
    margin: 0;
    padding: 8px 10px;
    border-radius: 8px;
    background: var(--bg-2);
    font-size: 12px;
    line-height: 1.5;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 320px;
    overflow: auto;
  }
</style>
