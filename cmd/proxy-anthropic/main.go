package main

import (
"fmt"
"log"
"net/http"
"os"

proxy "github.com/kyungw00k/anthropic-openai-proxy-go"
)

func main() {
listen := os.Getenv("LISTEN_ADDR")
if listen == "" {
listen = "127.0.0.1:8473"
}
upstream := os.Getenv("UPSTREAM_URL")
if upstream == "" {
upstream = "https://opencode.ai/zen/go/v1/chat/completions"
}
key := os.Getenv("UPSTREAM_KEY")
srv := proxy.NewServer(
upstream, key,
proxy.WithModelMap(map[string]string{
"GO/deepseek-v4-flash":            "deepseek-v4-flash",
"GO/minimax-m3":                   "minimax-m3",
"opencode-go/deepseek-v4-flash":   "deepseek-v4-flash",
}),
)
mux := http.NewServeMux()
mux.Handle("/v1/messages", srv.Handler())
mux.Handle("/v1/chat/completions", srv.Handler())
mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
fmt.Fprintf(w, "status=ok proxy=anthropic-to-openai upstream=%s\n", upstream)
})
log.Printf("proxy-anthropic: listening on %s -> %s", listen, upstream)
if err := http.ListenAndServe(listen, mux); err != nil {
log.Fatal(err)
}
}
