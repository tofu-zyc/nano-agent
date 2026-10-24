<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { homeShort } from '../../lib/format';
  import FieldGroup from '../FieldGroup.svelte';

  const skills = $derived(store.ov?.skills);
</script>

<div class="pnl">
  <p class="p-note lead">技能是一份份说明书（SKILL.md 和附带的脚本、资料），放在网关上，所有内核共用。模型看到技能列表，用到时再读全文。</p>
  <div class="p-card">
    {#each skills?.list ?? [] as sk (sk.name)}
      <div class="skill">
        <code>{sk.name}</code>
        <span>{sk.description}</span>
      </div>
    {:else}
      <p class="p-note">还没有技能。</p>
    {/each}
    <p class="p-note p-small">目录：<code>{homeShort(skills?.dir ?? '')}</code>。每个技能一个子目录，改动下一轮自动生效；增删技能在新会话、/reset 或整理上下文之后出现在已有的会话里。附带文件在调用时放到用到的机器的 <code>~/.nano-agent/skills/</code> 下。</p>
  </div>
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
