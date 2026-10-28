package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/doctor"
	"github.com/cfcromn/Throt/internal/platform"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdDoctor is 'throt doctor'. It reports even when the config is invalid
// or the daemon is down.
func cmdDoctor(ctx context.Context, e env, args []string) error {
	if err := e.atMostOneArg(args); err != nil {
		return err
	}
	cfg, cfgErr := e.loadConfig()
	if cfgErr != nil {
		cfg = config.Default()
	}
	_, statErr := os.Stat(e.cfgPath)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := daemonClient{socket: cfg.SocketPath}
	r := doctor.Collect(ctx, doctor.Inputs{
		ConfigPath: e.cfgPath,
		SocketPath: cfg.SocketPath,
		ConfigErr:  cfgErr,
		ConfigFile: statErr == nil,
		Ping: func(ctx context.Context) (app.PingResult, error) {
			var p app.PingResult
			err := transport.Call(ctx, cfg.SocketPath, "ping", nil, &p)
			return p, err
		},
		Status:    c.Status,
		Host:      platform.NewHostSampler(),
		Processes: platform.NewProcessSampler(),
		Adapters:  app.Adapters(cfg.Agents),
		UID:       uint32(os.Getuid()),
	})
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
