package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/jech/portmap"
)

func syntax() {
	log.Fatal("Syntax: portmap-example port")
}

func main() {
	flag.Parse()

	if flag.NArg() != 1 {
		syntax()
	}

	port, err := strconv.Atoi(flag.Arg(0))
	if err != nil || port <= 0 || port > 0xFFFF {
		syntax()
	}

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		portmap.Map(
			ctx, "portmap-example", uint16(port), portmap.All,
			func(proto string, status portmap.Status, err error) {
				if err != nil {
					log.Printf("Mapping error: %v", err)
				} else if status.Lifetime > 0 {
					log.Printf("Mapped %v %v->%v for %v",
						proto,
						status.Internal, status.External,
						status.Lifetime,
					)
				} else {
					log.Printf("Unmapped %v %v",
						proto, status.Internal,
					)
				}
			},
		)
		wg.Done()
	}()

	log.Print("Mappings established, press Ctrl-C to unmap")

	<-terminate

	cancel()
	wg.Wait()
}
