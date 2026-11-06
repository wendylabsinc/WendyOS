package main

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/twin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
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
	server := &http.Server{Addr: "127.0.0.1:8791", Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go service.RunMapping()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		server.Shutdown(shutdown)
	}()
	log.Print("digital twin listening on 127.0.0.1:8791")
	if err = server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
