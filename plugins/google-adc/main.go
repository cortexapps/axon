// Command google-adc prints a Google Authorization header value for the relay to
// inject.
//
// It is a credential source for the accept file, referenced as
// ${plugin:google-adc}. Unlike the plugins in PLUGIN_DIRS it is not looked up by
// name: the agent resolves it to a fixed path, so a file dropped beside the
// customer's plugins cannot stand in for it.
//
// It is a separate binary rather than code in the agent so the token it mints
// survives the agent's own restarts. See the gcp package for why that matters.
//
// Contract: the credential goes to stdout with no trailing newline, everything
// else goes to stderr, and a non-zero exit means no credential was produced. The
// caller turns that into a 502 rather than an upstream request.
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
		// Redacted again here even though the provider already redacts: this is
		// the last point before the text leaves the process, and a token exchange
		// reports failure by echoing the request.
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
	// A stack trace through this binary says nothing an operator can act on, and
	// the caller logs whatever reaches stderr. The message is the diagnostic.
	config.DisableStacktrace = true

	logger, err := config.Build()
	if err != nil {
		return zap.NewNop()
	}
	return logger
}
