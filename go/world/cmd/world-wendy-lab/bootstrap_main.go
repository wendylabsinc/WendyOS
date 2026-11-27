//go:build world_wendy_boot

package main

import (
	"log"
	"os"
	"syscall"
)

func main() {
	const dataDir = "/data"
	path, err := prepareNode("/etc/wendy-world/node-seed.json", dataDir)
	if err != nil {
		log.Fatal(err)
	}
	// Wendy creates root-owned persistent volumes. This unique volume belongs
	// only to this simulation app. World itself runs without root credentials.
	if os.Geteuid() == 0 {
		if err = os.Chown(dataDir, 65532, 65532); err != nil {
			log.Fatal(err)
		}
		if err = os.Chmod(dataDir, 0700); err != nil {
			log.Fatal(err)
		}
		if err = os.Chown(path, 65532, 65532); err != nil {
			log.Fatal(err)
		}
		if err = syscall.Setgroups([]int{}); err != nil {
			log.Fatal(err)
		}
		if err = syscall.Setgid(65532); err != nil {
			log.Fatal(err)
		}
		if err = syscall.Setuid(65532); err != nil {
			log.Fatal(err)
		}
	} else if os.Geteuid() != 65532 || os.Getegid() != 65532 {
		log.Fatal("bootstrap requires root initialization or UID/GID 65532")
	}
	log.Print("starting simulation observation node as UID 65532; no task executor configured")
	if err = syscall.Exec("/usr/local/bin/worldd", []string{"worldd", "--config", path}, os.Environ()); err != nil {
		log.Fatal(err)
	}
}
