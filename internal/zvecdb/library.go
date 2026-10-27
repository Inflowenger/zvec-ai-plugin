// Package zvecdb wraps the embedded zvec engine for the plugin: it initialises
// the library, turns a settings profile into a data directory, keeps the
// collections under it open in a pool, and runs the reads and writes the node
// actions expose.
//
// zvec is an embedded database, not a server. There is no host to dial and no
// credential to hold: a "connection" is a directory on the machine this process
// runs on, and each collection is a subdirectory of it that the engine opens
// under an exclusive lock. That single fact shapes everything here — see Store.
package zvecdb

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	zvec "github.com/zvec-ai/zvec-go"
)

// Library-wide configuration is read from the environment rather than from a
// settings profile. zvec.Initialize configures the engine once per process, so
// a per-profile value would mean whichever account's node ran first silently
// decided the memory limit for every other account on this plugin.
const (
	envMemoryLimitMB   = "ZVEC_MEMORY_LIMIT_MB"
	envQueryThreads    = "ZVEC_QUERY_THREADS"
	envOptimizeThreads = "ZVEC_OPTIMIZE_THREADS"
	envLogLevel        = "ZVEC_LOG_LEVEL"
)

var (
	initOnce sync.Once
	initErr  error
)

// InitLibrary initialises the zvec engine, at most once per process, and
// reports what went wrong if it could not.
//
// A failure here is almost always a missing native library rather than a
// mistake in a flow, so callers are expected to keep serving forms and fail
// individual actions with [ErrLibrary] instead of refusing to start: an author
// can then still open the node's drawer and read what to install.
func InitLibrary() error {
	initOnce.Do(func() {
		config, err := libraryConfig()
		if err != nil {
			initErr = err
			return
		}
		if config != nil {
			defer config.Destroy()
		}
		if err := zvec.Initialize(config); err != nil {
			initErr = fmt.Errorf("%w: %s", ErrLibrary, err)
		}
	})
	return initErr
}

// ShutdownLibrary releases the engine's resources. It is called once, on the
// way out of main, after the collection pool has been closed — shutting the
// library down under an open collection would pull the ground out from beneath
// it.
func ShutdownLibrary() error {
	if initErr != nil {
		return nil
	}
	return zvec.Shutdown()
}

// Version is the zvec C-API version this process is running against, or an
// empty string when the library never loaded.
func Version() string {
	if initErr != nil {
		return ""
	}
	return zvec.GetVersion()
}

// libraryConfig builds the engine configuration from the environment, returning
// nil when nothing was set (which is zvec's own default configuration).
func libraryConfig() (*zvec.ConfigData, error) {
	memory, hasMemory, err := envUint(envMemoryLimitMB)
	if err != nil {
		return nil, err
	}
	queryThreads, hasQueryThreads, err := envUint(envQueryThreads)
	if err != nil {
		return nil, err
	}
	optimizeThreads, hasOptimizeThreads, err := envUint(envOptimizeThreads)
	if err != nil {
		return nil, err
	}
	level, hasLevel, err := envLogLevelValue()
	if err != nil {
		return nil, err
	}
	if !hasMemory && !hasQueryThreads && !hasOptimizeThreads && !hasLevel {
		return nil, nil
	}

	config := zvec.NewConfigData()
	if config == nil {
		return nil, fmt.Errorf("%w: cannot allocate the engine configuration", ErrLibrary)
	}
	fail := func(what string, err error) (*zvec.ConfigData, error) {
		config.Destroy()
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if hasMemory {
		if err := config.SetMemoryLimit(memory * 1024 * 1024); err != nil {
			return fail(envMemoryLimitMB, err)
		}
	}
	if hasQueryThreads {
		if err := config.SetQueryThreadCount(uint32(queryThreads)); err != nil {
			return fail(envQueryThreads, err)
		}
	}
	if hasOptimizeThreads {
		if err := config.SetOptimizeThreadCount(uint32(optimizeThreads)); err != nil {
			return fail(envOptimizeThreads, err)
		}
	}
	if hasLevel {
		if err := config.SetConsoleLog(level); err != nil {
			return fail(envLogLevel, err)
		}
	}
	return config, nil
}

func envUint(name string) (uint64, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 {
		return 0, false, fmt.Errorf("%s=%q is not a positive whole number", name, raw)
	}
	return value, true, nil
}

// logLevels is the console log level by name, as an operator would write it.
var logLevels = map[string]zvec.LogLevel{
	"debug": zvec.LogLevelDebug,
	"info":  zvec.LogLevelInfo,
	"warn":  zvec.LogLevelWarn,
	"error": zvec.LogLevelError,
	"fatal": zvec.LogLevelFatal,
}

func envLogLevelValue() (zvec.LogLevel, bool, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(envLogLevel)))
	if raw == "" {
		return 0, false, nil
	}
	level, ok := logLevels[raw]
	if !ok {
		return 0, false, fmt.Errorf("%s=%q is not one of debug, info, warn, error, fatal", envLogLevel, raw)
	}
	return level, true, nil
}
