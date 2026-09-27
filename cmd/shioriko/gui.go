package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shioriko/internal/menubar"
	"shioriko/internal/service"
)

// runGUI runs Shioriko as a menu bar app. It never returns: the process exits
// once the service has stopped and unmounted.
func runGUI(c config) {
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("shioriko: ")
	lp := logPath()
	if f, err := openLogFile(lp); err == nil {
		logTo(f)
	} else {
		log.Printf("cannot open log file %s: %v", lp, err)
	}
	log.Printf("Shioriko %s starting (menu bar)", version)

	ui := menubar.New(version, lp)

	ctx, cancel := context.WithCancel(context.Background())
	ui.OnQuit(cancel)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		// A second signal now kills the process outright; the next launch
		// cleans up anything left mounted.
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		log.Printf("signal received; shutting down")
		ui.Quit()
	}()

	go func() {
		defer ui.Finished()
		if !takeInstanceLockGUI(ctx, ui) {
			return
		}
		opts := c.serviceOptions()
		opts.Reveal = ui.RevealOnMount
		opts.OnStatus = ui.SetStatus
		opts.OnEvent = ui.HandleEvent
		svc := service.New(opts)
		ui.Attach(svc)
		svc.Run(ctx)
		log.Printf("stopped")
	}()

	ui.Run()
}

// takeInstanceLockGUI takes the single-instance lock, asking the user what to
// do if another Shioriko is running. It returns false if the app should quit.
func takeInstanceLockGUI(ctx context.Context, ui *menubar.UI) bool {
	dir := stateDir()
	for ctx.Err() == nil {
		err := acquireInstanceLock(dir)
		if err == nil {
			// Daemons from before the lock existed run without taking it, so
			// look for the LaunchAgent itself too.
			if legacyAgentInstalled() {
				return offerLegacyAgentRemoval(ctx, ui)
			}
			return true
		}
		if !errors.Is(err, errAlreadyRunning) {
			log.Printf("cannot take the instance lock: %v", err)
			ui.Ask(ctx, "Shioriko を起動できません", err.Error(), "終了", "", false)
			return false
		}
		pid := lockHolder(dir)
		log.Printf("another Shioriko is already running (pid %d)", pid)
		ui.ShowMessage("別の Shioriko が実行中です", "終了するまでスマホをマウントできません")

		if legacyAgentInstalled() {
			if !offerLegacyAgentRemoval(ctx, ui) {
				return false
			}
			if waitForLock(ctx, dir, 10*time.Second) {
				return true
			}
			continue
		}

		choice := ui.Ask(ctx, "Shioriko はすでに実行中です",
			fmt.Sprintf("ターミナルなどから起動した別の Shioriko（プロセス ID %d）が動作しています。"+
				"そちらを終了してから「再試行」を選んでください。", pid),
			"終了", "再試行", false)
		if choice != 2 {
			return false
		}
	}
	return false
}

// offerLegacyAgentRemoval asks to stop and unregister the headless
// LaunchAgent, which cannot run alongside the menu bar app. It returns false
// if the user chose to quit instead.
func offerLegacyAgentRemoval(ctx context.Context, ui *menubar.UI) bool {
	ui.ShowMessage("バックグラウンド版の Shioriko が登録されています", "停止するまでスマホをマウントできません")
	choice := ui.Ask(ctx, "バックグラウンド版の Shioriko が登録されています",
		"ログイン時に自動起動する設定（LaunchAgent）で、メニューバーを使わない Shioriko が登録されています。"+
			"2つを同時には使えません。\n\n"+
			"「停止して続ける」を選ぶと、LaunchAgent を停止して登録を解除し、このアプリに切り替えます。"+
			"ログイン時に起動したい場合は、メニューの「設定」>「ログイン時に起動」をオンにしてください。",
		"停止して続ける", "終了", false)
	if choice != 1 {
		return false
	}
	if err := removeLegacyAgent(); err != nil {
		ui.Ask(ctx, "LaunchAgent を削除できませんでした", err.Error(), "終了", "", false)
		return false
	}
	return true
}

// waitForLock retries the instance lock for up to d.
func waitForLock(ctx context.Context, dir string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if acquireInstanceLock(dir) == nil {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
