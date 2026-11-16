// A transparent inbound-only test relay. TLS terminates at the real agent,
// whose container remains attached exclusively to its internal Docker network.
package main

import (
	"flag"
	"io"
	"net"
	"os"
)

func main() {
	target := flag.String("target", "", "fixed target address")
	flag.Parse()
	if net.ParseIP(*target) == nil {
		os.Exit(2)
	}
	for _, port := range []string{"50052", "5000"} {
		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			panic(err)
		}
		go func() {
			for {
				client, err := listener.Accept()
				if err != nil {
					return
				}
				go relay(client, net.JoinHostPort(*target, port))
			}
		}()
	}
	select {}
}
func relay(client net.Conn, target string) {
	defer client.Close()
	server, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(server, client); done <- struct{}{} }()
	go func() { io.Copy(client, server); done <- struct{}{} }()
	<-done
	client.Close()
	server.Close()
	<-done
}
