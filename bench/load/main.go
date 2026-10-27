package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(args []string) error {
	if len(args) > 0 && args[0] == "run" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		return runCommand(ctx, args[1:])
	}
	if len(args) == 1 && args[0] == "schema" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(reportSchema())
	}
	if len(args) == 3 && args[0] == "delta" {
		before, err := readReport(args[1])
		if err != nil {
			return err
		}
		after, err := readReport(args[2])
		if err != nil {
			return err
		}
		return printDelta(os.Stdout, before, after)
	}
	return fmt.Errorf("usage: load run [options] | delta <before.json> <after.json> (received %d arguments)", len(args))
}
