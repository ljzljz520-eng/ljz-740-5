# 顶层构建: 原生动态库 -> Go 示例 -> 冒烟运行
GO ?= go

.PHONY: all lib example run test vet fmt clean

all: lib example

lib:
	$(MAKE) -C native

example: lib
	$(GO) build -o build/example ./cmd/example

run: lib
	$(GO) run ./cmd/example -file testdata/vectors.txt -k 3 -save build/index.bin

test: lib
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	$(MAKE) -C native clean
	rm -f build/example build/index.bin
