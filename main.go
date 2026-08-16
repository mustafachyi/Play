package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"play/internal/app"
)

const version = "0.1.0"

func main() {
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
