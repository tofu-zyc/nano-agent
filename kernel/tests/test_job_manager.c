#include "../src/job_manager.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
static int checks,failures;
#define CHECK(c) do{checks++;if(!(c)){failures++;fprintf(stderr,"line %d: %s\n",__LINE__,#c);}}while(0)
static const char *str(const cJSON *m,const char *key){return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(m,key));}
static bool event(job_manager *m,const char *id,const char *kind,bool expected)
{
    int64_t deadline=now_ms()+5000;job_event e;
    while(now_ms()<deadline) {
        if(jobs_poll(m,now_ms(),&e)) {
            bool match=!strcmp(e.id,id) && !strcmp(e.kind,kind);
            if(match)CHECK(e.ok==expected);
            else CHECK(e.ok);
            jobs_event_free(&e);
            if(match)return true;
        } else plat_sleep_ms(1);
    }
    CHECK(false);return false;
}
static managed_job *create(job_manager *m,const char *execution)
{
    cJSON *origin=cJSON_Parse("{\"agent\":\"owner\",\"sid\":\"s\",\"run_id\":\"r\",\"turn_id\":1,\"call_id\":\"c\",\"created_by\":\"user\"}");
    cJSON *req=cJSON_Parse("{\"tool\":\"Shell\",\"args\":{\"command\":\"never replay this fixture\",\"yield_after_ms\":1000}}");
    cJSON_AddStringToObject(cJSON_GetObjectItemCaseSensitive(req,"args"),"execution",execution);
    char error[512]="";managed_job *j=jobs_create(m,origin,req,"machine:files","dir:/fixture",1,error,sizeof error);
    if(!j)fprintf(stderr,"create: %s\n",error);
    CHECK(j && !jobs_created(j) && !jobs_spawn_ready(j) && !jobs_background(j));
    cJSON_Delete(origin);cJSON_Delete(req);
    if(j)event(m,jobs_id(j),"created",true);
    return j;
}
static void start(job_manager *m,managed_job *j)
{
    CHECK(jobs_created(j) && jobs_prepare_start(j) && !jobs_spawn_ready(j));
    event(m,jobs_id(j),"start",true);
    CHECK(jobs_spawn_ready(j));jobs_did_spawn(j);
    cJSON *frame=cJSON_Parse("{\"t\":\"started\",\"pid\":7}");
    jobs_observe(j,frame,NULL,0);cJSON_Delete(frame);
    event(m,jobs_id(j),"started",true);
    event(m,jobs_id(j),"running",true);
}
static void finish(managed_job *j)
{
    cJSON *frame=cJSON_Parse("{\"t\":\"exit\",\"ok\":true,\"content\":\"done\",\"exit_code\":0,\"signal\":0,\"timed_out\":false,\"cancelled\":false}");
    jobs_observe(j,frame,NULL,0);cJSON_Delete(frame);
    jobs_process_finished(j);
}
int main(int argc,char **argv)
{
    plat_init();int count=0;char **args=plat_args(argc,argv,&count);
    char *base=count>1?plat_abspath(NULL,args[1]):plat_exe_dir();
    unsigned char random[8];CHECK(plat_random_bytes(random,sizeof random)==0);
    char name[64];snprintf(name,sizeof name,"job-manager-test-%02x%02x%02x%02x%02x%02x%02x%02x",
        random[0],random[1],random[2],random[3],random[4],random[5],random[6],random[7]);
    char *root=plat_join(base,name),error[512]="";
    const char *epoch="00112233445566778899aabbccddeeff";
    job_manager *m=jobs_open(root,epoch,NULL,error,sizeof error);
    if(!m){fprintf(stderr,"open: %s\n",error);return 1;}
    managed_job *j=create(m,"foreground");
    if(!j){jobs_close(m);return 1;}start(m,j);
    cJSON *frame=cJSON_Parse("{\"t\":\"output\",\"stream\":\"stdout\"}");
    jobs_observe(j,frame,"first",5);cJSON_Delete(frame);
    CHECK(jobs_detach(j,71,error,sizeof error) && !jobs_background(j));
    // Completion arrives while the durable handoff is pending. Both facts must
    // survive, and the owner sees one detach and one independent completion.
    finish(j);
    event(m,jobs_id(j),"detached",true);
    CHECK(jobs_background(j));
    event(m,jobs_id(j),"terminal",true);
    CHECK(jobs_finished(j));
    cJSON *snapshot=jobs_snapshot(j);
    CHECK(!strcmp(str(snapshot,"status"),"succeeded") && cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(snapshot,"background")));
    CHECK(cJSON_GetObjectItemCaseSensitive(snapshot,"output_cursor")->valuedouble==5);
    cJSON_Delete(snapshot);
    CHECK(jobs_detach(j,71,error,sizeof error)); // duplicate handoff is idempotent
    managed_job *second=create(m,"foreground");start(m,second);finish(second);
    CHECK(!jobs_detach(second,72,error,sizeof error) && *error);
    event(m,jobs_id(second),"terminal",true);
    CHECK(!jobs_background(second));
    // A queued auto job can request a handoff; it must not spawn just to obtain
    // a job ID, and ticking repeatedly does not emit another request.
    managed_job *automatic=create(m,"auto");
    job_event e;int due=0;
    while(jobs_poll(m,now_ms()+2000,&e)) {
        if(!strcmp(e.kind,"detach_ready") && e.job==automatic)due++;
        jobs_event_free(&e);
    }
    CHECK(due==1 && !jobs_spawn_ready(automatic));
    CHECK(jobs_detach(automatic,73,error,sizeof error));
    event(m,jobs_id(automatic),"detached",true);
    CHECK(jobs_background(automatic) && !jobs_spawn_ready(automatic));
    // Sabotage only this fixture's publication point. A failed detach keeps
    // the old published foreground owner, and reports the failure.
    managed_job *failure=create(m,"foreground");start(m,failure);
    char *dir=plat_join(root,jobs_id(failure));
    char *path=plat_join(dir,"meta.json"),*backup=plat_join(dir,"meta.good");
    CHECK(plat_rename(path,backup)==0 && plat_makedirs(path)==0);
    CHECK(jobs_detach(failure,74,error,sizeof error));
    event(m,jobs_id(failure),"detached",false);
    CHECK(!jobs_background(failure));
    CHECK(plat_remove_tree(path)==0 && plat_rename(backup,path)==0);
    free(path);free(backup);free(dir);
    char saved_id[97];snprintf(saved_id,sizeof saved_id,"%s",jobs_id(automatic));
    jobs_close(m);
    m=jobs_open(root,"ffeeddccbbaa99887766554433221100",NULL,error,sizeof error);
    CHECK(m);
    if(m) {
        managed_job *recovered=jobs_find(m,saved_id);
        CHECK(recovered && jobs_background(recovered) && jobs_finished(recovered) && !jobs_spawn_ready(recovered));
        snapshot=jobs_snapshot(recovered);
        CHECK(!strcmp(str(snapshot,"status"),"interrupted"));
        cJSON_Delete(snapshot);jobs_close(m);
    }
    char *real=plat_canonical(root),*real_base=plat_canonical(base);
    CHECK(real && real_base && plat_inside(real,real_base));
    if(real && real_base && plat_inside(real,real_base))CHECK(plat_remove_tree(root)==0);
    free(real);free(real_base);free(root);free(base);plat_args_free(args,count);plat_shutdown();
    printf("job-manager: %d checks, %d failures\n",checks,failures);
    return failures?1:0;
}
