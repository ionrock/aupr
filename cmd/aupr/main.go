// Command aupr is the PR feedback daemon entry point.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dagster-io/aupr/internal/cli"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
