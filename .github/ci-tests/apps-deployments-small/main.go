// A small real workload for ordinary Apps & Deployments E2Es.
// imageVersion is baked with -ldflags; deployment environment cannot change it.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

var imageVersion string

type response struct {
	App      string `json:"app"`
	Attempt  string `json:"attempt"`
	Version  string `json:"version"`
	Unit     string `json:"unit"`
	FileOnly string `json:"fileOnly"`
}

func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		log.Fatal("Linux boot id unavailable")
	}
	return strings.TrimSpace(string(b))
}

func main() {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version":
			fmt.Println(imageVersion)
			return
		case "--boot-id":
			fmt.Println(bootID())
			return
		default:
			log.Fatal("unsupported fixture argument")
		}
	}
	if imageVersion != "1.0.0" && imageVersion != "2.0.0" && imageVersion != "3.0.0" {
		log.Fatal("missing baked fixture version")
	}
	if imageVersion == "3.0.0" {
		log.Fatal("Intentional E2E startup failure")
	}
	if target := os.Getenv("E2E_FAIL_ON_BOOT_ID"); target != "" && target == bootID() {
		log.Fatal("Intentional E2E startup failure on the declared target")
	}
	read := func(name string) string {
		value, ok := os.LookupEnv(name)
		if !ok {
			log.Fatalf("missing fixture setting %s", name)
		}
		return value
	}
	payload := response{App: read("E2E_APP_ID"), Attempt: read("E2E_ATTEMPT"), Version: imageVersion,
		Unit: read("DISPLAY_UNIT"), FileOnly: read("FILE_ONLY")}
	port := read("E2E_HTTP_PORT")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		log.Fatal("invalid fixture HTTP port")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Fatal("fixture JSON encoding failed")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	log.Fatal(http.ListenAndServe(net.JoinHostPort("0.0.0.0", port), handler))
}
