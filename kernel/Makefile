CC       ?= cc
CFLAGS   ?= -O2 -g
WARN     := -Wall -Wextra -Wpedantic -Wshadow -Wstrict-prototypes
CPPFLAGS += -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ithird_party/cjson
STD      := -std=c11

BIN   := nano-kernel
EXEC  := nano-exec

# 平台层：POSIX（Linux/macOS）或 Windows。交叉编译 Windows 时用 NA_PLATFORM=windows。
ifeq ($(NA_PLATFORM),windows)
PLAT := src/plat_win.c
SB   := src/sandbox_win.c
else
PLAT := src/plat_posix.c
SB   := src/sandbox.c
endif

SRCS  := src/main.c src/core.c src/conf.c src/tools.c src/runner.c src/link.c src/util.c $(PLAT)
XSRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c $(SB) src/util.c $(PLAT)
XWIN_SRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox_win.c src/util.c src/plat_win.c
OBJS  := $(SRCS:%.c=build/%.o) build/cJSON.o
XOBJS := $(XSRCS:%.c=build/%.o) build/cJSON.o

# Windows 执行器：用 zig 交叉编译，在 WSL 里通过 interop 在真实 Windows 上测试。
ZIG      ?= ../build/tools/zig/zig
RG_WIN   ?= ../build/tools/rg.exe
WIN_EXEC := build/nano-exec.exe
WIN_CFLAGS := -O2 -g -std=c11 -Wall -Wextra -Wshadow -Isrc -Ithird_party/cjson
WIN_LIBS   := -lws2_32 -lshell32

# 测试用 ASan/UBSan 构建，内存问题直接报出来
SAN := -fsanitize=address,undefined -fno-omit-frame-pointer

# 远程执行用：执行器的源码包（服务器跑不了本机编译的执行器时，传过去现编），
# 能静态链接时顺带编一个静态版本（不依赖服务器的 glibc 版本），不能就算了
SRCTAR := nano-exec-src.tar.gz
XHDRS  := src/nexec.h src/sandbox.h src/util.h src/plat.h third_party/cjson/cJSON.h

.PHONY: all test test-win windows-exec clean static

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

windows-exec: $(WIN_EXEC)

$(WIN_EXEC): $(XWIN_SRCS) third_party/cjson/cJSON.c $(wildcard src/*.h)
	@test -x "$(ZIG)" || { echo "没有 zig：先运行 scripts/fetch-zig.sh"; exit 1; }
	@mkdir -p $(dir $@)
	$(ZIG) cc -target x86_64-windows-gnu $(WIN_CFLAGS) -o $@ $(XWIN_SRCS) third_party/cjson/cJSON.c $(WIN_LIBS)

# Windows 执行器通过 WSL interop 在真实 Windows 上跑；没有 zig 或 rg.exe 时给出提示后跳过
test-win:
	@if [ -x "$(ZIG)" ]; then \
	  $(MAKE) --no-print-directory windows-exec && \
	  { test -f "$(RG_WIN)" || { echo "没有 $(RG_WIN)：先运行 scripts/fetch-rg.sh"; exit 1; }; } && \
	  python3 tests/test_exec_win.py $(WIN_EXEC) $(RG_WIN); \
	else echo "（没有 zig，跳过 Windows 执行器测试；先运行 scripts/fetch-zig.sh）"; fi

test: build/test_core build/nano-kernel-asan build/nano-exec-asan test-win
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
	rm -rf build $(BIN) $(EXEC) $(SRCTAR) nano-exec-static

-include $(OBJS:.o=.d) $(XOBJS:.o=.d)
