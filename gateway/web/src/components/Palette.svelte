<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { score } from '../lib/fuzzy';
  import { ago, channelMeta, truncate } from '../lib/format';
  import type { FieldState, Target } from '../lib/types';
  import { homeOf } from '../lib/nodes';
  import Icon from './Icon.svelte';

  type Item = { id: string; icon: string; title: string; sub?: string; kind: string; text: string; color?: string; run: () => void };

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

  function node(id: string, t: Target, icon: string, color: string, title: string, sub: string, text: string): Item {
    return { id, icon, color, title, sub, kind: '节点', text, run: () => { store.openOnMap(t); close(); } };
  }

  const items = $derived.by(() => {
    const out: Item[] = [
      { id: 'v:topology', icon: 'graph', title: '拓扑', sub: '所有东西的状态、操作和设置都在节点上', kind: '页面', text: '拓扑 首页 topology graph 设置 settings', run: go('#/') },
      { id: 'v:sessions', icon: 'chat', title: '会话', sub: '所有对话，网页聊天', kind: '页面', text: '会话 对话 聊天 sessions chat', run: go('#/sessions') },
      node('n:gateway', { type: 'gateway' }, 'hub', 'var(--c-gateway)', '网关', '版本和更新、重启、代理、改过的设置', '网关 版本 更新 升级 重启 代理 update restart proxy'),
      node('n:agent', { type: 'agentDefaults' }, 'persona', 'var(--c-agent)', 'Agent 默认值', '所有 agent 没覆盖的设置：上限、权限、显示档位', 'agent 默认值 人格 系统提示词 prompt 上限 权限 管理员 显示'),
      node('n:agents', { type: 'agents' }, 'plus', 'var(--c-agent)', '添加 agent', '多一个一直存在的 agent，可以绑自己的机器人', 'agent 新建 添加 多个 bot 机器人'),
      node('n:rooms', { type: 'rooms' }, 'plus', 'var(--c-agent)', '创建群', '网关内部的群：选几个 agent 进去，它们在那儿互相说话', '群 房间 room 群聊 多 agent 私聊 dm'),
      // 每个 agent、每个群一个条目：⌘K 里能直接搜到名字
      ...store.agents.map((a) => node('n:agent:' + a.id, { type: 'agent', id: a.id } as Target, 'persona', 'var(--c-agent)',
        (a.emoji ? a.emoji + ' ' : '') + a.name, a.default ? '默认 agent' : 'agent · ' + a.id + ' · ' + a.sessions + ' 个会话',
        'agent ' + a.name + ' ' + a.id + ' 人格 模型 机器人')),
      ...store.rooms.map((r) => node('n:room:' + r.id, { type: 'room', id: r.id } as Target, 'chat', 'var(--accent-2)',
        '群 · ' + r.name, r.members.length + ' 个 agent' + (r.paused ? ' · 已暂停' : '') + ' · ' + r.count + ' 条',
        '群 房间 room ' + r.name)),
      node('n:model', { type: 'model' }, 'sparkles', 'var(--c-model)', '模型', '接口、模型、默认模型、请求参数', '模型 接口 model endpoint'),
      node('n:search', { type: 'search' }, 'search', 'var(--c-search)', '网络搜索', '开关、key、今天搜了几次', '网络搜索 联网 上网 搜索 web search deepseek'),
      node('n:memory', { type: 'memory' }, 'brain', 'var(--c-memory)', '上下文与记忆', '自动整理、空闲整理、归档', '上下文 记忆 整理 压缩 归档 memory context compact'),
      node('n:skills', { type: 'skills' }, 'book', 'var(--c-skill)', '技能', '技能列表和目录', '技能 skill'),
      node('n:schedules', { type: 'schedules' }, 'clock', 'var(--c-cron)', '定时任务', '查看、新建、编辑、立即运行', '定时任务 cron schedule 提醒'),
      node('n:machines', { type: 'machines' }, 'laptop', 'var(--c-kernel)', '接入机器', '别的电脑装内核连进来、添加 SSH 服务器、确认配对', '设备 电脑 服务器 安装 配对 SSH Windows Linux 连接 devices'),
      node('n:workspaces', { type: 'workspace' }, 'folder', 'var(--c-dir)', '工作目录', '全部工作目录、添加', '工作目录 目录 workspace'),
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
        text: `${m?.label} ${c.name} 渠道 机器人 bot`, run: () => { store.openOnMap({ type: 'channel', name: c.name }); close(); },
      });
    }
    for (const s of store.ov?.sessions ?? []) {
      out.push({
        id: 's:' + s.sid, icon: s.sid === 'main' ? 'hub' : 'chat', color: s.sid === 'main' ? 'var(--accent)' : channelMeta[s.channel]?.color, title: s.title, sub: `${s.sid} · ${s.last_at ? ago(s.last_at * 1000) : '新会话'}`, kind: '会话',
        text: `${s.title} ${s.sid} 会话`, run: go('#/sessions/' + encodeURIComponent(s.sid)),
      });
    }
    for (const f of store.fields) {
      const g = homeOf(f);
      out.push({
        id: 'f:' + f.key, icon: g.icon, color: g.color, title: f.label, sub: `${g.label}${f.section ? ' · ' + f.section : ''} · ${truncate(valuePreview(f), 40)}`, kind: '设置',
        text: `${f.label} ${f.key} ${g.label} ${f.section ?? ''} ${f.help ?? ''} ${f.keywords ?? ''}`,
        run: () => {
          store.openOnMap(g.target, f.key);
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
      <input bind:this={input} bind:value={q} onkeydown={onkey} placeholder="搜索设置、节点、会话、操作……比如“代理”、“上限”、“token”" />
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
