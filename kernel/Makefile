CC       ?= cc
CFLAGS   ?= -O2 -g
WARN     := -Wall -Wextra -Wpedantic -Wshadow -Wstrict-prototypes
CPPFLAGS += -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ithird_party/cjson
STD      := -std=c11

BIN   := nano-kernel
EXEC  := nano-exec
SRCS  := src/main.c src/core.c src/conf.c src/tools.c src/runner.c src/link.c src/skills.c src/util.c
XSRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox.c src/util.c
OBJS  := $(SRCS:%.c=build/%.o) build/cJSON.o
XOBJS := $(XSRCS:%.c=build/%.o) build/cJSON.o

# 测试用 ASan/UBSan 构建，内存问题直接报出来
SAN := -fsanitize=address,undefined -fno-omit-frame-pointer

# 远程执行用：执行器的源码包（服务器跑不了本机编译的执行器时，传过去现编），
# 能静态链接时顺带编一个静态版本（不依赖服务器的 glibc 版本），不能就算了
SRCTAR := nano-exec-src.tar.gz
XHDRS  := src/nexec.h src/sandbox.h src/util.h third_party/cjson/cJSON.h

.PHONY: all test clean static

all: $(BIN) $(EXEC) $(SRCTAR) static

$(SRCTAR): $(XSRCS) $(XHDRS) third_party/cjson/cJSON.c
	tar czf $@ --sort=name --owner=0 --group=0 --mtime=@0 $^

static: build/.static-tried

build/.static-tried: $(XOBJS)
	@$(CC) $(CFLAGS) $(LDFLAGS) -static -o nano-exec-static $^ 2>/dev/null \
	  || { rm -f nano-exec-static; echo "（本机不能静态链接，跳过 nano-exec-static；远程执行时会传动态版本或在服务器上现编）"; }
	@touch $@

$(BIN): $(OBJS)
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $^

# 执行器：内核每次调用工具都启动它一次，在沙箱里执行
$(EXEC): $(XOBJS)
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $^

build/src/%.o: src/%.c
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) $(CFLAGS) $(WARN) -MMD -MP -c -o $@ $<

# 第三方代码不套用本项目的警告选项
build/cJSON.o: third_party/cjson/cJSON.c
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CFLAGS) -c -o $@ $<

build/test_core: tests/test_core.c src/core.c src/core.h
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(WARN) $(SAN) -o $@ tests/test_core.c src/core.c

build/test_skills: tests/test_skills.c src/skills.c src/skills.h src/util.c src/util.h third_party/cjson/cJSON.c
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(WARN) $(SAN) -o $@ tests/test_skills.c src/skills.c src/util.c third_party/cjson/cJSON.c

test: build/test_core build/test_skills build/nano-kernel-asan build/nano-exec-asan
	./build/test_core
	./build/test_skills
	python3 tests/test_exec.py build/nano-exec-asan
	python3 tests/test_host.py build/nano-kernel-asan build/nano-exec-asan

# 执行器的 ASan 版本
build/nano-exec-asan: $(XSRCS) third_party/cjson/cJSON.c $(wildcard src/*.h)
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(SAN) -o $@ $(XSRCS) third_party/cjson/cJSON.c

# 整个内核的 ASan 版本，给宿主层集成测试用
build/nano-kernel-asan: $(SRCS) third_party/cjson/cJSON.c $(wildcard src/*.h)
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(SAN) -o $@ $(SRCS) third_party/cjson/cJSON.c

clean:
	rm -rf build $(BIN) $(EXEC) $(SRCTAR) nano-exec-static

-include $(OBJS:.o=.d) $(XOBJS:.o=.d)
