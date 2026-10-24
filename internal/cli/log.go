package cli

import (
	"errors"
	"fmt"
	logpkg "log"
	"os"

	"github.com/golang-migrate/migrate/v4"
)

// exitCodeStopped is the dedicated, non-zero process exit code used when
// the execution was stopped by a graceful stop request (first interrupt
// after waiting for the safe point, or second interrupt forcing exit).
// It is intentionally distinct from 0 (success) and 1 (failure).
const exitCodeStopped = 5

// Log represents the logger
type Log struct {
	verbose bool
}

// Printf prints out formatted string into a log
func (l *Log) Printf(format string, v ...interface{}) {
	if l.verbose {
		logpkg.Printf(format, v...)
	} else {
		fmt.Fprintf(os.Stderr, format, v...)
	}
}

// Println prints out args into a log
func (l *Log) Println(args ...interface{}) {
	if l.verbose {
		logpkg.Println(args...)
	} else {
		fmt.Fprintln(os.Stderr, args...)
	}
}

// Verbose shows if verbose print enabled
func (l *Log) Verbose() bool {
	return l.verbose
}

func (l *Log) fatal(args ...interface{}) {
	l.Println(args...)
	os.Exit(1)
}

// fatalCode prints args and exits with the given exit code.
func (l *Log) fatalCode(code int, args ...interface{}) {
	l.Println(args...)
	os.Exit(code)
}

func (l *Log) fatalErr(err error) {
	if errors.Is(err, migrate.ErrStop) {
		// Distinguish a stopped execution from a failed one with a
		// dedicated exit code; the stopped conclusion and the version
		// table state have already been reported by the engine.
		l.fatalCode(exitCodeStopped, "error:", err)
	}
	l.fatal("error:", err)
}
