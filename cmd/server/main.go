// Command server runs the auction application.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Lexv0lk/auction/internal/app"
	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/observability"
)

var errPositionalArguments = errors.New("server takes no positional arguments; use --help")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(out, "Usage: server [--help]\n\nRun the auction application. Configuration is read from the environment.")
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return err
	}
	if flags.NArg() != 0 {
		return errPositionalArguments
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, cfg, observability.NewLogger(out, cfg.LogLevel))
}
