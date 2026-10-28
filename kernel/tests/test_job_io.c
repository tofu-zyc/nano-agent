#include "../src/job_io.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int failures,checks;
#define CHECK(c) do{checks++;if(!(c)){failures++;fprintf(stderr,"line %d: %s\n",__LINE__,#c);}}while(0)
static cJSON *meta(const char *id,const char *status)
{
    cJSON *m=cJSON_CreateObject();
    cJSON_AddNumberToObject(m,"version",1);cJSON_AddStringToObject(m,"job_id",id);
    cJSON_AddStringToObject(m,"status",status);cJSON_AddStringToObject(m,"owner_agent","fixture");
    cJSON_AddStringToObject(m,"origin_sid","s");cJSON_AddStringToObject(m,"origin_run_id","r");
    cJSON_AddStringToObject(m,"origin_call_id","c");cJSON_AddBoolToObject(m,"background",true);
    cJSON_AddStringToObject(m,"command","this request must never be replayed");
    return m;
}
static bool next_result(job_io *io,job_io_result *result)
{
    int64_t deadline=now_ms()+10000;
    while(now_ms()<deadline) {
        if(job_io_poll(io,result))return true;
        plat_sleep_ms(1);
    }
    CHECK(false);return false;
}
static void save(job_io *io,const char *status,uint64_t revision)
{
    cJSON *m=meta("j_fixture",status);
    CHECK(job_io_meta(io,"j_fixture",revision,"state",m));cJSON_Delete(m);
    job_io_result result={0};
    if(next_result(io,&result)) {
        CHECK(result.ok && result.revision==revision && !strcmp(result.kind,"state"));
        job_io_result_free(&result);
    }
}
int main(int argc,char **argv)
{
    plat_init();int count=0;char **args=plat_args(argc,argv,&count);
    char *base=count>1?plat_abspath(NULL,args[1]):plat_exe_dir();
    unsigned char random[8];CHECK(plat_random_bytes(random,sizeof random)==0);
    char name[64];snprintf(name,sizeof name,"job-io-test-%02x%02x%02x%02x%02x%02x%02x%02x",
        random[0],random[1],random[2],random[3],random[4],random[5],random[6],random[7]);
    char *root=plat_join(base,name);char error[512]="";cJSON *records=NULL;
    job_io *io=job_io_open(root,NULL,&records,error,sizeof error);
    if(!io){fprintf(stderr,"open: %s\n",error);return 1;}
    CHECK(cJSON_GetArraySize(records)==0);cJSON_Delete(records);
    cJSON *m=meta("j_fixture","queued");
    CHECK(!job_io_meta(io,"../escape",1,"create",m));
    CHECK(!job_io_output(io,"j_not_accepted",0,1,"x",1));
    cJSON_Delete(m);
    save(io,"queued",1);
    char chunk[8192];
    uint64_t revision=1;
    for(size_t i=0;i<512;i++) {
        memset(chunk,(int)(i%256),sizeof chunk);
        CHECK(job_io_output(io,"j_fixture",i*sizeof chunk,i%2?2:1,chunk,sizeof chunk));
        // Fence each bounded block before feeding the next, guaranteeing this
        // portion is lossless and exercises physical rotation beyond 4 segments.
        if(i%32==31)save(io,"running",++revision);
    }
    CHECK(job_io_read(io,"j_fixture",0,1000,71));
    job_io_result result={0};
    if(next_result(io,&result)) {
        CHECK(result.ok && result.request_id==71 && result.data);
        cJSON *data=result.data;
        uint64_t oldest=(uint64_t)cJSON_GetObjectItemCaseSensitive(data,"truncated_before")->valuedouble;
        CHECK(oldest>0 && cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(data,"gap")));
        CHECK(cJSON_GetObjectItemCaseSensitive(data,"output_cursor")->valuedouble==4194304);
        CHECK(cJSON_GetObjectItemCaseSensitive(data,"next_cursor")->valuedouble==(double)(oldest+1000));
        cJSON *first=cJSON_GetArrayItem(cJSON_GetObjectItemCaseSensitive(data,"chunks"),0);
        size_t bytes=0;unsigned char *raw=b64_decode(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(first,"data")),&bytes);
        CHECK(raw && bytes==1000);
        if(raw)for(size_t i=0;i<bytes;i++)CHECK(raw[i]==(unsigned char)((oldest/8192)%256));
        free(raw);job_io_result_free(&result);
    }
    // Flood admission remains bounded and reserves room for state operations.
    for(size_t i=0;i<4000;i++)job_io_output(io,"j_fixture",4194304+i*sizeof chunk,1,chunk,sizeof chunk);
    job_io_stats stats=job_io_get_stats(io);
    CHECK(stats.high_water<=JOB_IO_QUEUE && stats.queued<=JOB_IO_QUEUE &&
          stats.bytes<=JOB_IO_QUEUE*JOB_IO_META_MAX);
    save(io,"running",++revision);
    printf("job-io queue: high_water=%zu dropped=%llu\n",stats.high_water,(unsigned long long)stats.dropped);
    job_io_close(io);
    io=job_io_open(root,NULL,&records,error,sizeof error);
    CHECK(io && cJSON_GetArraySize(records)==1);
    if(io) {
        cJSON *restored=cJSON_GetArrayItem(records,0);
        CHECK(!strcmp(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(restored,"status")),"unknown"));
        CHECK(!strcmp(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(restored,"command")),"this request must never be replayed"));
        cJSON_Delete(records);
        // A failed metadata write is a failed acknowledgement, never a grant.
        char *broken=plat_join(root,"j_broken/meta.json");
        CHECK(plat_makedirs(broken)==0);
        m=meta("j_broken","queued");
        CHECK(job_io_meta(io,"j_broken",1,"create",m));cJSON_Delete(m);
        if(next_result(io,&result)) {CHECK(!result.ok && *result.error);job_io_result_free(&result);}
        CHECK(job_io_remove(io,"j_broken",72));
        if(next_result(io,&result)){CHECK(result.ok && result.request_id==72);job_io_result_free(&result);}
        free(broken);job_io_close(io);
    }
    // A future/corrupt state cannot be silently imported or overwritten.
    char *bad_dir=plat_join(root,"j_future");CHECK(plat_makedirs(bad_dir)==0);
    char *bad=plat_join(bad_dir,"meta.json");
    const char *future="{\"version\":2,\"job_id\":\"j_future\",\"status\":\"queued\"}";
    CHECK(plat_write_durable(bad,future,strlen(future),0600)==0);
    records=NULL;error[0]=0;
    io=job_io_open(root,NULL,&records,error,sizeof error);
    CHECK(!io && *error && !records);
    buf_t preserved={0};CHECK(plat_read_file(bad,&preserved,4096)==0 && preserved.len==strlen(future));
    buf_free(&preserved);
    char *real=plat_canonical(root),*real_base=plat_canonical(base);
    CHECK(real && real_base && plat_inside(real,real_base));
    if(real && real_base && plat_inside(real,real_base))CHECK(plat_remove_tree(root)==0);
    free(real);free(real_base);free(bad);free(bad_dir);free(root);free(base);
    plat_args_free(args,count);plat_shutdown();
    printf("job-io: %d checks, %d failures\n",checks,failures);
    return failures?1:0;
}
