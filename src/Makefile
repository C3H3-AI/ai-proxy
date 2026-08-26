# Makefile — ha-ai-proxy Go 构建 + 上游同步
# 用法:
#   make sync      # 从上游生成式同步同源 internal 包（带 GENERATED 头）
#   make build     # 编译 serverd/ctl/login/logintrae/loginqoder
#   make vet       # go vet
#   make all       # sync + build + vet

SRC := .
UPSTREAM_DIR ?= D:/ai-hub/_upstream-wild-work

.PHONY: all sync build vet

sync:
	UPSTREAM_DIR=$(UPSTREAM_DIR) ./sync_vendor.sh

build:
	cd $(SRC) && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/serverd ./cmd/serverd
	cd $(SRC) && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ctl ./cmd/ctl
	cd $(SRC) && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login
	cd $(SRC) && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/logintrae ./cmd/logintrae
	cd $(SRC) && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/loginqoder ./cmd/login_qoder

vet:
	cd $(SRC) && go vet ./internal/...

all: sync build vet
