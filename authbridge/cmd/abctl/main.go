// Command abctl is an interactive terminal UI for inspecting AuthBridge's
// in-memory session store.
//
// Default mode opens a Namespaces → Pods picker, port-forwards the
// selected pod, and renders the session-events view. Pass --endpoint
// to skip the picker and connect directly (the pre-picker behavior).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/tui"
)

func main() {
	endpoint := flag.String("endpoint", "",
		"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl opens a Namespaces → Pods picker.")
	flag.Parse()

	// Friendly check: if picker mode and no kubectl, fail fast with a
	// clear message instead of a stack trace later.
	if *endpoint == "" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			fmt.Fprintln(os.Stderr, "abctl: kubectl not found on PATH; install it or pass --endpoint http://...")
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	opts := tui.RunOptions{Endpoint: *endpoint}
	if *endpoint == "" {
		opts.Lister = cluster.NewLister()
		opts.PortForwarder = cluster.NewPortForwarder()
	}
	if err := tui.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "abctl: %v\n", err)
		os.Exit(1)
	}
}
