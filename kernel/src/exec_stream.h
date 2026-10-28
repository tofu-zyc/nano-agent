/* Incremental nano-exec v2 decoder, owned by one execution task. */
#ifndef NA_EXEC_STREAM_H
#define NA_EXEC_STREAM_H
#include "cJSON.h"
#include "util.h"

#define NA_EXEC_FRAME_MAX 16384u
#define NA_EXEC_CHUNK_MAX 8192u

typedef void (*exec_frame_fn)(void *ctx,const cJSON *frame,const void *bytes,size_t n);
typedef struct {
    char *job_id;
    buf_t line;
    uint64_t seq,output_bytes;
    bool started,ended,failed;
    const char *error;
    cJSON *terminal;
    exec_frame_fn receive;
    void *ctx;
} exec_stream;
void exec_stream_init(exec_stream *s,const char *job_id,exec_frame_fn receive,void *ctx);
int exec_stream_feed(exec_stream *s,const void *bytes,size_t n);
bool exec_stream_finish(exec_stream *s);
void exec_stream_free(exec_stream *s);
#endif
