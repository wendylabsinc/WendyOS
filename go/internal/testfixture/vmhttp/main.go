// vmhttp is an actual scratch-image guest application for VM lifecycle tests.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: vmhttp PORT NONCE")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		log.Fatal(err)
	}
	var count atomic.Uint64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"nonce": os.Args[2], "boot_id": strings.TrimSpace(string(boot)), "counter": count.Add(1)})
	})
	server := &http.Server{Addr: ":" + os.Args[1], Handler: handler, ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	log.Fatal(server.ListenAndServe())
}
