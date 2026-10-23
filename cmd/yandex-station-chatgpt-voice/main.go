package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"yandex-station-chatgpt-voice/internal/voice"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := voice.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
