package bootstrap

import (
	"log"
	"os"
	"runtime/debug"

	"github.com/pbnjay/memory"
)

// memLimitFraction is the share of total system RAM used as the Go soft memory
// limit when one is not configured explicitly. It is deliberately high: the
// limit should only engage near the real ceiling so normal-sized repos run with
// the default GC, while a kernel-sized load is pushed into more aggressive GC
// (trading CPU) instead of being OOM-killed. Setting it much lower risks a GC
// death-spiral, which is worse for a long-running server than a clean exit.
const memLimitFraction = 0.90

// ConfigureRuntime applies process-wide runtime settings that keep enola
// well-behaved when a single large repository (e.g. the Linux kernel) is loaded
// into the in-memory fact store. It is safe to call once at startup from a
// binary's main(); library callers of NewEngine are intentionally left alone so
// importing the engine never mutates global runtime state.
//
// Today it sets a soft memory limit (Go's GOMEMLIMIT). The Go runtime responds
// by running the GC more aggressively as the heap approaches the limit, which
// caps RSS growth and avoids the OS OOM-killer taking down the whole
// long-running MCP server (which would lose every loaded snapshot).
//
// An explicit GOMEMLIMIT environment variable always wins: the runtime already
// honors it, and an operator's choice must not be overridden. Only when it is
// unset do we auto-detect total system RAM and derive a limit from it.
func ConfigureRuntime() {
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		// The runtime has already applied the env value; defer to it.
		return
	}

	total := memory.TotalMemory()
	if total == 0 {
		// Detection failed (unsupported platform); leave the runtime default of
		// no limit rather than guess.
		log.Printf("[runtime] could not detect system memory; leaving GOMEMLIMIT unset")
		return
	}

	limit := int64(float64(total) * memLimitFraction)
	debug.SetMemoryLimit(limit)
	log.Printf("[runtime] soft memory limit set to %d MiB (%.0f%% of %d MiB system RAM); override with GOMEMLIMIT",
		limit>>20, memLimitFraction*100, total>>20)
}
