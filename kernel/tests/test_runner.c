/* Actual child processes validate that each invocation keeps its own cap. */
#include "../src/runner.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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
            size_t cap=!strcmp(j->id,"large")?8192:!strcmp(j->id,"small")?256:16;
            size_t retained=cap<4096?cap:4096;
            if(j->out_cap!=cap || j->out.len!=retained || j->out_total!=4096 || j->exit_code!=0) {
                fprintf(stderr,"%s: cap=%zu retained=%zu total=%zu exit=%d\n",j->id,j->out_cap,j->out.len,j->out_total,j->exit_code);
                failed++;
            }
            finished++;job_free(j);
        }
    }
    if(finished!=3)failed++;
    runner_shutdown(&r,100);
    plat_args_free(args,count);
    plat_shutdown();
    printf("runner: %d children, %d failures\n",finished,failed);
    return failed?1:0;
}
