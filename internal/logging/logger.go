// Package logging configures process-wide application logging.
package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// Setup writes standard Go log output to stdout and, when configured, to a
// persistent log file. The returned close function must be called before exit.
func Setup(logFile string) (func() error, error) {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)

	if logFile == "" {
		log.SetOutput(os.Stdout)
		return func() error { return nil }, nil
	}

	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	log.SetOutput(io.MultiWriter(os.Stdout, file))
	return file.Close, nil
}
