#include "job_manager.h"
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

struct managed_job {
    job_manager *manager;
    char id[97],action[32],execution[16];
    cJSON *model,*published,*pending,*terminal,*start_frame;
    uint64_t revision,request_id;
    atomic_uint_fast64_t cursor;
    atomic_bool gap,observed_start,observed_exit;
    atomic_int_fast64_t started_at,started_mono;
    int timeout_seconds;
    atomic_flag observation_lock;
    int64_t created_mono,yield_ms;
    bool created,spawn_ready,spawned,started_reported,process_done,terminal_saved,auto_reported,cancel_requested,removing,removed;
    char *failure_status,*failure_detail;
    char tail[8192];
    size_t tail_length;
};
struct job_manager {
    job_io *io;
    char *epoch;
    uint64_t sequence;
    na_wake *wake;
    managed_job *jobs[JOB_IO_RECORDS];
    size_t count;
    job_event events[JOB_IO_QUEUE];
    size_t event_head,event_count;
};
static const char *str(const cJSON *m,const char *key)
{
    return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(m,key));
}
static void set_string(cJSON *m,const char *key,const char *value)
{
    cJSON_DeleteItemFromObjectCaseSensitive(m,key);
    cJSON_AddStringToObject(m,key,value?value:"");
}
static void set_number(cJSON *m,const char *key,double value)
{
    cJSON_DeleteItemFromObjectCaseSensitive(m,key);cJSON_AddNumberToObject(m,key,value);
}
static void set_bool(cJSON *m,const char *key,bool value)
{
    cJSON_DeleteItemFromObjectCaseSensitive(m,key);cJSON_AddBoolToObject(m,key,value);
}
static bool terminal_status(const char *status)
{
    return status && (!strcmp(status,"succeeded") || !strcmp(status,"failed") ||
        !strcmp(status,"cancelled") || !strcmp(status,"timed_out") ||
        !strcmp(status,"interrupted") || !strcmp(status,"unknown"));
}
static void observe_lock(managed_job *j)
{
    while(atomic_flag_test_and_set_explicit(&j->observation_lock,memory_order_acquire)){}
}
static void observe_unlock(managed_job *j)
{
    atomic_flag_clear_explicit(&j->observation_lock,memory_order_release);
}
static managed_job *allocate(job_manager *m,cJSON *model)
{
    if(m->count==JOB_IO_RECORDS)return NULL;
    managed_job *j=xcalloc(1,sizeof *j);
    j->manager=m;j->model=model;
    snprintf(j->id,sizeof j->id,"%s",str(model,"job_id"));
    atomic_init(&j->cursor,0);atomic_init(&j->gap,false);
    atomic_init(&j->observed_start,false);atomic_init(&j->observed_exit,false);
    atomic_init(&j->started_at,0);atomic_init(&j->started_mono,0);
    atomic_flag_clear(&j->observation_lock);
    m->jobs[m->count++]=j;return j;
}
static bool emit(job_manager *m,managed_job *j,const char *kind,bool ok,const char *error,
                 uint64_t request_id,cJSON *data)
{
    if(m->event_count==JOB_IO_QUEUE){cJSON_Delete(data);return false;}
    job_event e={.job=j,.ok=ok,.request_id=request_id,.data=data};
    snprintf(e.id,sizeof e.id,"%s",j?j->id:"");
    snprintf(e.kind,sizeof e.kind,"%s",kind);
    snprintf(e.error,sizeof e.error,"%s",error?error:"");
    m->events[(m->event_head+m->event_count)%JOB_IO_QUEUE]=e;m->event_count++;
    return true;
}
static void runtime_fields(managed_job *j,cJSON *meta)
{
    set_number(meta,"output_cursor",(double)atomic_load(&j->cursor));
    set_bool(meta,"output_gap",atomic_load(&j->gap));
    if(atomic_load(&j->observed_start)) {
        int64_t start=atomic_load(&j->started_at);
        set_number(meta,"started_at",(double)start);
        set_number(meta,"deadline_at",j->timeout_seconds>0?(double)(start+j->timeout_seconds):0);
    }
    observe_lock(j);
    if(j->start_frame) {
        const cJSON *pid=cJSON_GetObjectItemCaseSensitive(j->start_frame,"pid");
        if(cJSON_IsNumber(pid))set_number(meta,"pid",pid->valuedouble);
        const char *sandbox=str(j->start_frame,"sandbox_effective");
        if(sandbox)set_string(meta,"sandbox_effective",sandbox);
    }
    buf_t text={0};buf_put_utf8(&text,j->tail,j->tail_length);
    set_string(meta,"output_tail",buf_cstr(&text));buf_free(&text);
    observe_unlock(j);
}
static bool commit(managed_job *j,const char *status,const char *action)
{
    if(j->pending)return false;
    cJSON *next=cJSON_Duplicate(j->model,true);
    set_string(next,"status",status);
    runtime_fields(j,next);
    set_number(next,"revision",(double)(j->revision+1));
    if(!job_io_meta(j->manager->io,j->id,j->revision+1,action,next)){cJSON_Delete(next);return false;}
    j->revision++;j->pending=next;
    snprintf(j->action,sizeof j->action,"%s",action);
    return true;
}
job_manager *jobs_open(const char *root,const char *epoch,na_wake *wake,char *error,size_t n)
{
    job_manager *m=xcalloc(1,sizeof *m);m->epoch=xstrdup(epoch);m->wake=wake;
    cJSON *records=NULL;
    m->io=job_io_open(root,wake,&records,error,n);
    if(!m->io){free(m->epoch);free(m);return NULL;}
    const cJSON *item;
    cJSON_ArrayForEach(item,records) {
        managed_job *j=allocate(m,cJSON_Duplicate(item,true));
        if(!j)break;
        j->published=cJSON_Duplicate(item,true);j->created=true;
        j->terminal_saved=terminal_status(str(item,"status"));
        const cJSON *cursor=cJSON_GetObjectItemCaseSensitive(item,"output_cursor");
        if(cJSON_IsNumber(cursor) && cursor->valuedouble>=0 && cursor->valuedouble<=9007199254740991.0)
            atomic_store(&j->cursor,(uint64_t)cursor->valuedouble);
        const cJSON *revision=cJSON_GetObjectItemCaseSensitive(item,"revision");
        if(cJSON_IsNumber(revision) && revision->valuedouble>=0)j->revision=(uint64_t)revision->valuedouble;
        atomic_store(&j->gap,cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(item,"output_gap")));
        const char *tail=str(item,"output_tail");
        if(tail) {
            size_t length=strlen(tail);
            if(length>sizeof j->tail)tail+=length-sizeof j->tail,length=sizeof j->tail;
            memcpy(j->tail,tail,length);j->tail_length=length;
        }
    }
    cJSON_Delete(records);return m;
}
managed_job *jobs_create(job_manager *m,const cJSON *origin,const cJSON *request,
                         const char *shared,const char *exclusive,int wait_kind,
                         char *error,size_t n)
{
    if(m->count==JOB_IO_RECORDS){snprintf(error,n,"Job 记录已达上限 %d",JOB_IO_RECORDS);return NULL;}
    const char *agent=str(origin,"agent"),*sid=str(origin,"sid");
    if(!agent || !*agent || !sid || !*sid){snprintf(error,n,"Job 缺少固定 agent 或来源会话身份");return NULL;}
    const cJSON *args=cJSON_GetObjectItemCaseSensitive(request,"args");
    const char *execution=str(args,"execution");
    if(!execution)execution="foreground";
    if(strcmp(execution,"foreground") && strcmp(execution,"background") && strcmp(execution,"auto")) {
        snprintf(error,n,"execution 只能是 foreground、background 或 auto");return NULL;
    }
    const cJSON *time_limit=cJSON_GetObjectItemCaseSensitive(args,"timeout");
    const cJSON *maximum=cJSON_GetObjectItemCaseSensitive(request,"shell_timeout_max");
    int maximum_seconds=cJSON_IsNumber(maximum) && maximum->valuedouble>=1 && maximum->valuedouble<=86400*365
                        ? maximum->valueint:86400;
    int timeout=1800;
    if(time_limit) {
        if(!cJSON_IsNumber(time_limit) || time_limit->valuedouble<0 || time_limit->valuedouble>86400*365 ||
           time_limit->valuedouble!=(double)time_limit->valueint) {
            snprintf(error,n,"Job timeout 需为 0 或正整数秒");return NULL;
        }
        timeout=time_limit->valueint;
    }
    if(timeout==0 && !cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(request,"allow_no_timeout"))) {
        snprintf(error,n,"执行节点未允许不限时 Job");return NULL;
    }
    if(timeout>maximum_seconds)timeout=maximum_seconds;
    cJSON *model=cJSON_CreateObject();
    char id[97];snprintf(id,sizeof id,"j_%s-%llu",m->epoch,(unsigned long long)++m->sequence);
    cJSON_AddNumberToObject(model,"version",1);cJSON_AddStringToObject(model,"job_id",id);
    cJSON_AddStringToObject(model,"node_epoch",m->epoch);
    cJSON_AddStringToObject(model,"owner_agent",agent);cJSON_AddStringToObject(model,"origin_sid",sid);
    cJSON_AddItemToObject(model,"origin",cJSON_Duplicate(origin,true));
    cJSON_AddStringToObject(model,"origin_run_id",str(origin,"run_id")?str(origin,"run_id"):"");
    cJSON_AddStringToObject(model,"origin_call_id",str(origin,"call_id")?str(origin,"call_id"):"");
    cJSON_AddStringToObject(model,"created_by",str(origin,"created_by")?str(origin,"created_by"):"");
    cJSON_AddStringToObject(model,"status","preparing");
    cJSON_AddStringToObject(model,"execution",execution);
    cJSON_AddStringToObject(model,"res_keys",shared?shared:"");
    cJSON_AddStringToObject(model,"res_xkeys",exclusive?exclusive:"");
    cJSON_AddNumberToObject(model,"wait_kind",wait_kind);
    cJSON_AddNumberToObject(model,"created_at",(double)time(NULL));
    cJSON_AddBoolToObject(model,"background",false);
    cJSON_AddItemToObject(model,"request",cJSON_Duplicate(request,true));
    for(const char *const *key=(const char *const[]){"wait_id","wait_sid","lease",NULL};*key;key++) {
        const cJSON *value=cJSON_GetObjectItemCaseSensitive(request,*key);
        if(value)cJSON_AddItemToObject(model,*key,cJSON_Duplicate(value,true));
    }
    cJSON *lease=cJSON_GetObjectItemCaseSensitive(model,"lease");
    if(cJSON_IsObject(lease))set_string(lease,"job_id",id);
    managed_job *j=allocate(m,model);
    snprintf(j->execution,sizeof j->execution,"%s",execution);
    j->created_mono=now_ms();j->yield_ms=10000;j->timeout_seconds=timeout;
    set_number(j->model,"timeout_seconds",timeout);
    const cJSON *yield=cJSON_GetObjectItemCaseSensitive(args,"yield_after_ms");
    if(cJSON_IsNumber(yield) && yield->valuedouble>=100 && yield->valuedouble<=600000)j->yield_ms=yield->valueint;
    cJSON *frozen=cJSON_GetObjectItemCaseSensitive(j->model,"request");
    set_number(frozen,"stream",1);set_string(frozen,"job_id",j->id);
    cJSON *frozen_args=cJSON_GetObjectItemCaseSensitive(frozen,"args");
    if(!cJSON_IsObject(frozen_args)) {
        cJSON_DeleteItemFromObjectCaseSensitive(frozen,"args");
        frozen_args=cJSON_AddObjectToObject(frozen,"args");
    }
    set_number(frozen_args,"timeout",timeout);
    if(!commit(j,"queued","created")) {
        snprintf(error,n,"Job 元数据没有可靠受理，未启动命令");
        m->count--;cJSON_Delete(model);free(j);return NULL;
    }
    return j;
}
bool jobs_prepare_start(managed_job *j)
{
    if(!j->created || j->pending || j->spawned || j->process_done || j->terminal_saved || j->cancel_requested)return false;
    return commit(j,"starting","start");
}
bool jobs_spawn_ready(const managed_job *j){return j->spawn_ready && !j->spawned && !j->process_done && !j->cancel_requested;}
void jobs_did_spawn(managed_job *j){j->spawn_ready=false;j->spawned=true;}
void jobs_observe(void *value,const cJSON *frame,const void *data,size_t n)
{
    managed_job *j=value;
    if(n) {
        uint64_t cursor=atomic_fetch_add(&j->cursor,n);
        int stream=str(frame,"stream") && !strcmp(str(frame,"stream"),"stderr")?2:1;
        if(!job_io_output(j->manager->io,j->id,cursor,stream,data,n))atomic_store(&j->gap,true);
        observe_lock(j);
        const char *p=data;
        if(n>=sizeof j->tail){memcpy(j->tail,p+n-sizeof j->tail,sizeof j->tail);j->tail_length=sizeof j->tail;}
        else {
            size_t keep=j->tail_length;
            if(keep+n>sizeof j->tail) {
                size_t drop=keep+n-sizeof j->tail;memmove(j->tail,j->tail+drop,keep-drop);keep-=drop;
            }
            memcpy(j->tail+keep,data,n);j->tail_length=keep+n;
        }
        observe_unlock(j);return;
    }
    const char *type=str(frame,"t");
    if(type && !strcmp(type,"started")) {
        observe_lock(j);cJSON_Delete(j->start_frame);j->start_frame=cJSON_Duplicate(frame,true);observe_unlock(j);
        atomic_store(&j->started_at,(int64_t)time(NULL));atomic_store(&j->started_mono,now_ms());
        atomic_store(&j->observed_start,true);
    }
    if(type && !strcmp(type,"exit")) {
        observe_lock(j);cJSON_Delete(j->terminal);j->terminal=cJSON_Duplicate(frame,true);observe_unlock(j);
        atomic_store(&j->observed_exit,true);
    }
    if(j->manager->wake)plat_wake_signal(j->manager->wake);
}
void jobs_cancelling(managed_job *j){j->cancel_requested=true;j->spawn_ready=false;}
void jobs_process_finished(managed_job *j){j->process_done=true;j->spawn_ready=false;}
void jobs_fail(managed_job *j,const char *status,const char *detail)
{
    free(j->failure_status);free(j->failure_detail);
    j->failure_status=xstrdup(status);j->failure_detail=xstrdup(detail?detail:"");
    jobs_process_finished(j);
}
bool jobs_detach(managed_job *j,uint64_t request_id,char *error,size_t n)
{
    if(jobs_background(j))return true;
    if(j->cancel_requested || j->process_done || atomic_load(&j->observed_exit) || j->terminal_saved) {
        snprintf(error,n,"任务已结束，不能再转后台");return false;
    }
    if(!j->created || j->pending){snprintf(error,n,"任务状态正在保存，稍后可重试后台化");return false;}
    cJSON *old=cJSON_Duplicate(j->model,true);
    set_bool(j->model,"background",true);
    const char *status=str(j->model,"status");
    bool accepted=commit(j,status?status:"queued","detached");
    if(!accepted){cJSON_Delete(j->model);j->model=old;snprintf(error,n,"后台所有权没有可靠受理，保持前台执行");}
    else {cJSON_Delete(old);j->request_id=request_id;}
    return accepted;
}
void jobs_detach_denied(managed_job *j){j->auto_reported=true;}
bool jobs_acknowledge(managed_job *j,uint64_t seq)
{
    if(!j->terminal_saved || j->pending || j->removing || j->removed || seq!=1)return false;
    if(str(j->model,"notification_state") && !strcmp(str(j->model,"notification_state"),"delivered"))return true;
    cJSON *old=cJSON_Duplicate(j->model,true);
    set_string(j->model,"notification_state","delivered");
    bool accepted=commit(j,str(j->model,"status"),"ack");
    if(!accepted){cJSON_Delete(j->model);j->model=old;}else cJSON_Delete(old);
    return accepted;
}
bool jobs_background(const managed_job *j){return j->published && cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(j->published,"background"));}
bool jobs_finished(const managed_job *j){return j->terminal_saved;}
bool jobs_created(const managed_job *j){return j->created;}
const char *jobs_id(const managed_job *j){return j->id;}
const cJSON *jobs_request(const managed_job *j){return cJSON_GetObjectItemCaseSensitive(j->model,"request");}
cJSON *jobs_snapshot(const managed_job *j)
{
    cJSON *out=cJSON_Duplicate(j->published?j->published:j->model,true);
    // Only committed background ownership/status are presented as published.
    runtime_fields((managed_job *)j,out);
    return out;
}
managed_job *jobs_find(job_manager *m,const char *id)
{
    for(size_t i=0;i<m->count;i++)if(!strcmp(m->jobs[i]->id,id))return m->jobs[i];
    return NULL;
}
bool jobs_read(managed_job *j,uint64_t cursor,size_t bytes,uint64_t request_id)
{
    return j->created && !j->removing && !j->removed && job_io_read(j->manager->io,j->id,cursor,bytes,request_id);
}
static void terminal_commit(managed_job *j)
{
    const char *status="unknown";
    observe_lock(j);
    cJSON *result=j->terminal?cJSON_Duplicate(j->terminal,true):NULL;
    observe_unlock(j);
    if(j->failure_status) {
        status=j->failure_status;
        set_string(j->model,"error",j->failure_detail);
    } else if(result) {
        if(cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(result,"cancelled")))status="cancelled";
        else if(cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(result,"timed_out")))status="timed_out";
        else status=cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(result,"ok"))?"succeeded":"failed";
        cJSON_DeleteItemFromObjectCaseSensitive(j->model,"result");
        cJSON_AddItemToObject(j->model,"result",result);result=NULL;
    } else set_string(j->model,"error","执行器未回报完整终态，不能确认命令结果");
    cJSON_Delete(result);
    set_number(j->model,"ended_at",(double)time(NULL));
    set_string(j->model,"notification_state","pending");
    set_number(j->model,"last_event_seq",1);
    commit(j,status,"terminal");
}
static void pump(job_manager *m,int64_t now)
{
    job_io_result saved;
    while(m->event_count<JOB_IO_QUEUE-1 && job_io_poll(m->io,&saved)) {
        managed_job *j=jobs_find(m,saved.job_id);
        if(j && !strcmp(saved.kind,"remove")) {
            j->removed=saved.ok;j->removing=false;
            emit(m,j,"removed",saved.ok,saved.error,saved.request_id,NULL);
        } else if(!strcmp(saved.kind,"read")) {
            if(j && saved.data) {
                set_number(saved.data,"output_cursor",(double)atomic_load(&j->cursor));
                if(atomic_load(&j->gap))set_bool(saved.data,"gap",true);
            }
            emit(m,j,"output",saved.ok,saved.error,saved.request_id,saved.data);saved.data=NULL;
        } else if(j && !strcmp(saved.kind,"output")) {
            atomic_store(&j->gap,true);
            emit(m,j,"log_error",false,saved.error,0,NULL);
        } else if(j && j->pending && saved.revision==j->revision) {
            if(saved.ok) {
                if(saved.data) {cJSON_Delete(j->pending);j->pending=saved.data;saved.data=NULL;}
                if(cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(j->pending,"output_gap")))atomic_store(&j->gap,true);
                cJSON_Delete(j->model);j->model=j->pending;j->pending=NULL;
                cJSON_Delete(j->published);j->published=cJSON_Duplicate(j->model,true);
                if(!strcmp(j->action,"created"))j->created=true;
                if(!strcmp(j->action,"start"))j->spawn_ready=true;
                if(!strcmp(j->action,"terminal"))j->terminal_saved=true;
            } else {
                cJSON_Delete(j->pending);j->pending=NULL;
                if(j->published){cJSON_Delete(j->model);j->model=cJSON_Duplicate(j->published,true);}
                else j->process_done=true;
            }
            emit(m,j,j->action,saved.ok,saved.error,j->request_id,jobs_snapshot(j));
            j->request_id=0;j->action[0]=0;
        }
        job_io_result_free(&saved);
    }
    for(size_t i=0;i<m->count && m->event_count<JOB_IO_QUEUE-2;i++) {
        managed_job *j=m->jobs[i];
        if(atomic_load(&j->observed_start) && !j->started_reported) {
            if(emit(m,j,"started",true,NULL,0,NULL))j->started_reported=true;
        }
        if(j->created && !j->pending && !j->terminal_saved) {
            if(j->process_done)terminal_commit(j);
            else if(j->cancel_requested) {
                if(strcmp(str(j->model,"status"),"cancelling"))commit(j,"cancelling","cancelling");
            }
            else if(atomic_load(&j->observed_start) && strcmp(str(j->model,"status"),"running"))
                commit(j,"running","running");
            else if(!j->auto_reported && !jobs_background(j) &&
                    (!strcmp(j->execution,"background") || (!strcmp(j->execution,"auto") && now-j->created_mono>=j->yield_ms))) {
                if(emit(m,j,"detach_ready",true,NULL,0,NULL))j->auto_reported=true;
            }
        }
    }
}
bool jobs_poll(job_manager *m,int64_t now,job_event *event)
{
    pump(m,now);
    if(!m->event_count)return false;
    *event=m->events[m->event_head];
    m->event_head=(m->event_head+1)%JOB_IO_QUEUE;m->event_count--;
    return true;
}
size_t jobs_count(const job_manager *m){return m->count;}
bool jobs_removable(const managed_job *j,double *created_at)
{
    const char *notification=str(j->model,"notification_state"),*status=str(j->model,"status");
    if(!j->terminal_saved || j->pending || j->removing || j->removed ||
       !notification || strcmp(notification,"delivered") || !status || !strcmp(status,"unknown"))return false;
    const cJSON *created=cJSON_GetObjectItemCaseSensitive(j->model,"created_at");
    if(created_at)*created_at=cJSON_IsNumber(created)?created->valuedouble:0;
    return true;
}
bool jobs_remove(managed_job *j,uint64_t request_id)
{
    if(!jobs_removable(j,NULL) || !job_io_remove(j->manager->io,j->id,request_id))return false;
    j->removing=true;return true;
}
static void free_job(managed_job *j)
{
    cJSON_Delete(j->model);cJSON_Delete(j->published);cJSON_Delete(j->pending);cJSON_Delete(j->terminal);cJSON_Delete(j->start_frame);
    free(j->failure_status);free(j->failure_detail);free(j);
}
bool jobs_forget(managed_job *j)
{
    if(!j->removed || j->pending)return false;
    job_manager *m=j->manager;
    for(size_t i=0;i<m->count;i++)if(m->jobs[i]==j) {
        memmove(m->jobs+i,m->jobs+i+1,(m->count-i-1)*sizeof m->jobs[0]);
        m->count--;free_job(j);return true;
    }
    return false;
}
int64_t jobs_next_deadline(job_manager *m)
{
    int64_t next=-1;
    for(size_t i=0;i<m->count;i++) {
        managed_job *j=m->jobs[i];
        if(!j->created || j->pending || j->auto_reported || j->process_done || j->terminal_saved || j->cancel_requested ||
           strcmp(j->execution,"auto"))continue;
        int64_t deadline=j->created_mono+j->yield_ms;
        if(next<0 || deadline<next)next=deadline;
    }
    return next;
}
cJSON *jobs_list(job_manager *m)
{
    cJSON *list=cJSON_CreateArray();
    for(size_t i=0;i<m->count;i++)if(m->jobs[i]->created)cJSON_AddItemToArray(list,jobs_snapshot(m->jobs[i]));
    return list;
}
void jobs_event_free(job_event *event){cJSON_Delete(event->data);event->data=NULL;}
void jobs_close(job_manager *m)
{
    if(!m)return;
    job_io_close(m->io);
    for(size_t i=0;i<m->count;i++) {
        managed_job *j=m->jobs[i];
        free_job(j);
    }
    for(size_t i=0;i<m->event_count;i++)jobs_event_free(&m->events[(m->event_head+i)%JOB_IO_QUEUE]);
    free(m->epoch);free(m);
}
