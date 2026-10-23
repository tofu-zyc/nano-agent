<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { score } from '../lib/fuzzy';
  import { ago, channelMeta, truncate } from '../lib/format';
  import type { FieldState, Target } from '../lib/types';
  import Icon from './Icon.svelte';

  type Item = { id: string; icon: string; title: string; sub?: string; kind: string; text: string; color?: string; run: () => void };

  const groupInfo: Record<string, { label: string; target: Target; icon: string; color: string }> = {
    context: { label: '上下文与采样', target: { type: 'gateway' }, icon: 'layers', color: 'var(--c-model)' },
    gateway: { label: '网关', target: { type: 'gateway' }, icon: 'hub', color: 'var(--c-gateway)' },
    kernel: { label: '内核', target: { type: 'kernel' }, icon: 'shield', color: 'var(--c-kernel)' },
    workspace: { label: '工作目录与沙箱', target: { type: 'workspace' }, icon: 'folder', color: 'var(--c-dir)' },
    cli: { label: '命令行', target: { type: 'channel', name: 'cli' }, icon: 'terminal', color: 'var(--c-cli)' },
    telegram: { label: 'Telegram', target: { type: 'channel', name: 'telegram' }, icon: 'send', color: 'var(--c-telegram)' },
    feishu: { label: '飞书', target: { type: 'channel', name: 'feishu' }, icon: 'feishu', color: 'var(--c-feishu)' },
  };

  let q = $state('');
  let sel = $state(0);
  let input: HTMLInputElement;

  const close = () => (store.palette = false);
  const go = (h: string) => () => {
    location.hash = h;
    close();
  };

  function valuePreview(f: FieldState) {
    if (f.type === 'secret') return f.is_set ? '已设置' : '未设置';
    if (f.type === 'bool') return f.value ? '开' : '关';
    if (Array.isArray(f.value)) return f.value.length ? f.value.join('、') : '空';
    if (f.value === null || f.value === undefined || f.value === '') return '空';
    return String(typeof f.value === 'object' ? JSON.stringify(f.value) : f.value);
  }

  const items = $derived.by(() => {
    const out: Item[] = [
      { id: 'v:topology', icon: 'graph', title: '拓扑', sub: '网关、渠道、会话、模型、内核的全貌', kind: '页面', text: '拓扑 首页 topology graph', run: go('#/') },
      { id:'v:schedules',icon:'clock',title:'定时任务',sub:'查看下次运行、结果，创建和编辑任务',kind:'页面',text:'定时任务 cron schedule 提醒',run:go('#/schedules') },
      { id: 'v:sessions', icon: 'chat', title: '会话', sub: '所有对话，网页聊天', kind: '页面', text: '会话 对话 聊天 sessions chat', run: go('#/sessions') },
      { id:'v:devices',icon:'terminal',title:'设备与服务器',sub:'下载电脑启动包，添加和连接 SSH 服务器',kind:'页面',text:'设备 服务器 安装包 启动器 SSH Windows Linux 连接',run:go('#/devices') },
      { id: 'v:personas', icon: 'persona', title: '人格', sub: '编辑系统提示词', kind: '页面', text: '人格 系统提示词 persona prompt', run: go('#/personas') },
      { id: 'v:settings', icon: 'sliders', title: '全部设置', sub: '按对象分组的所有配置项', kind: '页面', text: '设置 配置 settings config', run: go('#/settings') },
      {
        id: 'a:test', icon: 'zap', title: '测试默认模型', sub: '发一个很小的请求，看通不通、多快', kind: '操作', text: '测试 模型 连接 test model ping',
        run: async () => {
          close();
          if (!(store.ov?.models?.models?.length)) {
            store.openOnMap({ type: 'model' });
            store.toast({ kind: 'err', text: '还没有配置模型，先添加接口和模型' }, 2600);
            return;
          }
          try {
            const r = await api.post<{ ok: boolean; error?: string; ms?: number }>('/api/test/model', { id: '' });
            if (!r.ok) throw new Error(r.error || '没通');
            store.toast({ kind: 'ok', text: `模型连接正常 · ${r.ms ?? '—'} ms` }, 2400);
          } catch (err) {
            store.fail(err);
          }
        },
      },
      {
        id: 'a:addmodel', icon: 'plus', title: '添加模型或接口', sub: '加一个 OpenAI 兼容接口；多模态模型勾“能接收图片”', kind: '操作', text: '添加 新建 模型 接口 endpoint vision 多模态 图片',
        run: () => {
          store.openModel('model');
          close();
        },
      },
      {
        id: 'a:main', icon: 'hub', title: '打开主会话', sub: '终端、网页和你的机器人私聊共用的对话', kind: '操作', text: '主会话 聊天 main chat',
        run: go('#/sessions/main'),
      },
      {
        id: 'a:newchat', icon: 'plus', title: '新建会话', sub: '开一段新的对话，上下文和设置都是独立的', kind: '操作', text: '新建 会话 聊天 new chat session',
        run: () => {
          close();
          store.newSession();
        },
      },
      {
        id: 'a:theme', icon: store.dark ? 'sun' : 'moon', title: store.dark ? '切换到浅色' : '切换到深色', kind: '操作', text: '主题 深色 浅色 theme dark light',
        run: () => {
          store.setTheme(store.dark ? 'light' : 'dark');
          close();
        },
      },
      {
        id: 'a:reload', icon: 'refresh', title: '重新读取配置文件', sub: '手动改过 gateway.toml 之后', kind: '操作', text: '重新读取 配置文件 reload gateway.toml',
        run: async () => {
          close();
          try {
            await api.post('/api/config/reload');
            store.toast({ kind: 'ok', text: '已重新读取配置文件' });
          } catch (err) {
            store.fail(err);
          }
        },
      },
      { id: 'a:logout', icon: 'logout', title: '退出登录', kind: '操作', text: '退出 登录 logout', run: () => { store.logout(); close(); } },
    ];
    for (const c of store.ov?.channels ?? []) {
      const m = channelMeta[c.name];
      out.push({
        id: 'c:' + c.name, icon: m?.icon ?? 'link', color: m?.color, title: m?.label ?? c.name, sub: c.status.identity ?? c.status.state, kind: '渠道',
        text: `${m?.label} ${c.name} 渠道 机器人 bot`, run: () => { store.open({ type: 'channel', name: c.name }); close(); },
      });
    }
    for (const s of store.ov?.sessions ?? []) {
      out.push({
        id: 's:' + s.sid, icon: s.sid === 'main' ? 'hub' : 'chat', color: s.sid === 'main' ? 'var(--accent)' : channelMeta[s.channel]?.color, title: s.title, sub: `${s.sid} · ${s.last_at ? ago(s.last_at * 1000) : '新会话'}`, kind: '会话',
        text: `${s.title} ${s.sid} 会话`, run: go('#/sessions/' + encodeURIComponent(s.sid)),
      });
    }
    for (const f of store.fields) {
      const g = groupInfo[f.group];
      if (!g || f.group === 'model') continue; // 模型的设置在模型节点面板
      out.push({
        id: 'f:' + f.key, icon: g.icon, color: g.color, title: f.label, sub: `${g.label}${f.section ? ' · ' + f.section : ''} · ${truncate(valuePreview(f), 40)}`, kind: '设置',
        text: `${f.label} ${f.key} ${g.label} ${f.section ?? ''} ${f.help ?? ''} ${f.keywords ?? ''}`,
        run: () => {
          if (f.group === 'context') location.hash = '#/settings?focus=' + encodeURIComponent(f.key);
          else store.open(g.target, f.key);
          close();
        },
      });
    }
    return out;
  });

  const results = $derived.by(() => {
    if (!q.trim()) return items.filter((i) => i.kind !== '设置').slice(0, 14);
    return items
      .map((i) => {
        const st = score(q, i.title);
        return { i, s: Math.max(st >= 0 ? st + 200 : -1, score(q, i.text)) };
      })
      .filter((x) => x.s > 0)
      .sort((a, b) => b.s - a.s)
      .slice(0, 40)
      .map((x) => x.i);
  });

  $effect(() => {
    q;
    sel = 0;
  });

  function onkey(e: KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      sel = Math.min(sel + 1, results.length - 1);
      scroll();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      sel = Math.max(sel - 1, 0);
      scroll();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      results[sel]?.run();
    }
  }

  function scroll() {
    queueMicrotask(() => document.querySelector('.pal .item.sel')?.scrollIntoView({ block: 'nearest' }));
  }

  $effect(() => {
    input?.focus();
  });
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="backdrop" onclick={close}>
  <div class="pal" onclick={(e) => e.stopPropagation()}>
    <div class="search">
      <Icon name="search" size={17} />
      <input bind:this={input} bind:value={q} onkeydown={onkey} placeholder="搜索设置、会话、操作……比如“代理”、“token”、“思考”" />
      <span class="kbd">Esc</span>
    </div>
    <div class="list">
      {#each results as it, i (it.id)}
        <button class="item" class:sel={i === sel} onmouseenter={() => (sel = i)} onclick={it.run}>
          <span class="ic" style="--c: {it.color ?? 'var(--accent)'}"><Icon name={it.icon} size={15} /></span>
          <span class="txt">
            <span class="t">{it.title}</span>
            {#if it.sub}<span class="s">{it.sub}</span>{/if}
          </span>
          <span class="k">{it.kind}</span>
        </button>
      {:else}
        <div class="empty">没有找到“{q}”相关的内容</div>
      {/each}
    </div>
    <div class="foot">
      <span><span class="kbd">↑</span><span class="kbd">↓</span> 选择</span>
      <span><span class="kbd">Enter</span> 打开</span>
      <span class="grow"></span>
      <span>{store.fields.length} 个配置项 · {store.ov?.sessions?.length ?? 0} 个会话</span>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 60;
    background: rgba(5, 7, 12, 0.5);
    backdrop-filter: blur(3px);
    display: flex;
    justify-content: center;
    padding-top: 12vh;
    animation: fade 0.15s ease;
  }
  @keyframes fade {
    from {
      opacity: 0;
    }
  }
  .pal {
    width: min(640px, calc(100vw - 32px));
    max-height: 66vh;
    display: flex;
    flex-direction: column;
    border-radius: 16px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow), 0 0 0 1px color-mix(in srgb, var(--accent) 15%, transparent);
    overflow: hidden;
    animation: rise 0.18s ease;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 11px;
    padding: 14px 16px;
    border-bottom: 1px solid var(--line);
    color: var(--accent-2);
  }
  .search input {
    flex: 1;
    border: none;
    outline: none;
    background: none;
    font-size: 15px;
    color: var(--text);
  }
  .list {
    flex: 1;
    overflow: auto;
    padding: 6px;
  }
  .item {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 10px;
    border: none;
    border-radius: 10px;
    background: none;
    text-align: left;
  }
  .item.sel {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .ic {
    width: 30px;
    height: 30px;
    flex: none;
    border-radius: 9px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 14%, transparent);
  }
  .txt {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .t {
    font-size: 13.5px;
    font-weight: 500;
  }
  .s {
    font-size: 12px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .k {
    font-size: 11px;
    color: var(--muted);
    padding: 2px 7px;
    border-radius: 6px;
    border: 1px solid var(--line);
  }
  .empty {
    padding: 30px;
    text-align: center;
    color: var(--muted);
  }
  .foot {
    display: flex;
    gap: 14px;
    align-items: center;
    padding: 8px 14px;
    border-top: 1px solid var(--line);
    font-size: 11.5px;
    color: var(--muted);
  }
  .foot .kbd {
    margin-right: 3px;
  }
  .grow {
    flex: 1;
  }
</style>
