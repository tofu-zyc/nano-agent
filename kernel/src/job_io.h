/* One bounded worker owns Job metadata/log I/O; no disk work in poll callbacks. */
#ifndef NA_JOB_IO_H
#define NA_JOB_IO_H
#include "plat.h"
#include "cJSON.h"

#define JOB_IO_QUEUE 128
#define JOB_IO_LOG_QUEUE 112
#define JOB_IO_RECORDS 256
#define JOB_IO_META_MAX (64u<<10)
#define JOB_IO_SEGMENT_MAX (512u<<10)
#define JOB_IO_OUTPUT_MAX (64u<<10)
typedef struct job_io job_io;
typedef struct {
    char job_id[97],kind[32];
    uint64_t revision,request_id;
    bool ok;
    char error[512];
    cJSON *data;
} job_io_result;
typedef struct {size_t queued,bytes,high_water;uint64_t dropped;} job_io_stats;

job_io *job_io_open(const char *root,na_wake *wake,cJSON **recovered,char *error,size_t error_len);
bool job_io_meta(job_io *io,const char *id,uint64_t revision,const char *kind,const cJSON *meta);
bool job_io_output(job_io *io,const char *id,uint64_t cursor,int stream,const void *bytes,size_t n);
bool job_io_read(job_io *io,const char *id,uint64_t cursor,size_t max_bytes,uint64_t request_id);
bool job_io_remove(job_io *io,const char *id,uint64_t request_id);
bool job_io_poll(job_io *io,job_io_result *result);
job_io_stats job_io_get_stats(job_io *io);
void job_io_result_free(job_io_result *result);
void job_io_close(job_io *io);
#endif
