#include "job_io.h"
#include <errno.h>
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#if NA_WINDOWS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#else
#include <pthread.h>
#endif

typedef struct {
    char id[97],kind[32];
    int type,stream;
    uint64_t revision,cursor,request_id;
    size_t size,limit;
    char *data;
} work;
struct job_io {
    char *root;
    na_wake *wake;
    work queue[JOB_IO_QUEUE];
    size_t head,count,bytes,high_water;
    uint64_t dropped;
    job_io_result results[JOB_IO_QUEUE];
    size_t result_head,result_count;
    bool closing;
    char known[JOB_IO_RECORDS][97];
    size_t known_count;
    bool log_gap[JOB_IO_RECORDS];
#if NA_WINDOWS
    CRITICAL_SECTION mu;
    CONDITION_VARIABLE cond;
    HANDLE thread;
#else
    pthread_mutex_t mu;
    pthread_cond_t cond;
    pthread_t thread;
#endif
};
static void lock(job_io *io) {
#if NA_WINDOWS
    EnterCriticalSection(&io->mu);
#else
    pthread_mutex_lock(&io->mu);
#endif
}
static void unlock(job_io *io) {
#if NA_WINDOWS
    LeaveCriticalSection(&io->mu);
#else
    pthread_mutex_unlock(&io->mu);
#endif
}
static void signal_work(job_io *io) {
#if NA_WINDOWS
    WakeAllConditionVariable(&io->cond);
#else
    pthread_cond_broadcast(&io->cond);
#endif
}
static void wait_work(job_io *io) {
#if NA_WINDOWS
    SleepConditionVariableCS(&io->cond,&io->mu,INFINITE);
#else
    pthread_cond_wait(&io->cond,&io->mu);
#endif
}
static bool valid_id(const char *id)
{
    if(!id || strncmp(id,"j_",2) || strlen(id)>96 || !id[2])return false;
    for(const unsigned char *p=(const unsigned char *)id;*p;p++)
        if(!((*p>='a' && *p<='z') || (*p>='A' && *p<='Z') || (*p>='0' && *p<='9') || *p=='_' || *p=='-'))return false;
    return true;
}
static const char *str(const cJSON *m,const char *key)
{
    return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(m,key));
}
static cJSON *parse_json(const char *data,size_t size)
{
    const char *end=NULL;
    cJSON *m=cJSON_ParseWithLengthOpts(data,size,&end,false);
    if(!m)return NULL;
    while(end<data+size && isspace((unsigned char)*end))end++;
    if(end!=data+size){cJSON_Delete(m);return NULL;}
    return m;
}
static bool terminal(const char *status)
{
    return status && (!strcmp(status,"succeeded") || !strcmp(status,"failed") ||
        !strcmp(status,"cancelled") || !strcmp(status,"timed_out") ||
        !strcmp(status,"interrupted") || !strcmp(status,"unknown"));
}
static char *job_path(job_io *io,const char *id,const char *name)
{
    char *dir=plat_join(io->root,id);
    if(!name)return dir;
    char *p=plat_join(dir,name);free(dir);return p;
}
static bool known_status(const char *status)
{
    if(terminal(status))return true;
    return status && (!strcmp(status,"preparing") || !strcmp(status,"queued") ||
        !strcmp(status,"starting") || !strcmp(status,"running") ||
        !strcmp(status,"detaching") || !strcmp(status,"cancelling"));
}
static size_t known_index(job_io *io,const char *id)
{
    for(size_t i=0;i<io->known_count;i++)if(!strcmp(io->known[i],id))return i;
    return io->known_count;
}
static bool push(job_io *io,work *item)
{
    lock(io);
    size_t limit=item->type==1?JOB_IO_LOG_QUEUE:JOB_IO_QUEUE;
    size_t known=known_index(io,item->id);
    if(io->closing || io->count>=limit || (item->type!=0 && known==io->known_count) ||
       (item->type==0 && known==io->known_count && io->known_count==JOB_IO_RECORDS)) {
        if(item->type==1)io->dropped++;
        unlock(io);free(item->data);return false;
    }
    if(item->type==0 && known==io->known_count)
        snprintf(io->known[io->known_count++],97,"%s",item->id);
    io->queue[(io->head+io->count)%JOB_IO_QUEUE]=*item;
    io->count++;io->bytes+=item->size;
    if(io->count>io->high_water)io->high_water=io->count;
    signal_work(io);unlock(io);return true;
}
static void complete(job_io *io,job_io_result *result)
{
    lock(io);
    while(io->result_count==JOB_IO_QUEUE && !io->closing)wait_work(io);
    if(io->result_count==JOB_IO_QUEUE){unlock(io);job_io_result_free(result);return;}
    io->results[(io->result_head+io->result_count)%JOB_IO_QUEUE]=*result;
    io->result_count++;
    signal_work(io);unlock(io);
    if(io->wake)plat_wake_signal(io->wake);
}
static int flush_logs(job_io *io,const char *id)
{
    for(int i=0;i<4;i++) {
        char name[32];snprintf(name,sizeof name,"output.%d.jsonl",i);
        char *path=job_path(io,id,name);plat_stat_t st;
        bool exists=plat_stat(path,&st) && st.exists;
        int rc=exists?plat_sync_file(path):0;
        free(path);if(rc<0)return -1;
    }
    return 0;
}
static int append_log(job_io *io,const work *item)
{
    char *dir=job_path(io,item->id,NULL);
    int rc=plat_makedirs(dir);free(dir);
    if(rc<0)return -1;
    buf_t base64={0};b64_encode(&base64,(const unsigned char *)item->data,item->size);
    cJSON *record=cJSON_CreateObject();
    cJSON_AddNumberToObject(record,"cursor",(double)item->cursor);
    cJSON_AddNumberToObject(record,"next_cursor",(double)(item->cursor+item->size));
    cJSON_AddStringToObject(record,"stream",item->stream==2?"stderr":"stdout");
    cJSON_AddStringToObject(record,"data",buf_cstr(&base64));
    char *json=cJSON_PrintUnformatted(record);
    cJSON_Delete(record);buf_free(&base64);
    char *path=job_path(io,item->id,"output.0.jsonl");
    plat_stat_t st;bool exists=plat_stat(path,&st) && st.exists;
    if(exists && st.size+(int64_t)strlen(json)+1>JOB_IO_SEGMENT_MAX) {
        char *old=job_path(io,item->id,"output.3.jsonl");
        plat_stat_t oldest;
        if(plat_stat(old,&oldest) && oldest.exists && plat_unlink(old)<0)rc=-1;
        free(old);
        for(int i=2;i>=0 && rc==0;i--) {
            char from_name[32],to_name[32];
            snprintf(from_name,sizeof from_name,"output.%d.jsonl",i);
            snprintf(to_name,sizeof to_name,"output.%d.jsonl",i+1);
            char *from=job_path(io,item->id,from_name),*to=job_path(io,item->id,to_name);
            plat_stat_t source;
            if(plat_stat(from,&source) && source.exists)rc=plat_rename(from,to);
            free(from);free(to);
        }
    }
    if(rc==0) {
        buf_t line={0};buf_puts(&line,json);buf_puts(&line,"\n");
        rc=plat_append_file(path,line.p,line.len,false);buf_free(&line);
    }
    free(path);cJSON_free(json);return rc;
}
static cJSON *read_log(job_io *io,const work *item)
{
    cJSON *result=cJSON_CreateObject(),*chunks=cJSON_AddArrayToObject(result,"chunks");
    uint64_t cursor=item->cursor,oldest=UINT64_MAX,latest=0;
    size_t used=0,count=0;
    bool gaps=false,malformed=false,full=false;
    for(int file=3;file>=0;file--) {
        char name[32];snprintf(name,sizeof name,"output.%d.jsonl",file);
        char *path=job_path(io,item->id,name);plat_stat_t st;
        bool exists=plat_stat(path,&st) && st.exists;buf_t data={0};
        if(exists && plat_read_file(path,&data,JOB_IO_SEGMENT_MAX)<0){malformed=true;free(path);buf_free(&data);continue;}
        free(path);
        char *line=data.p,*end=data.p?data.p+data.len:NULL;
        while(line && line<end) {
            char *nl=memchr(line,'\n',(size_t)(end-line));
            if(!nl){malformed=true;break;}
            cJSON *record=parse_json(line,(size_t)(nl-line));
            const cJSON *begin=cJSON_GetObjectItemCaseSensitive(record,"cursor");
            const cJSON *next=cJSON_GetObjectItemCaseSensitive(record,"next_cursor");
            const char *encoded=str(record,"data"),*stream=str(record,"stream");
            size_t n=0;unsigned char *bytes=encoded?b64_decode(encoded,&n):NULL;
            bool valid=cJSON_IsNumber(begin) && cJSON_IsNumber(next) && begin->valuedouble>=0 &&
                next->valuedouble<=9007199254740991.0 && next->valuedouble>begin->valuedouble &&
                (double)(uint64_t)begin->valuedouble==begin->valuedouble &&
                (double)(uint64_t)next->valuedouble==next->valuedouble &&
                bytes && n<=8192 && (double)n==next->valuedouble-begin->valuedouble &&
                stream && (!strcmp(stream,"stdout") || !strcmp(stream,"stderr"));
            if(!valid)malformed=true;
            else {
                uint64_t from=(uint64_t)begin->valuedouble,to=(uint64_t)next->valuedouble;
                if(from<oldest)oldest=from;
                if(to>latest)latest=to;
                if(!full && to>cursor) {
                    size_t skip=cursor>from?(size_t)(cursor-from):0;
                    if(from>cursor){gaps=true;cursor=from;}
                    size_t take=n-skip;
                    if(take>item->limit-used)take=item->limit-used;
                    if(take) {
                        buf_t b64={0};b64_encode(&b64,bytes+skip,take);
                        cJSON *chunk=cJSON_CreateObject();
                        cJSON_AddNumberToObject(chunk,"cursor",(double)(from+skip));
                        cJSON_AddNumberToObject(chunk,"next_cursor",(double)(from+skip+take));
                        cJSON_AddStringToObject(chunk,"stream",stream);
                        cJSON_AddStringToObject(chunk,"data",buf_cstr(&b64));
                        cJSON_AddItemToArray(chunks,chunk);buf_free(&b64);
                        used+=take;count++;cursor=from+skip+take;
                    }
                    full=used>=item->limit || count>=64;
                }
            }
            free(bytes);cJSON_Delete(record);line=nl+1;
        }
        buf_free(&data);
    }
    cJSON_AddNumberToObject(result,"next_cursor",(double)cursor);
    cJSON_AddNumberToObject(result,"output_cursor",(double)latest);
    cJSON_AddNumberToObject(result,"truncated_before",(double)(oldest==UINT64_MAX?0:oldest));
    cJSON_AddBoolToObject(result,"gap",gaps||malformed);
    if(malformed)cJSON_AddStringToObject(result,"error","日志包含缺口、半条或损坏的记录；原文件保留");
    return result;
}
static void execute(job_io *io,work *item)
{
    job_io_result result={.ok=true,.revision=item->revision,.request_id=item->request_id};
    snprintf(result.job_id,sizeof result.job_id,"%s",item->id);
    snprintf(result.kind,sizeof result.kind,"%s",item->kind);
    int rc=0;
    if(item->type==0) {
        char *dir=job_path(io,item->id,NULL);rc=plat_makedirs(dir);free(dir);
        if(rc==0)rc=flush_logs(io,item->id);
        char *path=job_path(io,item->id,"meta.json");
        cJSON *meta=parse_json(item->data,item->size);
        lock(io);size_t index=known_index(io,item->id);
        bool gap=index<io->known_count && io->log_gap[index];unlock(io);
        if(gap) {
            cJSON_DeleteItemFromObjectCaseSensitive(meta,"output_gap");
            cJSON_AddBoolToObject(meta,"output_gap",true);
            cJSON_DeleteItemFromObjectCaseSensitive(meta,"log_error");
            cJSON_AddStringToObject(meta,"log_error","日志写入失败，输出包含缺口");
        }
        char *json=cJSON_PrintUnformatted(meta);
        if(!json || strlen(json)>JOB_IO_META_MAX)rc=-1;
        if(rc==0)rc=plat_write_durable(path,json,strlen(json),0600);
        if(rc==0){result.data=meta;meta=NULL;}
        cJSON_Delete(meta);cJSON_free(json);free(path);
    } else if(item->type==1) {
        rc=append_log(io,item);
        if(rc<0){lock(io);size_t index=known_index(io,item->id);if(index<io->known_count)io->log_gap[index]=true;unlock(io);}
    }
    else if(item->type==2)result.data=read_log(io,item);
    else if(item->type==3) {
        char *dir=job_path(io,item->id,NULL);rc=plat_remove_tree(dir);free(dir);
        if(rc==0) {
            lock(io);size_t index=known_index(io,item->id);
            if(index<io->known_count) {
                io->known_count--;
                memcpy(io->known[index],io->known[io->known_count],97);
                io->log_gap[index]=io->log_gap[io->known_count];
                io->log_gap[io->known_count]=false;
            }
            unlock(io);
        }
    }
    result.ok=rc==0;
    if(rc<0)snprintf(result.error,sizeof result.error,"Job %s 的 %s 未完成：%s",item->id,item->kind,strerror(errno));
    if(item->type!=1 || rc<0)complete(io,&result);
    free(item->data);
}
#if NA_WINDOWS
static DWORD WINAPI worker(LPVOID value)
#else
static void *worker(void *value)
#endif
{
    job_io *io=value;
    for(;;) {
        lock(io);
        while(!io->count && !io->closing)wait_work(io);
        if(!io->count && io->closing){unlock(io);break;}
        work item=io->queue[io->head];
        io->head=(io->head+1)%JOB_IO_QUEUE;io->count--;io->bytes-=item.size;
        signal_work(io);unlock(io);
        execute(io,&item);
    }
    return 0;
}
static int recover(job_io *io,cJSON *records,char *error,size_t error_len)
{
    if(plat_makedirs(io->root)<0)return -1;
    plat_dirent *entries=NULL;size_t n=0;bool truncated=false;
    if(!plat_read_dir(io->root,JOB_IO_RECORDS+1,&entries,&n,&truncated,error,error_len))return -1;
    int rc=0;size_t valid=0;
    for(size_t i=0;i<n && rc==0;i++) {
        if(!entries[i].dir || !valid_id(entries[i].name))continue;
        if(++valid>JOB_IO_RECORDS){rc=-1;break;}
        char *path=job_path(io,entries[i].name,"meta.json");buf_t bytes={0};
        if(plat_read_file(path,&bytes,JOB_IO_META_MAX)<0) {
            snprintf(error,error_len,"读取 Job %s 失败，保留原目录",entries[i].name);rc=-1;free(path);buf_free(&bytes);break;
        }
        cJSON *m=parse_json(bytes.p,bytes.len);buf_free(&bytes);
        const cJSON *version=cJSON_GetObjectItemCaseSensitive(m,"version");
        const char *id=str(m,"job_id"),*status=str(m,"status");
        if(!cJSON_IsNumber(version) || version->valuedouble!=1 || !id || strcmp(id,entries[i].name) || !known_status(status)) {
            snprintf(error,error_len,"Job %s 的记录版本或身份损坏，保留原文件",entries[i].name);rc=-1;cJSON_Delete(m);free(path);break;
        }
        if(!terminal(status)) {
            bool queued=!strcmp(status,"queued") || !strcmp(status,"preparing");
            cJSON_ReplaceItemInObjectCaseSensitive(m,"status",cJSON_CreateString(queued?"interrupted":"unknown"));
            cJSON_DeleteItemFromObjectCaseSensitive(m,"recovery_reason");
            cJSON_AddStringToObject(m,"recovery_reason","执行节点已重启；没有自动重放原命令");
            cJSON_DeleteItemFromObjectCaseSensitive(m,"ended_at");
            cJSON_AddNumberToObject(m,"ended_at",(double)time(NULL));
            cJSON_DeleteItemFromObjectCaseSensitive(m,"notification_state");
            cJSON_AddStringToObject(m,"notification_state","pending");
            char *json=cJSON_PrintUnformatted(m);
            rc=plat_write_durable(path,json,strlen(json),0600);cJSON_free(json);
        }
        free(path);
        if(rc==0) {
            snprintf(io->known[io->known_count++],97,"%s",entries[i].name);
            cJSON_AddItemToArray(records,m);
        } else cJSON_Delete(m);
    }
    plat_dirents_free(entries,n);
    if(truncated || valid>JOB_IO_RECORDS) {
        snprintf(error,error_len,"Job 目录超过恢复上限 %d，未删除任何记录",JOB_IO_RECORDS);rc=-1;
    }
    return rc;
}
job_io *job_io_open(const char *root,na_wake *wake,cJSON **recovered,char *error,size_t error_len)
{
    *recovered=NULL;
    job_io *io=xcalloc(1,sizeof *io);io->root=xstrdup(root);io->wake=wake;
    cJSON *records=cJSON_CreateArray();
    if(recover(io,records,error,error_len)<0) {
        if(!*error)snprintf(error,error_len,"Job 存储不可用：%s",strerror(errno));
        cJSON_Delete(records);free(io->root);free(io);return NULL;
    }
#if NA_WINDOWS
    InitializeCriticalSection(&io->mu);InitializeConditionVariable(&io->cond);
    io->thread=CreateThread(NULL,0,worker,io,0,NULL);
    bool started=io->thread!=NULL;
#else
    pthread_mutex_init(&io->mu,NULL);pthread_cond_init(&io->cond,NULL);
    bool started=pthread_create(&io->thread,NULL,worker,io)==0;
#endif
    if(!started) {
        snprintf(error,error_len,"Job I/O 工作者无法启动");
#if NA_WINDOWS
        DeleteCriticalSection(&io->mu);
#else
        pthread_cond_destroy(&io->cond);pthread_mutex_destroy(&io->mu);
#endif
        cJSON_Delete(records);free(io->root);free(io);return NULL;
    }
    *recovered=records;return io;
}
bool job_io_meta(job_io *io,const char *id,uint64_t revision,const char *kind,const cJSON *meta)
{
    if(!valid_id(id) || !kind || strlen(kind)>31 || !cJSON_IsObject(meta))return false;
    const char *actual=str(meta,"job_id");
    const cJSON *version=cJSON_GetObjectItemCaseSensitive(meta,"version");
    if(!actual || strcmp(actual,id) || !cJSON_IsNumber(version) || version->valuedouble!=1 ||
       !known_status(str(meta,"status")))return false;
    char *json=cJSON_PrintUnformatted(meta);
    size_t size=json?strlen(json):0;
    if(!size || size>JOB_IO_META_MAX){cJSON_free(json);return false;}
    work item={.type=0,.revision=revision,.data=json,.size=size};
    snprintf(item.id,sizeof item.id,"%s",id);snprintf(item.kind,sizeof item.kind,"%s",kind);
    return push(io,&item);
}
bool job_io_output(job_io *io,const char *id,uint64_t cursor,int stream,const void *bytes,size_t n)
{
    if(!valid_id(id) || !n || n>8192 || cursor>9007199254740991ULL-n || (stream!=1 && stream!=2))return false;
    work item={.type=1,.stream=stream,.cursor=cursor,.data=xmalloc(n),.size=n};
    memcpy(item.data,bytes,n);snprintf(item.id,sizeof item.id,"%s",id);snprintf(item.kind,sizeof item.kind,"output");
    return push(io,&item);
}
bool job_io_read(job_io *io,const char *id,uint64_t cursor,size_t max_bytes,uint64_t request_id)
{
    if(!valid_id(id) || !max_bytes || max_bytes>JOB_IO_OUTPUT_MAX || cursor>9007199254740991ULL)return false;
    work item={.type=2,.cursor=cursor,.request_id=request_id,.limit=max_bytes};
    snprintf(item.id,sizeof item.id,"%s",id);snprintf(item.kind,sizeof item.kind,"read");
    return push(io,&item);
}
bool job_io_remove(job_io *io,const char *id,uint64_t request_id)
{
    if(!valid_id(id))return false;
    work item={.type=3,.request_id=request_id};
    snprintf(item.id,sizeof item.id,"%s",id);snprintf(item.kind,sizeof item.kind,"remove");
    return push(io,&item);
}
bool job_io_poll(job_io *io,job_io_result *result)
{
    lock(io);
    if(!io->result_count){unlock(io);return false;}
    *result=io->results[io->result_head];
    io->result_head=(io->result_head+1)%JOB_IO_QUEUE;io->result_count--;
    signal_work(io);unlock(io);return true;
}
job_io_stats job_io_get_stats(job_io *io)
{
    lock(io);
    job_io_stats stats={.queued=io->count,.bytes=io->bytes,.high_water=io->high_water,.dropped=io->dropped};
    unlock(io);return stats;
}
void job_io_result_free(job_io_result *result){cJSON_Delete(result->data);result->data=NULL;}
void job_io_close(job_io *io)
{
    if(!io)return;
    lock(io);io->closing=true;signal_work(io);unlock(io);
#if NA_WINDOWS
    WaitForSingleObject(io->thread,INFINITE);CloseHandle(io->thread);DeleteCriticalSection(&io->mu);
#else
    pthread_join(io->thread,NULL);pthread_cond_destroy(&io->cond);pthread_mutex_destroy(&io->mu);
#endif
    for(size_t i=0;i<io->result_count;i++)job_io_result_free(&io->results[(io->result_head+i)%JOB_IO_QUEUE]);
    free(io->root);free(io);
}
