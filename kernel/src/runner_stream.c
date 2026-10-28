#include "runner.h"
#include <stdlib.h>
#include <string.h>

void runner_stream_destroy(exec_stream *s)
{
    if(!s)return;
    exec_stream_free(s);free(s);
}
void runner_stream_feed(job_t *j,const void *data,size_t n)
{
    bool ended=j->stream->ended;
    exec_stream_feed(j->stream,data,n);
    if(!ended && j->stream->ended && !j->stream->failed) {
        char *json=cJSON_PrintUnformatted(j->stream->terminal);
        if(json){buf_append(&j->out,json,strlen(json));cJSON_free(json);}
    }
}
cJSON *runner_result_object(const job_t *j)
{
    if(j->stream) {
        if(!exec_stream_finish(j->stream))return NULL;
        return cJSON_Duplicate(j->stream->terminal,true);
    }
    return j->out_total<=j->out.len ? cJSON_ParseWithLength(j->out.p?j->out.p:"",j->out.len) : NULL;
}
