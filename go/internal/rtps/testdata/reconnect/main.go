package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: reconnect PYTHON PUBLISHER_SCRIPT")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := rtps.NewParticipant(rtps.Config{DomainID: 42, Interface: "lo"})
	if err != nil {
		return err
	}
	defer p.Close()
	go p.Run(ctx)
	var old []rtps.Endpoint
	for _, phase := range []string{"initial", "restart"} {
		pub := exec.Command(os.Args[1], os.Args[2], phase)
		pub.Stdout = os.Stdout
		pub.Stderr = os.Stderr
		if err := pub.Start(); err != nil {
			return err
		}
		stop := func() { _ = pub.Process.Signal(syscall.SIGTERM); _ = pub.Wait() }
		wanted := 4
		if phase == "restart" {
			wanted = 8
		}
		subscribed := map[rtps.GUID]rtps.Endpoint{}
		received := map[rtps.GUID]int{}
		deadline := time.Now().Add(45 * time.Second)
		tick := time.NewTicker(100 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case s := <-p.Samples():
				if _, ok := subscribed[s.Writer]; ok {
					received[s.Writer]++
				}
			case <-tick.C:
				for _, ep := range p.Endpoints() {
					if !strings.HasPrefix(ep.Topic, "rt/thermal/") {
						continue
					}
					isOld := false
					for _, o := range old {
						if ep.GUID == o.GUID {
							isOld = true
						}
					}
					if isOld {
						continue
					}
					if _, ok := subscribed[ep.GUID]; !ok {
						if err := p.Subscribe(ep); err != nil {
							stop()
							return err
						}
						subscribed[ep.GUID] = ep
						fmt.Printf("%s subscribed %s %s\n", phase, ep.Topic, ep.GUID)
					}
				}
			}
			ready := len(received) == wanted
			for _, n := range received {
				if n < 2 {
					ready = false
				}
			}
			if ready {
				break
			}
		}
		tick.Stop()
		fmt.Printf("%s RESULT discovered=%d received=%d wanted=%d stats=%+v\n", phase, len(subscribed), len(received), wanted, p.Stats())
		stop()
		for _, ep := range subscribed {
			p.Unsubscribe(ep.GUID)
			old = append(old, ep)
		}
		for writer, count := range received {
			if count < 2 {
				return fmt.Errorf("FAIL: %s writer %s delivered only %d samples", phase, writer, count)
			}
		}
		if len(received) != wanted {
			return fmt.Errorf("FAIL: %s received %d/%d topics", phase, len(received), wanted)
		}
		time.Sleep(time.Second)
	}
	fmt.Println("PASS: retained Wendy RTPS participant receives raw and color topics after FastDDS publisher replacement")
	return nil
}
