# kore-plugin-site 构建入口：build / dist / check / clean。
SHELL := /bin/bash

ID := site
BIN := bin/$(ID)
DIST := dist
VERSION := $(shell sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' info.json | head -n 1)
PACKAGE := $(DIST)/$(ID)-$(VERSION).tar.gz

.PHONY: build dist check clean

# 静态构建：G-16 兼容性基线（CGO_ENABLED=0 + GOAMD64=v1 + trimpath + -s -w）。
build:
	CGO_ENABLED=0 GOAMD64=v1 go build -trimpath -buildvcs=false -ldflags="-s -w" -o $(BIN) .

# 打包：info.json（回填 size_bytes）+ bin/site + SHA256SUMS，并附 .sha256。
# 本插件无 web/ 前端，故不打 web 目录。
dist: build
	@set -e; \
	rm -rf $(DIST)/stage $(PACKAGE) $(PACKAGE).sha256; \
	mkdir -p $(DIST)/stage/bin; \
	cp $(BIN) $(DIST)/stage/bin/$(ID); \
	base=$$(find $(DIST)/stage/bin -type f -printf '%s\n' | awk '{sum += $$1} END {print sum + 0}'); \
	size=$$base; \
	for _ in 1 2 3; do \
		sed "s/\"size_bytes\": 0/\"size_bytes\": $$size/" info.json > $(DIST)/stage/info.json; \
		next=$$((base + $$(stat -c%s $(DIST)/stage/info.json))); \
		if [ "$$next" = "$$size" ]; then break; fi; \
		size=$$next; \
	done; \
	( cd $(DIST)/stage && sha256sum info.json bin/$(ID) > SHA256SUMS ); \
	tar -czf $(PACKAGE) -C $(DIST)/stage info.json bin SHA256SUMS; \
	( cd $(DIST) && sha256sum $(ID)-$(VERSION).tar.gz > $(ID)-$(VERSION).tar.gz.sha256 ); \
	rm -rf $(DIST)/stage; \
	echo "打包完成: $(PACKAGE)"

# 门禁：gofmt（有输出即失败）→ go vet → go test。
check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt 需要格式化："; echo "$$out"; exit 1; fi; echo "gofmt: clean"
	go vet ./...
	go test -timeout 200s ./...

clean:
	rm -rf bin dist
	rm -f go.work.sum
