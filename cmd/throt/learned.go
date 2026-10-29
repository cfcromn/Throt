package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/transport"
)

const learnedUsage = "usage: throt learned [--forget SIGNATURE | --clear]"

// cmdLearned is 'throt learned [--forget SIGNATURE | --clear]'.
func cmdLearned(ctx context.Context, e env, args []string) error {
	p, err := parseLearnedArgs(args, e.stdout, e.stderr)
	if err != nil {
		return err
	}
	cfg := e.clientConfig()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var r app.LearnedResult
	if err := transport.Call(ctx, cfg.SocketPath, "learned", p, &r); err != nil {
		return err
	}
	if p.Clear || p.Forget != "" {
		fmt.Fprintf(e.stdout, "forgot %d learned command(s)\n", r.Forgotten)
		return nil
	}
	printLearned(e.stdout, r)
	return nil
}

// parseLearnedArgs parses the arguments after 'learned'.
func parseLearnedArgs(args []string, stdout, stderr io.Writer) (app.LearnedParams, error) {
	var p app.LearnedParams
	fs := flag.NewFlagSet("throt learned", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), learnedUsage); fs.PrintDefaults() }
	fs.StringVar(&p.Forget, "forget", "", "forget one learned `SIGNATURE`, as listed")
	fs.BoolVar(&p.Clear, "clear", false, "forget every learned command")
	if err := parseFlags(fs, args, stdout); err != nil {
		return p, err
	}
	if fs.NArg() > 0 || p.Clear && p.Forget != "" {
		return p, usageLine(learnedUsage)
	}
	return p, nil
}

// printLearned lists the learned commands, most recently heavy first.
func printLearned(w io.Writer, r app.LearnedResult) {
	if len(r.Signatures) == 0 {
		fmt.Fprintln(w, "no learned heavy commands")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SIGNATURE\tPEAK MEM\tHEAVY RUNS\tLIGHT SINCE\tLAST HEAVY")
	for _, s := range r.Signatures {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", s.Signature, gib(s.PeakRSS), s.HeavyRuns, s.Misses, s.LastHeavy.Local().Format("2006-01-02 15:04"))
	}
	tw.Flush()
}
