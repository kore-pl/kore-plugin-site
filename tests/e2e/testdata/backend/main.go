// 命令 kore-site-e2e-backend 是 docker e2e 用的极简静态文件后端。
//
// 反代验收要求 upstream 是容器内的 127.0.0.1:<port>：nginx:alpine 的 busybox
// 不含 httpd applet，容器里也没有 python，因此这里用 CGO_ENABLED=0 构建一个
// 静态二进制、docker cp 进容器后常驻，保证"环回地址 + 另一个端口"语义真实。
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18990", "监听地址")
	dir := flag.String("dir", ".", "静态文件目录")
	flag.Parse()
	// 默认 FileServer 会列目录；e2e 只取固定文件，行为足够。
	log.Fatal(http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))))
}
