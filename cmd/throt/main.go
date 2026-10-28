// Command throt is the Throt client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cfcromn/Throt/internal/config"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// -h printed the usage to stdout; asking for help is not a failure.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ee exitError
		if !errors.As(err, &ee) || ee.msg != "" {
			fmt.Fprintln(os.Stderr, "throt:", err)
		}
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

// globalFlags returns the flag set for the flags before the command.
func globalFlags() (fs *flag.FlagSet, cfgPath *string, limit *int) {
	fs = flag.NewFlagSet("throt", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usageText()); fs.PrintDefaults() }
	defPath, _ := config.DefaultPath()
	cfgPath = fs.String("config", defPath, "path to config file")
	limit = fs.Int("n", 50, "number of events for 'events'")
	return fs, cfgPath, limit
}

// parseFlags parses args with fs, whose Usage must write to fs.Output().
// On -h or -help it prints the usage to stdout and returns flag.ErrHelp,
// which main treats as success. Any other error comes back as a one-line
// usage error naming fs's -h, with nothing printed.
func parseFlags(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	usage, out := fs.Usage, fs.Output()
	fs.Usage = func() {}
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	fs.Usage = usage
	fs.SetOutput(out)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stdout)
		fs.Usage()
		fs.SetOutput(out)
	case err != nil:
		return usageError(fs.Name(), err.Error())
	}
	return err
}

// run parses the flags before the command and runs the command, "top" when
// there is none.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs, cfgPath, limit := globalFlags()
	fs.SetOutput(stderr)
	if err := parseFlags(fs, args, stdout); err != nil {
		return err
	}
	name, rest := "top", []string(nil)
	if fs.NArg() > 0 {
		name, rest = fs.Arg(0), fs.Args()[1:]
	}
	e := env{stdin: stdin, stdout: stdout, stderr: stderr, cfgPath: *cfgPath, limit: *limit}
	c, ok := lookup(name)
	if !ok {
		return usageError("throt", fmt.Sprintf("unknown command %q", name))
	}
	// -h right after the command never runs it.
	if !c.flags && len(rest) > 0 && isHelp(rest[0]) {
		fmt.Fprint(stdout, c.commandUsage())
		return flag.ErrHelp
	}
	return c.run(ctx, e, rest)
}
