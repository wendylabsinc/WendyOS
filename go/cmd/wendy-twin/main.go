package main

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/twin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--proxy" {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:8791", 5*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		go func() { copyProxyStream(conn, os.Stdin); conn.(*net.TCPConn).CloseWrite() }()
		copyProxyStream(os.Stdout, conn)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	directory := os.Getenv("WENDY_TWIN_DATA")
	if directory == "" {
		directory = "/data"
	}
	store, err := twin.NewStore(directory)
	if err != nil {
		log.Fatal(err)
	}
	socket := os.Getenv("WENDY_AGENT_SOCKET")
	if socket == "" {
		log.Fatal("WENDY_AGENT_SOCKET is required; grant the admin entitlement")
	}
	conn, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	service := &twin.Server{Store: store, Conn: conn, Context: ctx}
	listener, err := net.Listen("tcp", "127.0.0.1:8791")
	if err != nil {
		log.Fatal(err)
	}
	log.Print("digital twin listening on 127.0.0.1:8791")
	if err = service.ServeWithWorld(ctx, listener, twin.WorldOptions{
		ConfigPath: os.Getenv("WENDY_TWIN_WORLD_CONFIG"),
		Ready:      func(address string) { log.Printf("digital twin World simulation service listening on %s", address) },
	}); err != nil {
		log.Fatal(err)
	}
}
