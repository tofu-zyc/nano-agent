#include "exec_stream.h"
#include <stdlib.h>
#include <string.h>

static int reject(exec_stream *s,const char *reason)
{
    s->failed=true;s->error=reason;
    buf_free(&s->line);
    return -1;
}
static const char *string(const cJSON *o,const char *key)
{
    return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(o,key));
}
static bool is_integer(const cJSON *v,double min,double max)
{
    return cJSON_IsNumber(v) && v->valuedouble>=min && v->valuedouble<=max &&
           v->valuedouble==(double)(int64_t)v->valuedouble;
}
static bool valid_base64(const char *p)
{
    size_t n=strlen(p);
    if(!n || n%4 || n>((NA_EXEC_CHUNK_MAX+2)/3)*4) return false;
    size_t padding=0;
    if(p[n-1]=='=')padding++;
    if(n>1 && p[n-2]=='=')padding++;
    for(size_t i=0;i<n-padding;i++) {
        unsigned char c=(unsigned char)p[i];
        if(!((c>='A' && c<='Z') || (c>='a' && c<='z') || (c>='0' && c<='9') || c=='+' || c=='/'))return false;
    }
    for(size_t i=n-padding;i<n;i++)if(p[i]!='=')return false;
    return true;
}
static int frame(exec_stream *s)
{
    // cJSON stores strings as C strings. Reject escaped NUL before decoding so
    // a suffix cannot be discarded from an identity or base64 value.
    for(size_t i=0;i+1<s->line.len;i++) {
        if(s->line.p[i]!='\\')continue;
        if(i+5<s->line.len && !memcmp(s->line.p+i+1,"u0000",5))
            return reject(s,"执行器字段包含转义 NUL");
        i++;
    }
    const char *end=NULL;
    cJSON *m=cJSON_ParseWithLengthOpts(buf_cstr(&s->line),s->line.len+1,&end,true);
    if(!cJSON_IsObject(m)){cJSON_Delete(m);return reject(s,"执行器输出不是完整 JSON 帧");}
    const char *id=string(m,"job_id"), *t=string(m,"t");
    const cJSON *seq=cJSON_GetObjectItemCaseSensitive(m,"seq");
    if(!id || strcmp(id,s->job_id) || !t || !is_integer(seq,1,9007199254740991.0) ||
       (uint64_t)seq->valuedouble!=s->seq+1 || s->ended) {
        cJSON_Delete(m);return reject(s,"执行器帧身份、序号或终态边界不匹配");
    }
    unsigned char *bytes=NULL;
    size_t n=0;
    bool keep=false,valid=true;
    if(!strcmp(t,"started")) {
        const cJSON *pid=cJSON_GetObjectItemCaseSensitive(m,"pid");
        valid=!s->started && s->seq==0 && is_integer(pid,1,9007199254740991.0);
        if(valid)s->started=true;
    } else if(!strcmp(t,"output")) {
        const char *channel=string(m,"stream"), *encoding=string(m,"encoding"), *data=string(m,"data");
        valid=s->started && channel && (!strcmp(channel,"stdout") || !strcmp(channel,"stderr")) &&
              encoding && !strcmp(encoding,"base64") && data && valid_base64(data);
        if(valid) {
            bytes=b64_decode(data,&n);
            valid=bytes && n>0 && n<=NA_EXEC_CHUNK_MAX;
            if(valid)s->output_bytes+=n;
        }
    } else if(!strcmp(t,"exit")) {
        const cJSON *ok=cJSON_GetObjectItemCaseSensitive(m,"ok");
        valid=cJSON_IsBool(ok) && (!cJSON_IsTrue(ok) || s->started) &&
              string(m,"content") &&
              is_integer(cJSON_GetObjectItemCaseSensitive(m,"exit_code"),-2147483648.0,4294967295.0) &&
              is_integer(cJSON_GetObjectItemCaseSensitive(m,"signal"),0,255) &&
              cJSON_IsBool(cJSON_GetObjectItemCaseSensitive(m,"timed_out")) &&
              cJSON_IsBool(cJSON_GetObjectItemCaseSensitive(m,"cancelled"));
        if(valid && cJSON_IsTrue(ok)) {
            valid=!cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m,"timed_out")) &&
                  !cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m,"cancelled")) &&
                  cJSON_GetObjectItemCaseSensitive(m,"exit_code")->valuedouble==0 &&
                  cJSON_GetObjectItemCaseSensitive(m,"signal")->valuedouble==0;
        }
        if(valid){s->ended=true;s->terminal=m;keep=true;}
    } else valid=false;
    if(!valid) {
        free(bytes);cJSON_Delete(m);
        return reject(s,"执行器帧内容或状态顺序无效");
    }
    s->seq++;
    if(s->receive)s->receive(s->ctx,m,bytes,n);
    free(bytes);
    if(!keep)cJSON_Delete(m);
    s->line.len=0;
    return 0;
}
void exec_stream_init(exec_stream *s,const char *job_id,exec_frame_fn receive,void *ctx)
{
    memset(s,0,sizeof *s);
    s->job_id=xstrdup(job_id);s->receive=receive;s->ctx=ctx;
}
int exec_stream_feed(exec_stream *s,const void *bytes,size_t n)
{
    if(s->failed)return -1;
    const unsigned char *p=bytes;
    while(n--) {
        unsigned char c=*p++;
        if(c=='\n') {
            if(!s->line.len)return reject(s,"执行器包含空帧");
            if(frame(s)<0)return -1;
        } else {
            if(c==0)return reject(s,"执行器协议包含 NUL");
            if(s->line.len>=NA_EXEC_FRAME_MAX-1)return reject(s,"执行器单帧超过 16 KiB");
            buf_append(&s->line,&c,1);
        }
    }
    return 0;
}
bool exec_stream_finish(exec_stream *s)
{
    if(!s->failed && (!s->ended || s->line.len))reject(s,"执行器没有完整终态帧");
    return !s->failed && s->ended;
}
void exec_stream_free(exec_stream *s)
{
    free(s->job_id);buf_free(&s->line);cJSON_Delete(s->terminal);
    memset(s,0,sizeof *s);
}
