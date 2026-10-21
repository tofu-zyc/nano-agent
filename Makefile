.PHONY: all kernel gateway web test run chat clean

all: kernel gateway

kernel:
	$(MAKE) -C kernel

KERNEL_BUNDLE = gateway/internal/device/assets/kernel-src.tar.gz
KERNEL_SOURCE = $(wildcard kernel/src/*.c kernel/src/*.h kernel/third_party/cjson/*.c kernel/third_party/cjson/*.h) kernel/Makefile
DEVICE_SKILL = gateway/internal/device/assets/skills.tar.gz

$(DEVICE_SKILL): skills/device-connections/SKILL.md skills/device-connections/references/devices.md
	tar czf $@ --sort=name --owner=0 --group=0 --mtime=@0 -C skills device-connections

$(KERNEL_BUNDLE): $(KERNEL_SOURCE)
	tar czf $@ --sort=name --owner=0 --group=0 --mtime=@0 kernel/Makefile kernel/src kernel/third_party/cjson

gateway: kernel $(KERNEL_BUNDLE) $(DEVICE_SKILL)
	cd gateway && go build -o nano-gateway ./cmd/nano-gateway

# 重新构建网页界面（改了 gateway/web 下的前端代码之后；构建结果提交在仓库里，平时不需要 Node）
web:
	cd gateway/web && npm install --no-audit --no-fund && npm run check && npm run build

# 单元测试 + 宿主层集成测试 + 端到端测试（模型用 mock，不花钱）
test: all
	$(MAKE) -C kernel test
	cd gateway && go test -race ./...
	./scripts/e2e.sh

# 本机一键运行，在终端里直接聊天
run: all
	./scripts/run-local.sh

# 在终端里和正在运行的网关聊天（网关先 make run 跑起来）
chat:
	@test -x gateway/nano-gateway || $(MAKE) -s gateway
	@./gateway/nano-gateway chat -c gateway.toml

clean:
	$(MAKE) -C kernel clean
	rm -f gateway/nano-gateway
