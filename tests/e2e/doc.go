// Package e2e 是 site 插件的 docker 端到端验收（需 -tags e2e 才编译）。
//
// 默认 go test ./... 不跑本包：端到端依赖本机 docker 与 nginx:alpine 镜像，
// 不把容器运行时变成单元门禁的前置条件。
package e2e
