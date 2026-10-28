#include "../src/exec_stream.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int checks,failures;
#define CHECK(c) do{checks++;if(!(c)){failures++;fprintf(stderr,"line %d: %s\n",__LINE__,#c);}}while(0)
static const char *START="{\"t\":\"started\",\"seq\":1,\"job_id\":\"j\",\"pid\":7}\n";
static const char *OUTPUT="{\"t\":\"output\",\"seq\":2,\"job_id\":\"j\",\"stream\":\"stderr\",\"encoding\":\"base64\",\"data\":\"AP+A\"}\n";
static const char *EXIT="{\"t\":\"exit\",\"seq\":3,\"job_id\":\"j\",\"ok\":true,\"content\":\"done\",\"exit_code\":0,\"signal\":0,\"timed_out\":false,\"cancelled\":false}\n";
static size_t observed;
static void receive(void *ctx,const cJSON *frame,const void *bytes,size_t n)
{
    (void)ctx;(void)frame;
    if(n) {
        static const unsigned char expected[]={0,255,128};
        CHECK(n==sizeof expected && !memcmp(bytes,expected,n));
        observed+=n;
    }
}
int main(int argc,char **argv)
{
    exec_stream s;
    if(argc>1 && !strcmp(argv[1],"--decode")) {
        exec_stream_init(&s,argc>2?argv[2]:"j_fixture",NULL,NULL);
        char buf[997];
        size_t n;
        while((n=fread(buf,1,sizeof buf,stdin)))exec_stream_feed(&s,buf,n);
        bool ok=exec_stream_finish(&s);
        if(ok)printf("decoded: frames=%llu bytes=%llu\n",(unsigned long long)s.seq,(unsigned long long)s.output_bytes);
        else fprintf(stderr,"%s\n",s.error);
        exec_stream_free(&s);return ok?0:1;
    }
    exec_stream_init(&s,"j",receive,NULL);
    const char *parts[]={START,OUTPUT,EXIT};
    for(size_t p=0;p<3;p++)for(const char *b=parts[p];*b;b++)CHECK(exec_stream_feed(&s,b,1)==0);
    CHECK(exec_stream_finish(&s) && s.started && s.ended && observed==3 && s.output_bytes==3 && s.terminal);
    CHECK(exec_stream_feed(&s,EXIT,strlen(EXIT))<0);
    exec_stream_free(&s);
    const char *invalid[]={
        "{\"t\":\"started\",\"seq\":1,\"job_id\":\"other\",\"pid\":7}\n",
        "{\"t\":\"started\",\"seq\":2,\"job_id\":\"j\",\"pid\":7}\n",
        "{\"t\":\"started\",\"seq\":1,\"job_id\":\"j\\u0000other\",\"pid\":7}\n",
        "{\"t\":\"started\",\"seq\":1.5,\"job_id\":\"j\",\"pid\":7}\n",
        "{\"t\":\"output\",\"seq\":1,\"job_id\":\"j\",\"stream\":\"stdout\",\"encoding\":\"base64\",\"data\":\"AA==\"}\n",
        "{\"t\":\"exit\",\"seq\":1,\"job_id\":\"j\",\"ok\":true,\"content\":\"done\",\"exit_code\":0,\"signal\":0,\"timed_out\":false,\"cancelled\":false}\n"
    };
    for(size_t i=0;i<sizeof invalid/sizeof *invalid;i++) {
        exec_stream_init(&s,"j",NULL,NULL);
        CHECK(exec_stream_feed(&s,invalid[i],strlen(invalid[i]))<0);
        CHECK(!exec_stream_finish(&s) && s.error);
        exec_stream_free(&s);
    }
    exec_stream_init(&s,"j",NULL,NULL);
    CHECK(exec_stream_feed(&s,START,strlen(START))==0);
    CHECK(!exec_stream_finish(&s));
    exec_stream_free(&s);
    exec_stream_init(&s,"j",NULL,NULL);
    char huge[NA_EXEC_FRAME_MAX+128];memset(huge,'a',sizeof huge);
    CHECK(exec_stream_feed(&s,huge,sizeof huge)<0 && s.line.cap==0);
    exec_stream_free(&s);
    printf("exec-stream: %d checks, %d failures\n",checks,failures);
    return failures?1:0;
}
