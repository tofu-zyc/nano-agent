CC       ?= cc
CFLAGS   ?= -O2 -g
WARN     := -Wall -Wextra -Wpedantic -Wshadow -Wstrict-prototypes
CPPFLAGS += -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ibuild -Ithird_party/cjson
STD      := -std=c11

# 版本号：发布时由上层 make release VERSION=x.y.z 传进来，平时用 git describe
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_H := build/version.h

# 在 macOS 上直接 make 时自动用 macOS 的平台层
ifeq ($(shell uname -s 2>/dev/null),Darwin)
NA_PLATFORM ?= macos
endif

BIN   := nano-kernel
EXEC  := nano-exec

# 平台层：POSIX（Linux/macOS）或 Windows。交叉编译 Windows 时用 NA_PLATFORM=windows。
ifeq ($(NA_PLATFORM),windows)
PLAT := src/plat_win.c
SB   := src/sandbox_win.c
RUNNER := src/runner_win.c
PIPE   := src/pipe_win.c
else ifeq ($(NA_PLATFORM),macos)
CPPFLAGS += -D_DARWIN_C_SOURCE
PLAT := src/plat_posix.c
SB   := src/sandbox_stub.c
RUNNER := src/runner.c
PIPE   :=
else
PLAT := src/plat_posix.c
SB   := src/sandbox.c
RUNNER := src/runner.c
PIPE   :=
endif

SRCS  := src/main.c src/core.c src/conf.c src/tools.c src/update.c src/ctl.c $(RUNNER) src/link.c src/pipe.c $(PIPE) src/util.c $(PLAT)
XSRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c $(SB) src/util.c $(PLAT)
XWIN_SRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox_win.c src/util.c src/plat_win.c
KWIN_SRCS := src/main.c src/core.c src/conf.c src/tools.c src/update.c src/ctl.c src/runner_win.c src/link.c src/pipe.c src/pipe_win.c src/util.c src/plat_win.c
MAC_SRCS  := src/main.c src/core.c src/conf.c src/tools.c src/update.c src/ctl.c src/runner.c src/link.c src/pipe.c src/util.c src/plat_posix.c
MAC_XSRCS := src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox_stub.c src/util.c src/plat_posix.c
OBJS  := $(SRCS:%.c=build/%.o) build/cJSON.o
XOBJS := $(XSRCS:%.c=build/%.o) build/cJSON.o

# Windows 执行器：用 zig 交叉编译，在 WSL 里通过 interop 在真实 Windows 上测试。
ZIG      ?= ../build/tools/zig/zig
RG_WIN   ?= ../build/tools/rg.exe
WIN_EXEC := build/nano-exec.exe
WIN_KERNEL := build/nano-kernel.exe
WIN_CFLAGS := -O2 -g -std=c11 -Wall -Wextra -Wshadow -Isrc -Ibuild -Ithird_party/cjson
WIN_LIBS   := -lws2_32 -lshell32 -ladvapi32

# 发布包用：Linux 静态链接（musl），不挑发行版的 glibc
CROSS_CFLAGS := -O2 -std=c11 -fno-lto -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ibuild -Ithird_party/cjson
CROSS_DIR    := build/cross
MAC_CFLAGS   := $(CROSS_CFLAGS) -D_DARWIN_C_SOURCE

# 测试用 ASan/UBSan 构建，内存问题直接报出来
SAN := -fsanitize=address,undefined -fno-omit-frame-pointer

# 远程执行用：执行器的源码包（服务器跑不了本机编译的执行器时，传过去现编），
# 能静态链接时顺带编一个静态版本（不依赖服务器的 glibc 版本），不能就算了
SRCTAR := nano-exec-src.tar.gz
XHDRS  := src/nexec.h src/sandbox.h src/util.h src/plat.h third_party/cjson/cJSON.h

.DEFAULT_GOAL := all
.PHONY: all test test-win windows-exec windows-kernel cross-linux cross-macos clean static FORCE

# 版本号变了才重写 version.h，只有用到它的文件会重新编译
$(VERSION_H): FORCE
	@mkdir -p $(dir $@)
	@echo '#define NA_VERSION "$(VERSION)"' > $@.tmp
	@cmp -s $@.tmp $@ && rm -f $@.tmp || mv $@.tmp $@

all: $(BIN) $(EXEC) $(SRCTAR) static

# GNU tar 才有这些让结果固定的选项；macOS 的 bsdtar 没有，照常打包
TARFLAGS := $(shell tar --version 2>/dev/null | grep -q GNU && echo --sort=name --owner=0 --group=0 --mtime=@0)

$(SRCTAR): $(XSRCS) $(XHDRS) third_party/cjson/cJSON.c
	tar czf $@ $(TARFLAGS) $^

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

build/src/%.o: src/%.c | $(VERSION_H)
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
windows-kernel: $(WIN_KERNEL)

# 本地交叉编译 Linux 发布包（x86_64 和 arm64），只用 zig，不需要 sudo
cross-linux: $(VERSION_H)
	@test -x "$(ZIG)" || { echo "没有 zig：先运行 scripts/fetch-zig.sh"; exit 1; }
	@mkdir -p $(CROSS_DIR)/linux-x86_64 $(CROSS_DIR)/linux-aarch64
	$(ZIG) cc -target x86_64-linux-musl $(CROSS_CFLAGS) -static -o $(CROSS_DIR)/linux-x86_64/nano-kernel $(SRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target x86_64-linux-musl $(CROSS_CFLAGS) -static -o $(CROSS_DIR)/linux-x86_64/nano-exec $(XSRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target aarch64-linux-musl $(CROSS_CFLAGS) -static -o $(CROSS_DIR)/linux-aarch64/nano-kernel $(SRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target aarch64-linux-musl $(CROSS_CFLAGS) -static -o $(CROSS_DIR)/linux-aarch64/nano-exec $(XSRCS) third_party/cjson/cJSON.c

# macOS 第一版没有沙箱（landlock=0）；只交叉编译，实际运行测试交给 macOS runner
cross-macos: $(VERSION_H)
	@test -x "$(ZIG)" || { echo "没有 zig：先运行 scripts/fetch-zig.sh"; exit 1; }
	@mkdir -p $(CROSS_DIR)/macos-aarch64 $(CROSS_DIR)/macos-x86_64
	$(ZIG) cc -target aarch64-macos $(MAC_CFLAGS) -o $(CROSS_DIR)/macos-aarch64/nano-kernel $(MAC_SRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target aarch64-macos $(MAC_CFLAGS) -o $(CROSS_DIR)/macos-aarch64/nano-exec $(MAC_XSRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target x86_64-macos $(MAC_CFLAGS) -o $(CROSS_DIR)/macos-x86_64/nano-kernel $(MAC_SRCS) third_party/cjson/cJSON.c
	$(ZIG) cc -target x86_64-macos $(MAC_CFLAGS) -o $(CROSS_DIR)/macos-x86_64/nano-exec $(MAC_XSRCS) third_party/cjson/cJSON.c

$(WIN_EXEC): $(XWIN_SRCS) third_party/cjson/cJSON.c $(wildcard src/*.h)
	@test -x "$(ZIG)" || { echo "没有 zig：先运行 scripts/fetch-zig.sh"; exit 1; }
	@mkdir -p $(dir $@)
	$(ZIG) cc -target x86_64-windows-gnu $(WIN_CFLAGS) -o $@ $(XWIN_SRCS) third_party/cjson/cJSON.c $(WIN_LIBS)

$(WIN_KERNEL): $(KWIN_SRCS) third_party/cjson/cJSON.c $(wildcard src/*.h) $(VERSION_H)
	@test -x "$(ZIG)" || { echo "没有 zig：先运行 scripts/fetch-zig.sh"; exit 1; }
	@mkdir -p $(dir $@)
	$(ZIG) cc -target x86_64-windows-gnu $(WIN_CFLAGS) -o $@ $(KWIN_SRCS) third_party/cjson/cJSON.c $(WIN_LIBS)

# Windows 执行器通过 WSL interop 在真实 Windows 上跑；没有 zig 或 rg.exe 时给出提示后跳过
test-win:
	@if [ "$(NANO_TEST_NO_WINDOWS)" != "1" ] && [ -x "$(ZIG)" ] && [ -x /mnt/c/Windows/System32/cmd.exe ]; then \
	  $(MAKE) --no-print-directory windows-exec windows-kernel && \
	  { test -f "$(RG_WIN)" || { echo "没有 $(RG_WIN)：先运行 scripts/fetch-rg.sh"; exit 1; }; } && \
	  python3 tests/test_exec_win.py $(WIN_EXEC) $(RG_WIN); \
	else echo "（没有 zig 或 WSL interop，跳过 Windows 执行器测试；Windows 上的测试在 CI 的 Windows runner 跑）"; fi

test: build/test_core build/nano-kernel-asan build/nano-exec-asan test-win
	./build/test_core
	python3 tests/test_exec.py build/nano-exec-asan
	python3 tests/test_host.py build/nano-kernel-asan build/nano-exec-asan

# 执行器的 ASan 版本
build/nano-exec-asan: $(XSRCS) third_party/cjson/cJSON.c $(wildcard src/*.h)
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(SAN) -o $@ $(XSRCS) third_party/cjson/cJSON.c

# 整个内核的 ASan 版本，给宿主层集成测试用
build/nano-kernel-asan: $(SRCS) third_party/cjson/cJSON.c $(wildcard src/*.h) $(VERSION_H)
	@mkdir -p $(dir $@)
	$(CC) $(STD) $(CPPFLAGS) -O1 -g $(SAN) -o $@ $(SRCS) third_party/cjson/cJSON.c

clean:
	rm -rf build $(BIN) $(EXEC) $(SRCTAR) nano-exec-static

-include $(OBJS:.o=.d) $(XOBJS:.o=.d)
