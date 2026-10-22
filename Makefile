.PHONY: all kernel gateway web test test-win run chat clean install installer install-kernel

all: kernel gateway

kernel:
	$(MAKE) -C kernel

gateway: kernel
	cd gateway && go build -o nano-gateway ./cmd/nano-gateway

# 重新构建网页界面（改了 gateway/web 下的前端代码之后；构建结果提交在仓库里，平时不需要 Node）
web:
	cd gateway/web && npm install --no-audit --no-fund && npm run check && npm run build

# 单元测试 + 宿主层集成测试 + 端到端测试（模型用 mock，不花钱）
test: all
	$(MAKE) -C kernel test
	cd gateway && go test -race ./...
	./scripts/e2e.sh
	@if [ -f kernel/build/nano-kernel.exe ] && [ -x /mnt/c/Windows/System32/cmd.exe ]; then \
	  python3 scripts/e2e-win.py; \
	else echo "（没有 Windows 内核或 WSL interop，跳过 Windows 内核端到端测试）"; fi

# 只跑 Windows 执行器和内核的测试（需要 zig、build/tools/rg.exe、WSL interop）
test-win:
	$(MAKE) -C kernel test-win
	python3 scripts/e2e-win.py

# 本机一键运行，在终端里直接聊天
run: all
	./scripts/run-local.sh

# 在终端里和正在运行的网关聊天（网关先 make run 跑起来）
chat:
	@test -x gateway/nano-gateway || $(MAKE) -s gateway
	@./gateway/nano-gateway chat -c gateway.toml

# 安装网关到 ~/.local（程序、配置、数据分开；不再放在源码目录里）
install: all
	./scripts/install-gateway.sh

# 在这台机器上安装内核（开机自启，连到网关）。别的电脑用 make installer 生成的单文件安装包
install-kernel:
	./scripts/install-kernel.sh

# 单文件内核安装包：安装脚本后面带着内核源码，拷到别的 Linux / WSL 电脑上 sh 一下就装好
INSTALLER := dist/nano-kernel-install.sh
installer:
	@mkdir -p dist build/installer/kernel
	@rm -rf build/installer/kernel/*
	@cd kernel && tar cf - Makefile src third_party | tar xf - -C ../build/installer/kernel
	@git describe --always --dirty 2>/dev/null > build/installer/kernel/VERSION || echo dev > build/installer/kernel/VERSION
	@{ cat scripts/install-kernel.sh; echo; echo '__NANO_PAYLOAD__'; \
	   tar czf - --sort=name --owner=0 --group=0 -C build/installer kernel | base64; } > $(INSTALLER).tmp
	@chmod 755 $(INSTALLER).tmp && mv $(INSTALLER).tmp $(INSTALLER)
	@echo "已生成 $(INSTALLER)（$$(du -h $(INSTALLER) | cut -f1)）：拷到要接入的电脑上，运行 sh nano-kernel-install.sh"

clean:
	$(MAKE) -C kernel clean
	rm -f gateway/nano-gateway
	rm -rf dist build
