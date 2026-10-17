package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/apexracing/tracklogic-peripherals/pkg/input"
	"os"
	"os/signal"
)

func main() {
	keyFilter := flag.Uint("key", 0, "include this keyboard virtual key in monitor output (0: controllers only)")
	capture := flag.Bool("capture", false, "record one full press and release")
	stopFile := flag.String("stop-file", "", "optional diagnostic stop signal")
	diagnostics := flag.Bool("diagnostics", false, "include native input lifecycle errors on stderr")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *stopFile != "" {
		go watchStop(ctx, cancel, *stopFile)
	}
	var options []input.Option
	if *diagnostics {
		options = append(options, input.WithDiagnostics(os.Stderr))
	}
	m := input.NewManager(options...)
	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer m.Close()
	encoder := json.NewEncoder(os.Stdout)
	if *capture {
		b, err := m.Capture(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		encoder.Encode(b)
		return
	}
	fmt.Fprintln(os.Stderr, "Native VR input ready (in-process); stop with Ctrl+C")
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-m.Errors():
			fmt.Fprintln(os.Stderr, err)
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.Type != "input" || e.Binding.Kind != input.Key || e.Binding.Code == uint32(*keyFilter) {
				encoder.Encode(e)
			}
		}
	}
}
