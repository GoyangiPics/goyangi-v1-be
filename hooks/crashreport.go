package hooks

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Crash reports.
//
// A Go crash — an unrecovered panic outside a request handler, or a fatal
// runtime error such as concurrent map writes, which nothing can recover —
// kills the process and prints its trace to stderr. In production stderr goes
// to a log file on the host, which can only be read from that machine; the
// updater's own system_logs note says no more than "exit status 2".
//
// So the runtime is also told to write its crash output to a file in pb_data,
// and the next boot (the updater restarts the server within seconds) moves
// that file aside and files its contents in system_logs, where it can be read
// from the admin UI. Clean exits — drains, restarts — leave the file empty, so
// only real crashes are reported.

const (
	crashFile     = "crash.log"
	crashPrevFile = "crash.prev.log"
	// system_logs.message caps at 5000; the headline and the crashing
	// goroutine come first in a Go trace, so the start is the part to keep.
	crashMessageMax = 4500
	// system_logs.context caps at 100000 bytes of JSON.
	crashContextMax = 90000
)

// RegisterCrashReport must run first in main, right after pocketbase.New, so
// a crash during the rest of setup is captured too.
func RegisterCrashReport(app *pocketbase.PocketBase) {
	dir := app.DataDir()
	path := filepath.Join(dir, crashFile)

	// Read and move the previous run's report out of the way before the new
	// file truncates it. Kept as crash.prev.log so it survives a failure to
	// write it to system_logs.
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("⚠️  crash report: could not read %s: %v", path, err)
	}
	report := strings.TrimSpace(string(previous))
	if report != "" {
		if err := os.WriteFile(filepath.Join(dir, crashPrevFile), previous, 0o644); err != nil {
			log.Printf("⚠️  crash report: could not keep the previous report: %v", err)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("⚠️  crash report: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("⚠️  crash report: could not open %s: %v", path, err)
		return
	}
	// SetCrashOutput duplicates the descriptor, so ours can be closed.
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Printf("⚠️  crash report: %v", err)
	}
	_ = f.Close()

	if report == "" {
		return
	}
	log.Printf("💥 the previous run crashed; its trace is in %s", crashPrevFile)
	// OnServe rather than OnBootstrap: system_logs is ensured at the end of
	// bootstrap, and the updater's own "restarted after unexpected exit" note
	// is filed at serve time too, so the two land next to each other.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		LogServerWarning("previous run crashed: "+truncate(report, crashMessageMax), map[string]any{
			"trace": truncate(report, crashContextMax),
		})
		return e.Next()
	})
}
