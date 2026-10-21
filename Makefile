.PHONY: all kernel gateway web test run clean

all: kernel gateway

kernel:
	$(MAKE) -C kernel

gateway:
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

clean:
	$(MAKE) -C kernel clean
	rm -f gateway/nano-gateway
