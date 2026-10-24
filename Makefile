.PHONY: all kernel gateway web test test-ui test-win test-install test-macos run chat clean install installer install-kernel release release-kernel release-linux release-windows release-macos release-gateway

all: kernel gateway

kernel:
	$(MAKE) -C kernel

gateway: kernel
	cd gateway && go build -ldflags "-X nano-agent/gateway/internal/version.Version=$(VERSION)" -o nano-gateway ./cmd/nano-gateway

# 重新构建网页界面（改了 gateway/web 下的前端代码之后；构建结果提交在仓库里，平时不需要 Node）
web:
	cd gateway/web && npm install --no-audit --no-fund && npm run check && npm run build

# 单元测试 + 宿主层集成测试 + 端到端测试（模型用 mock，不花钱）
test: all
	$(MAKE) -C kernel test
	cd gateway && go test -race ./...
	./scripts/e2e.sh
	python3 scripts/e2e-control.py
	@if python3 -c "import playwright" 2>/dev/null; then python3 scripts/check-web-ui.py; \
	else echo "（没有 playwright，跳过网页界面检查：pip install playwright && python3 -m playwright install chromium-headless-shell）"; fi
	@if [ "$(NANO_TEST_NO_WINDOWS)" != "1" ] && [ -f kernel/build/nano-kernel.exe ] && [ -x /mnt/c/Windows/System32/cmd.exe ]; then \
	  python3 scripts/e2e-win.py; \
	else echo "（没有 Windows 内核或 WSL interop，跳过 Windows 内核端到端测试）"; fi
	$(MAKE) test-install
	$(MAKE) test-macos

# 只跑 Windows 执行器和内核的测试（需要 zig、build/tools/rg.exe、WSL interop）
# 网页界面检查：浏览器把拓扑图的节点逐个点开，每个配置项恰好在一个节点里、改了能保存（要 playwright）
test-ui: all
	python3 scripts/check-web-ui.py

test-win:
	$(MAKE) -C kernel test-win
	python3 scripts/e2e-win.py

# macOS 只做交叉编译检查；没有 Mac，实际运行测试交给 macOS runner
test-macos:
	@if [ -x $(ZIG) ]; then $(MAKE) -C kernel cross-macos; \
	else echo "（没有 zig，跳过 macOS 交叉编译）"; fi

# 离线安装测试：make release 的包在临时 HOME / USERPROFILE 里装、配对、升级、卸载
test-install:
	@if [ -x $(ZIG) ]; then $(MAKE) --no-print-directory release VERSION=test >/dev/null && \
	  python3 scripts/test-install-linux.py && python3 scripts/test-oneliner.py && python3 scripts/test-update.py; \
	else echo "（没有 zig，跳过 Linux 离线安装测试和一行安装测试）"; fi
	@if [ "$(NANO_TEST_NO_WINDOWS)" != "1" ] && [ -x $(ZIG) ] && [ -x /mnt/c/Windows/System32/cmd.exe ]; then python3 scripts/test-install-win.py; \
	else echo "（没有 zig 或 WSL interop，跳过 Windows 离线安装测试）"; fi

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

# ---- 本地发布包（不创建仓库、不推送；以后接 release 下载） ----

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
DIST := dist
RELEASE := build/release
ZIG := build/tools/zig/zig
RG := build/tools/rg

$(ZIG):
	scripts/fetch-zig.sh

$(RG)/linux-x86_64:
	scripts/fetch-rg.sh --all

release: release-kernel release-gateway
	@cp scripts/install.sh scripts/install.ps1 $(DIST)/
	@echo '$(VERSION)' > $(DIST)/VERSION
	@cd $(DIST) && sha256sum *-$(VERSION)-*.tar.gz *-$(VERSION)-*.zip install.sh install.ps1 VERSION > SHA256SUMS
	@echo "发布包在 $(DIST)/："
	@ls -1 $(DIST)

release-kernel: release-linux release-windows release-macos

release-linux: $(ZIG) $(RG)/linux-x86_64
	$(MAKE) -C kernel cross-linux
	@mkdir -p $(DIST) $(RELEASE)
	@for arch in x86_64 aarch64; do \
	  dir=$(RELEASE)/nano-kernel-$(VERSION)-linux-$$arch; \
	  rm -rf $$dir; mkdir -p $$dir; \
	  cp kernel/build/cross/linux-$$arch/nano-kernel $$dir/; \
	  cp kernel/build/cross/linux-$$arch/nano-exec $$dir/; \
	  cp $(RG)/linux-$$arch $$dir/rg; \
	  cp scripts/install-kernel.sh $$dir/; \
	  echo '$(VERSION)' > $$dir/VERSION; \
	  tar czf $(DIST)/nano-kernel-$(VERSION)-linux-$$arch.tar.gz -C $(RELEASE) nano-kernel-$(VERSION)-linux-$$arch; \
	done

release-windows: $(ZIG) $(RG)/windows-x86_64.exe
	$(MAKE) -C kernel windows-exec windows-kernel
	@mkdir -p $(DIST) $(RELEASE)
	@dir=$(RELEASE)/nano-kernel-$(VERSION)-windows-x86_64; \
	  rm -rf $$dir; mkdir -p $$dir; \
	  cp kernel/build/nano-kernel.exe $$dir/; \
	  cp kernel/build/nano-exec.exe $$dir/; \
	  cp $(RG)/windows-x86_64.exe $$dir/rg.exe; \
	  cp scripts/install-kernel.ps1 $$dir/; \
	  echo '$(VERSION)' > $$dir/VERSION; \
	  python3 scripts/zipdir.py $$dir "$(DIST)/nano-kernel-$(VERSION)-windows-x86_64.zip"

release-macos: $(ZIG) $(RG)/linux-x86_64
	$(MAKE) -C kernel cross-macos
	@mkdir -p $(DIST) $(RELEASE)
	@for arch in aarch64 x86_64; do \
	  dir=$(RELEASE)/nano-kernel-$(VERSION)-macos-$$arch; \
	  rm -rf $$dir; mkdir -p $$dir; \
	  cp kernel/build/cross/macos-$$arch/nano-kernel $$dir/; \
	  cp kernel/build/cross/macos-$$arch/nano-exec $$dir/; \
	  cp $(RG)/macos-$$arch $$dir/rg; \
	  cp scripts/install-kernel.sh $$dir/; \
	  echo '$(VERSION)' > $$dir/VERSION; \
	  tar czf $(DIST)/nano-kernel-$(VERSION)-macos-$$arch.tar.gz -C $(RELEASE) nano-kernel-$(VERSION)-macos-$$arch; \
	done

release-gateway: $(ZIG) $(RG)/linux-x86_64
	$(MAKE) -C kernel cross-linux
	@mkdir -p $(DIST) $(RELEASE)
	@for pair in amd64:x86_64 arm64:aarch64; do \
	  goarch=$${pair%%:*}; karch=$${pair##*:}; \
	  dir=$(RELEASE)/nano-gateway-$(VERSION)-linux-$$karch; \
	  rm -rf $$dir; mkdir -p $$dir/lib $$dir/personas $$dir/skills; \
	  (cd gateway && GOOS=linux GOARCH=$$goarch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X nano-agent/gateway/internal/version.Version=$(VERSION)" -o ../$$dir/nano-gateway ./cmd/nano-gateway); \
	  cp kernel/build/cross/linux-$$karch/nano-kernel $$dir/lib/; \
	  cp kernel/build/cross/linux-$$karch/nano-exec $$dir/lib/; \
	  cp $(RG)/linux-$$karch $$dir/lib/rg; \
	  cp gateway.example.toml $$dir/; \
	  cp -a personas/. $$dir/personas/; \
	  cp -a skills/. $$dir/skills/; \
	  cp scripts/install-gateway.sh $$dir/; \
	  echo '$(VERSION)' > $$dir/VERSION; \
	  tar czf $(DIST)/nano-gateway-$(VERSION)-linux-$$karch.tar.gz -C $(RELEASE) nano-gateway-$(VERSION)-linux-$$karch; \
	done

clean:
	$(MAKE) -C kernel clean
	rm -f gateway/nano-gateway
	rm -rf dist build
