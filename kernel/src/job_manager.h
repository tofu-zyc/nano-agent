/* Job ownership and lifecycle. Main loop drives transitions; stdout callbacks
 * only copy bounded observations and enqueue log writes. */
#ifndef NA_JOB_MANAGER_H
#define NA_JOB_MANAGER_H
#include "job_io.h"
#include "exec_stream.h"
typedef struct job_manager job_manager;
typedef struct managed_job managed_job;
typedef struct {
    managed_job *job;
    char id[97],kind[32],error[512];
    bool ok;
    uint64_t request_id;
    cJSON *data;
} job_event;
job_manager *jobs_open(const char *root,const char *epoch,na_wake *wake,char *error,size_t n);
managed_job *jobs_create(job_manager *m,const cJSON *origin,const cJSON *request,
                         const char *shared,const char *exclusive,int wait_kind,
                         char *error,size_t n);
bool jobs_prepare_start(managed_job *j);
bool jobs_spawn_ready(const managed_job *j);
void jobs_did_spawn(managed_job *j);
void jobs_observe(void *job,const cJSON *frame,const void *data,size_t n);
void jobs_cancelling(managed_job *j);
void jobs_process_finished(managed_job *j);
void jobs_fail(managed_job *j,const char *status,const char *detail);
bool jobs_detach(managed_job *j,uint64_t request_id,char *error,size_t n);
void jobs_detach_denied(managed_job *j);
bool jobs_acknowledge(managed_job *j,uint64_t seq);
bool jobs_background(const managed_job *j);
bool jobs_finished(const managed_job *j);
bool jobs_created(const managed_job *j);
const char *jobs_id(const managed_job *j);
const cJSON *jobs_request(const managed_job *j);
cJSON *jobs_snapshot(const managed_job *j);
managed_job *jobs_find(job_manager *m,const char *id);
bool jobs_read(managed_job *j,uint64_t cursor,size_t bytes,uint64_t request_id);
bool jobs_poll(job_manager *m,int64_t now,job_event *event);
int64_t jobs_next_deadline(job_manager *m);
cJSON *jobs_list(job_manager *m);
void jobs_event_free(job_event *event);
/* Stop/join all associated stdout readers before releasing their observation owner. */
void jobs_close(job_manager *m);
#endif
