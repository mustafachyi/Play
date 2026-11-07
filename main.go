package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"play/internal/app"
)

const version = "0.1.2"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--play-worker" {
		if app.RunWorker(context.Background()) != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := app.Run(ctx, version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()

	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		os.Exit(130)
	}

	fmt.Fprintf(os.Stderr, "play: %v\n", err)
	os.Exit(1)
}
