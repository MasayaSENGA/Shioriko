package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shioriko/internal/service"
)

// runHeadless runs the service in the foreground without any UI, logging to
// stderr. This is the original command-line mode.
func runHeadless(c config) {
	log.SetFlags(log.Ltime)
	log.SetPrefix("shioriko: ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, let a second one kill the process at once
		// instead of waiting for a long transfer to finish.
		<-ctx.Done()
		stop()
	}()

	if !waitForInstanceLock(ctx, c.once) {
		os.Exit(1)
	}

	opts := c.serviceOptions()
	opts.Reveal = func() bool { return c.reveal }
	service.New(opts).Run(ctx)
}

// waitForInstanceLock takes the single-instance lock. If another Shioriko
// holds it, a one-shot run gives up, while a long-running one (for example
// under launchd, which would otherwise keep restarting it) waits for the
// other process to exit.
func waitForInstanceLock(ctx context.Context, giveUp bool) bool {
	dir := stateDir()
	logged := false
	for {
		err := acquireInstanceLock(dir)
		if err == nil {
			return true
		}
		if !errors.Is(err, errAlreadyRunning) {
			log.Printf("cannot take the instance lock: %v", err)
			return false
		}
		if giveUp {
			log.Printf("another Shioriko is already running (pid %d); quit it first", lockHolder(dir))
			return false
		}
		if !logged {
			log.Printf("another Shioriko is already running (pid %d); waiting for it to exit", lockHolder(dir))
			logged = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
}
