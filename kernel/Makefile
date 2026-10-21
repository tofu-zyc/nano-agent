CC       ?= cc
CFLAGS   ?= -O2 -g
WARN     := -Wall -Wextra -Wpedantic -Wshadow -Wstrict-prototypes
CPPFLAGS += -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ithird_party/cjson
STD      := -std=c11

BIN   := nano-kernel
EXEC  := nano-exec
SRCS  := src/main.c src/core.c src/conf.c src/tools.c src/runner.c src/link.c src/util.c
XSRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox.c src/util.c
OBJS  := $(SRCS:%.c=build/%.o) build/cJSON.o
XOBJS := $(XSRCS:%.c=build/%.o) build/cJSON.o

# 测试用 ASan/UBSan 构建，内存问题直接报出来
SAN := -fsanitize=address,undefined -fno-omit-frame-pointer

.PHONY: all test clean

all: $(BIN) $(EXEC)

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

test: build/test_core build/nano-kernel-asan build/nano-exec-asan
	./build/test_core
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
	rm -rf build $(BIN) $(EXEC)

-include $(OBJS:.o=.d) $(XOBJS:.o=.d)
