// Command throtd is the Throt daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/platform"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "throtd:", err)
		os.Exit(1)
	}
}

func run() error {
	defPath, _ := config.DefaultPath()
	cfgPath := flag.String("config", defPath, "path to config file")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(app.Version)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return app.NewDaemon(cfg, platform.NewHostSampler(), platform.NewProcessSampler(), log).Run(ctx)
}
