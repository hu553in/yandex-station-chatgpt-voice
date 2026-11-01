package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"yandex-station-chatgpt-voice/internal/voice"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := voice.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil && err != context.Canceled {
		log.New(os.Stderr, "", log.LstdFlags|log.LUTC).Print("Error: ", err)
		os.Exit(1)
	}
}
