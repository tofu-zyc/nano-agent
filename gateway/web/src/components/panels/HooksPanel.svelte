<script lang="ts">
 import {onMount,untrack} from 'svelte';
 import {api} from '../../lib/api';
 import {store} from '../../lib/state.svelte';
 import type {HookRule,HookSpec,HookDelivery,HookResponse} from '../../lib/types';
 let {agent:initialAgent}: {agent?:string}=$props();
 let agent=$state(untrack(()=>initialAgent??'default')), sid=$state('main');
 let rules=$state<HookRule[]>([]),runs=$state<HookDelivery[]>([]),selected=$state<HookRule>();
 let editing=$state(false),busy=$state(false),error=$state(''),next=$state('');
 let name=$state(''),events=$state('job.completed, job.failed'),kind=$state<'notify'|'command'|'agent'>('notify');
 let conversation=$state<'temporary'|'origin'|'life'|'branch'>('temporary'),text=$state(''),job=$state(''),branch=$state('');
 let once=$state(true),enabled=$state(true),deliver=$state<'creator'|'none'>('creator');
 let decision=$state<'allow'|'deny'|'approval'>('deny');
 let deadline=$state(''),debounce=$state(500),cooldown=$state(1000),timeout=$state(5000),budget=$state(0),advanced=$state('{}');
 let operation=crypto.randomUUID(),disposed=false,sequence=0;
 const labels:Record<string,string>={pending:'等待',running:'运行中',retry:'等待重试',succeeded:'成功',failed:'失败',uncertain:'结果不明',cancelled:'已撤回'};
 const sources=$derived((store.ov?.sessions??[]).filter(s=>s.agent===agent&&!s.sid.includes('hook:')));
 const agents=$derived(store.ov?.agents??[]);
 const beforeOnly=$derived(events.split(',').map(x=>x.trim()).filter(Boolean).join()==='tool.before');
 const type=$derived(beforeOnly?'command':kind);
 async function load(){
  const token=++sequence;error='';
  try{
   const query=new URLSearchParams({agent,sid});
   const out=await api.get<HookResponse>('/api/hooks?'+query);
   if(disposed||token!==sequence)return;
   rules=out.rules??[];
   if(selected&&!editing)selected=rules.find(r=>r.id===selected!.id);
  }catch(e){if(!disposed)error=String(e instanceof Error?e.message:e)}
 }
 async function records(more=false){
  try{
   const out=await api.get<HookResponse>('/api/hooks?'+new URLSearchParams({action:'runs',agent,sid,id:selected?.id??'',after:more?next:''}));
   if(disposed)return;
   runs=(more?[...runs,...out.deliveries??[]]:out.deliveries??[]).slice(0,200);next=out.next??'';
  }catch(e){error=String(e instanceof Error?e.message:e)}
 }
 function edit(rule?:HookRule){
  selected=rule;editing=true;error='';operation=crypto.randomUUID();
  const s=rule?.spec;
  name=s?.name??'';events=s?.on.join(', ')??'job.completed, job.failed';kind=s?.handler.type??'notify';
  conversation=s?.handler.conversation??'temporary';text=s?.handler.text??s?.handler.command??s?.handler.prompt??'';
  branch=s?.handler.branch_sid??'';job=s?.match?.job_id??'';once=rule?.once??true;enabled=rule?.enabled??true;decision=s?.handler.decision??'deny';
  deliver=s?.deliver==='none'?'none':'creator';sid=s?.scope?.source_sid??sources[0]?.sid??'main';
  deadline=s?.deadline_at?new Date(s.deadline_at-new Date().getTimezoneOffset()*60000).toISOString().slice(0,16):'';
  debounce=s?.debounce_ms??500;cooldown=s?.cooldown_ms??1000;timeout=s?.handler.timeout_ms??(kind==='agent'?300000:5000);budget=s?.handler.token_budget??0;
  const extra=s?{...s,handler:{...s.handler},scope:s.scope?{mode:s.scope.mode,workspaces:s.scope.workspaces}:undefined}:{};
  advanced=JSON.stringify(extra,null,2);
 }
 async function action(action:string,rule:HookRule){
  if(busy)return;busy=true;error='';
  try{
   await api.post<HookResponse>('/api/hooks',{action,id:rule.id,revision:rule.revision,agent,sid:rule.spec.scope?.source_sid??sid,rearm:action==='enable'&&!!rule.claimed_by});
   await load();await records();
  }catch(e){error=String(e instanceof Error?e.message:e)}finally{busy=false}
 }
 async function save(){
  if(busy)return;busy=true;error='';
  try{
   const extra=JSON.parse(advanced) as Partial<HookSpec>;
   const spec:HookSpec={...extra,name,on:events.split(',').map(x=>x.trim()).filter(Boolean),
    handler:{...extra.handler,type,conversation,timeout_ms:timeout,token_budget:budget,
     ...(type==='notify'?{text}:type==='command'?{command:text||undefined}:{prompt:text}),branch_sid:branch||undefined,
     ...(beforeOnly?{decision}:{})},
    match:{...extra.match,job_id:job||undefined},debounce_ms:debounce,cooldown_ms:cooldown,deliver,
    deadline_at:deadline?new Date(deadline).getTime():undefined};
   if(!beforeOnly)delete spec.handler.decision;
   if(type!=='command'){delete spec.handler.command;delete spec.handler.script;delete spec.handler.digest}
   if(type!=='notify')delete spec.handler.text;
   if(type!=='agent')delete spec.handler.prompt;
   await api.post('/api/hooks',{action:selected?'update':'create',id:selected?.id,revision:selected?.revision,operation_id:operation,agent,sid,spec,once,enabled});
   editing=false;await load();
  }catch(e){error=String(e instanceof Error?e.message:e)}finally{busy=false}
 }
 onMount(()=>{
  void load();
  const off=store.on(e=>{if(e.t==='hooks'&&e.name===agent){void load();if(runs.length)void records()}});
  return()=>{disposed=true;off()};
 });
</script>
<div class="hooks">
 <p class="muted">发生事件后处理。通知不调用模型；独立对话按固定权限运行。安静分支的结果留在分支中。</p>
 <div class="row">
  <label>Agent<select bind:value={agent} onchange={()=>{selected=undefined;editing=false;sid=sources[0]?.sid??'main';void load()}}>
   {#each agents as a}<option value={a.id}>{a.name}</option>{/each}
   {#if !agents.length}<option value="default">默认</option>{/if}
  </select></label>
  <button onclick={()=>edit()}>新建规则</button><button onclick={()=>void load()} disabled={busy}>刷新</button>
 </div>
 {#if error}<p role="alert" class="error">{error}</p>{/if}
 {#if editing}
  <form onsubmit={e=>{e.preventDefault();void save()}}>
   <label>名称<input bind:value={name} maxlength="120" required/></label>
   <label>来源会话<select bind:value={sid} disabled={!!selected}>{#each sources as s}<option value={s.sid}>{s.title||s.sid}</option>{/each}{#if !sources.some(s=>s.sid===sid)}<option value={sid}>{sid}</option>{/if}</select></label>
   <label>事件（逗号分隔）<input bind:value={events} required list="hook-events"/></label>
   <datalist id="hook-events"><option value="job.completed, job.failed"></option><option value="deadline.at"></option><option value="device.online"></option><option value="file.changed"></option><option value="tool.before"></option><option value="mind.threshold_crossed"></option></datalist>
   <label>只匹配任务编号（可选）<input bind:value={job}/></label>
   {#if events.includes('deadline.at')}<label>提醒时间<input type="datetime-local" bind:value={deadline} required/></label>{/if}
   {#if beforeOnly}
    <p class="muted">前置检查在执行前同步运行，固定使用短命令，最多 2 秒；只能追加拒绝或审批，不能放宽原来的权限与审批。</p>
    <label>判断<select bind:value={decision}><option value="deny">拒绝 deny</option><option value="allow">放行 allow</option><option value="approval">要求审批 approval</option></select></label>
   {:else}
    <label>处理方式<select bind:value={kind} onchange={()=>{timeout=kind==='agent'?300000:5000}}><option value="notify">固定通知</option><option value="command">短命令</option><option value="agent">Agent 处理</option></select></label>
   {/if}
   {#if type==='agent'}
    <label>运行场合<select bind:value={conversation}><option value="temporary">独立临时对话</option><option value="origin">原会话队列</option><option value="life">新内心片段</option><option value="branch">持久安静分支</option></select></label>
    {#if conversation==='branch'}<label>已有分支（空表示创建一次）<input bind:value={branch}/></label>{/if}
    <label>本次 token 子预算（0 不限，仍受总预算约束）<input type="number" min="0" bind:value={budget}/></label>
   {/if}
   <label>{beforeOnly?'命令（可选；标准输入为事件 JSON，标准输出 {"decision":"allow|deny|approval"}）':type==='notify'?'通知模板':type==='command'?'命令（标准输入为事件 JSON）':'任务说明'}<textarea rows="5" bind:value={text} maxlength="2000" required={!beforeOnly}></textarea></label>
   <label>结果投递<select bind:value={deliver}><option value="creator">登记时创建者的原窗口</option><option value="none">仅留记录</option></select></label>
   <div class="row"><label>去抖毫秒<input type="number" min="0" max="60000" bind:value={debounce}/></label><label>冷却毫秒<input type="number" min="0" max="86400000" bind:value={cooldown}/></label></div>
   <label>{beforeOnly?'命令上限毫秒（前置最多 2 秒）':'处理上限毫秒'}<input type="number" min="1" max="600000" bind:value={timeout}/></label>
   <label><input type="checkbox" bind:checked={once}/>成功后停用</label><label><input type="checkbox" bind:checked={enabled}/>启用</label>
   <details><summary>高级配置：字段匹配、脚本、观察路径、权限上限和重试</summary><p class="muted">以下 JSON 保留额外字段；上方表单覆盖同名字段。脚本变更后必须重新保存规则以登记摘要。</p><textarea rows="12" bind:value={advanced}></textarea></details>
   <div class="row"><button type="submit" disabled={busy}>保存</button><button type="button" onclick={()=>editing=false}>取消</button></div>
  </form>
 {/if}
 {#each rules as rule (rule.id)}
  <article>
   <strong>{rule.spec.name||rule.id}</strong><span class="tag">{rule.silent?'安静 · ':''}{rule.enabled?'启用':'停用'} · 修订 {rule.revision}</span>
   <p>{rule.spec.on.join('、')} → {rule.spec.handler.type} {rule.spec.handler.type==='agent'?rule.spec.handler.conversation??'temporary':''}{rule.spec.handler.decision?' · '+rule.spec.handler.decision:''}</p>
   <p class="muted">{rule.spec.scope?.source_sid} · {rule.creator}{rule.once?' · 一次性':''}</p>
   {#if rule.paused_reason}<p class="error">已暂停：{rule.paused_reason}</p>{/if}
   <div class="row"><button onclick={()=>edit(rule)}>编辑</button><button disabled={busy} onclick={()=>void action(rule.enabled?'disable':'enable',rule)}>{rule.enabled?'停用':rule.claimed_by?'重新启用一次':'启用'}</button><button onclick={()=>{selected=rule;void records()}}>投递记录</button><button disabled={busy} onclick={()=>void action('delete',rule)}>删除</button></div>
  </article>
 {/each}
 {#if !rules.length&&!editing}<p class="muted">还没有规则。可以用固定通知提醒任务完成，或让 agent 在独立场合处理。</p>{/if}
 <div class="row"><button onclick={()=>{selected=undefined;void records()}}>查看全部投递</button></div>
 {#each runs as run (run.delivery_id)}
  <article><strong>{labels[run.status]??run.status}</strong><span> · 第 {run.attempt} 次</span>{#if run.preflight}<span class="tag">前置检查</span>{/if}<p class="muted">{run.hook_id} · {run.delivery_id.slice(-12)}</p>{#if run.run_sid}<button onclick={()=>store.go('sessions/'+encodeURIComponent(run.run_sid!))}>查看运行对话</button>{/if}{#if run.error}<p class="error">{run.error}</p>{/if}{#if run.result}<pre>{run.result}</pre>{/if}</article>
 {/each}
 {#if next}<button onclick={()=>void records(true)} disabled={runs.length>=200}>继续读取</button>{/if}
</div>
<style>
 .hooks{display:grid;gap:14px}.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}label{display:grid;gap:6px;font-size:13px}form{display:grid;gap:12px;padding:12px;border:1px solid var(--line);border-radius:10px}input,select,textarea{background:var(--panel);color:var(--text);border:1px solid var(--line);border-radius:7px;padding:8px;max-width:100%;box-sizing:border-box}textarea{width:100%;resize:vertical}button{background:var(--panel);color:var(--text);border:1px solid var(--line);border-radius:7px;padding:7px 9px;cursor:pointer}button:disabled{opacity:.5}article{border-bottom:1px solid var(--line);padding:10px 0}p{margin:7px 0;overflow-wrap:anywhere}.muted{color:var(--text-3);font-size:12px}.error{color:var(--danger)}pre{white-space:pre-wrap;font-size:12px;overflow-wrap:anywhere}.tag{margin-left:8px;font-size:11px;color:var(--text-3)}
</style>
