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
	os.Exit(run())
}

func run() int {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// NotifyContext preserves the signal cause; forward plain cancellation for a quiet stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopForward := context.AfterFunc(signals, cancel)
	defer stopForward()
	err := voice.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if code := voice.ExitCode(err); code != 0 {
		log.New(os.Stderr, "", log.LstdFlags|log.LUTC).Print("Error: ", err)
		return code
	}
	return 0
}
