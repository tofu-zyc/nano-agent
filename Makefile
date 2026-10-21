.PHONY: all kernel gateway test run clean

all: kernel gateway

kernel:
	$(MAKE) -C kernel

gateway:
	cd gateway && go build -o nano-gateway ./cmd/nano-gateway

# 单元测试 + 宿主层集成测试 + 端到端测试（模型用 mock，不花钱）
test: all
	$(MAKE) -C kernel test
	cd gateway && go test -race ./...
	./scripts/e2e.sh

# 本机一键运行，在终端里直接聊天
run: all
	./scripts/run-local.sh

clean:
	$(MAKE) -C kernel clean
	rm -f gateway/nano-gateway
