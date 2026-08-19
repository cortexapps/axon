// Command google-adc prints a Google Authorization header value for the relay to
// inject. An accept file references it as ${plugin:google-adc}.
//
// Contract: the credential goes to stdout with no trailing newline, everything
// else goes to stderr, and a non-zero exit means no credential was produced.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/cortexapps/axon/plugins/google-adc/internal/gcp"
	"github.com/cortexapps/axon/plugins/google-adc/internal/redact"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	probe := flag.Bool("probe", false,
		"check that a credential configuration exists and exit, without minting a token")
	verbose := flag.Bool("verbose", false, "log at debug level")
	flag.Parse()

	logger := newLogger(*verbose)
	defer logger.Sync()

	if err := gcp.RunPlugin(context.Background(), os.Stdout, logger, *probe); err != nil {
		fmt.Fprintln(os.Stderr, redact.Redact(err.Error()))
		os.Exit(1)
	}
}

// newLogger writes to stderr. Anything on stdout would be concatenated into the
// Authorization header.
func newLogger(verbose bool) *zap.Logger {
	level := zapcore.WarnLevel
	if verbose {
		level = zapcore.DebugLevel
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(level)
	config.OutputPaths = []string{"stderr"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.DisableStacktrace = true

	logger, err := config.Build()
	if err != nil {
		return zap.NewNop()
	}
	return logger
}
