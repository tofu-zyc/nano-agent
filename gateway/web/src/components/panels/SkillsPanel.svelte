<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { homeShort } from '../../lib/format';
  import FieldGroup from '../FieldGroup.svelte';

  const skills = $derived(store.ov?.skills);
  // dream、life 是网关自己用的方法（整理记忆、拟人型醒来做什么），不给模型当技能调用
  const reserved = ['dream', 'life'];
  const normal = $derived((skills?.list ?? []).filter((s) => !reserved.includes(s.name)));
  const own = $derived((skills?.list ?? []).filter((s) => reserved.includes(s.name)));
</script>

<div class="pnl">
  <p class="p-note lead">技能是一份份说明书（SKILL.md 和附带的脚本、资料），放在网关上，所有内核共用。模型看到技能列表，用到时再读全文。</p>
  <div class="p-card">
    {#each normal as sk (sk.name)}
      <div class="skill">
        <code>{sk.name}</code>
        <span>{sk.description}</span>
      </div>
    {:else}
      <p class="p-note">还没有技能。</p>
    {/each}
    <p class="p-note p-small">目录：<code>{homeShort(skills?.dir ?? '')}</code>。每个技能一个子目录，改动下一轮自动生效；增删技能会自动更新已有会话的技能目录，不必清空聊天。配套文件在调用时放到各执行机器的独立只读缓存，AI 按技能回执里的实际路径读取。</p>
  </div>
  {#if own.length}
    <h5 class="p-h">网关自己用的方法 <span class="muted p-small">不出现在模型的技能列表里</span></h5>
    <div class="p-card">
      {#each own as sk (sk.name)}
        <div class="skill">
          <code>{sk.name}</code>
          <span>{sk.name === 'dream' ? '怎么整理记忆：agent 睡着、空闲或到点时照着它把最近的聊天整理进长期记忆。' : '拟人型 agent 一个人的时候做什么：醒来时照着它冲浪、想事情、找人聊天。'}改这个文件就是改所有 agent 的做法；只想改某一个，放到 <code>agents/&lt;id&gt;/skills/{sk.name}/SKILL.md</code>。</span>
        </div>
      {/each}
    </div>
  {/if}
  <FieldGroup group="skills" focus={store.focus} />
</div>

<style>
  .skill {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 7px 10px;
    border-radius: 9px;
    background: var(--panel-solid);
    font-size: 12.5px;
  }
  .skill code {
    color: var(--c-skill);
  }
  .skill span {
    color: var(--text-2);
    line-height: 1.5;
  }
</style>
