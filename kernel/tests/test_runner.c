/* Actual child processes validate that each invocation keeps its own cap. */
#include "../src/runner.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void streamed(void *ctx,const cJSON *frame,const void *data,size_t n)
{
    (void)frame;(void)data;
    *(size_t *)ctx+=n;
}
int main(int argc,char **argv)
{
    plat_init();
    int count=0;
    char **args=plat_args(argc,argv,&count);
    if(count>1 && !strcmp(args[1],"child")) {
        char bytes[4096];memset(bytes,'x',sizeof bytes);
        fwrite(bytes,1,sizeof bytes,stdout);
        fflush(stdout);
        return 0;
    }
    if(count>1 && !strcmp(args[1],"child-stream")) {
        char bytes[4096];memset(bytes,'x',sizeof bytes);
        buf_t encoded={0};b64_encode(&encoded,(const unsigned char *)bytes,sizeof bytes);
        puts("{\"t\":\"started\",\"seq\":1,\"job_id\":\"j_fixture\",\"pid\":7}");
        for(int i=0;i<32;i++)
            printf("{\"t\":\"output\",\"seq\":%d,\"job_id\":\"j_fixture\",\"stream\":\"stdout\",\"encoding\":\"base64\",\"data\":\"%s\"}\n",i+2,buf_cstr(&encoded));
        puts("{\"t\":\"exit\",\"seq\":34,\"job_id\":\"j_fixture\",\"ok\":true,\"content\":\"stream finished\",\"exit_code\":0,\"signal\":0,\"timed_out\":false,\"cancelled\":false}");
        fflush(stdout);buf_free(&encoded);
        return 0;
    }
    runner_t r={.out_cap=16384};
    char *child[]={args[0],"child",NULL};
    char error[512]="";
    job_t *a=runner_start_bounded(&r,child,"s","large","fixture",xstrdup(""),0,8192,error,sizeof error);
    job_t *b=runner_start_bounded(&r,child,"s","small","fixture",xstrdup(""),0,256,error,sizeof error);
    if(!a || !b){fprintf(stderr,"spawn: %s\n",error);runner_shutdown(&r,100);return 1;}
    /* Altering the default cannot affect either already-created reader. */
    r.out_cap=16;
    job_t *c=runner_start_ex(&r,child,"s","default","fixture",xstrdup(""),0,error,sizeof error);
    if(!c){fprintf(stderr,"spawn: %s\n",error);runner_shutdown(&r,100);return 1;}
    size_t streamed_bytes=0;
    char *stream_child[]={args[0],"child-stream",NULL};
    job_t *d=runner_start_streamed(&r,stream_child,"s","stream","fixture",xstrdup(""),0,
                                  "j_fixture",streamed,&streamed_bytes,error,sizeof error);
    if(!d){fprintf(stderr,"stream spawn: %s\n",error);runner_shutdown(&r,100);return 1;}
    int finished=0,failed=0;
    int64_t deadline=now_ms()+10000;
    while(runner_count(&r) && now_ms()<deadline) {
        na_pollfd fds[16];pollref_t refs[16];
        size_t n=runner_pollfds(&r,fds,refs,16);
        if(n){plat_poll(fds,n,20);runner_on_poll(&r,fds,refs,n);}
        else plat_sleep_ms(2);
        runner_reap(&r,now_ms());
        job_t *j;
        while((j=runner_take_finished(&r,now_ms()))) {
            if(j->stream) {
                bool ok=false;char *result=job_result(j,&ok);
                if(!ok || strcmp(result,"stream finished") || streamed_bytes!=131072 ||
                   j->out.len>NA_EXEC_FRAME_MAX || j->out_total<131072) {
                    fprintf(stderr,"stream result: %s bytes=%zu retained=%zu\n",result,streamed_bytes,j->out.len);failed++;
                }
                free(result);finished++;job_free(j);continue;
            }
            size_t cap=!strcmp(j->id,"large")?8192:!strcmp(j->id,"small")?256:16;
            size_t retained=cap<4096?cap:4096;
            if(j->out_cap!=cap || j->out.len!=retained || j->out_total!=4096 || j->exit_code!=0) {
                fprintf(stderr,"%s: cap=%zu retained=%zu total=%zu exit=%d\n",j->id,j->out_cap,j->out.len,j->out_total,j->exit_code);
                failed++;
            }
            finished++;job_free(j);
        }
    }
    if(finished!=4)failed++;
    runner_shutdown(&r,100);
    plat_args_free(args,count);
    plat_shutdown();
    printf("runner: %d children, %d failures\n",finished,failed);
    return failed?1:0;
}
