package hooks

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// Supervised deploys.
//
// In production the server runs as a child of scripts/updater, which watches
// `main`, builds each new commit and swaps the binary in. The one thing it
// can't do from outside is know when a restart would lose work: an upload's
// encode lives only in a goroutine (nothing re-queues an unprocessed record at
// boot), and the bot batches Discord posts in memory. So the updater asks, by
// writing "drain" to this process's stdin, and the server exits on its own
// once nothing is in flight. "warn <message>" lines are written to system_logs.
//
// stdin rather than an HTTP route: the API is public through the tunnel, where
// every request arrives from 127.0.0.1, so a route would need its own secret.
// A pipe only the parent holds needs nothing, and works the same on Windows,
// where a process can't be sent SIGTERM.
//
// On Railway there is no updater: a deploy sends SIGTERM and kills the process
// RAILWAY_DEPLOYMENT_DRAINING_SECONDS later. The same wait runs then, from
// PocketBase's OnTerminate, bounded by that deadline (docs/railway-hosting.md).

// supervisedEnv is set to "1" by the updater on the child it starts.
const supervisedEnv = "GOYANGI_SUPERVISED"

const (
	// drainQuietPeriod is how long everything must stay idle before the server
	// stops accepting work. Bridges the gaps inside a multi-file upload, where
	// the queue empties for a moment between files.
	drainQuietPeriod = 30 * time.Second
	// drainGrace lets requests that slipped past the draining check before it
	// flipped finish creating their records (and enqueue their jobs).
	drainGrace     = 5 * time.Second
	drainPoll      = 2 * time.Second
	drainReportGap = 10 * time.Minute
)

var draining atomic.Bool

// Draining reports whether a supervised restart has stopped taking new work.
// The ingest API asks before accepting a message, so the other bot gets a
// retryable 503 rather than a half-done job.
func Draining() bool {
	return draining.Load()
}

// BotBusy reports the Discord bot's in-flight work (bot.Busy). Set in main.go;
// hooks can't import bot without a cycle.
var BotBusy func() int

// RegisterDrain wires the supervised-restart protocol, or on Railway the
// SIGTERM drain. A no-op otherwise, so `go run . serve` is unaffected.
func RegisterDrain(app *pocketbase.PocketBase) {
	supervised := os.Getenv(supervisedEnv) == "1"
	budget := railwayDrainBudget()
	if !supervised && budget == 0 {
		return
	}

	// Refuse new uploads for the few seconds between "idle" and exit. Covers
	// the bot too: its ingest creates records through the same hook.
	app.OnRecordCreate("contents").BindFunc(func(e *core.RecordEvent) error {
		if draining.Load() {
			return apis.NewApiError(503, "Server is updating, please retry in a minute.", nil)
		}
		return e.Next()
	})

	if supervised {
		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			go listenSupervisor(app)
			return e.Next()
		})
		return
	}

	// Railway redeploys by sending SIGTERM and kills the process once the
	// service's draining time is up. PocketBase turns SIGTERM into OnTerminate;
	// this handler runs ahead of its graceful shutdown, so the HTTP server keeps
	// answering while the in-flight work finishes. Anything still running at
	// the deadline is re-queued at a later boot (encode recovery) or retried by
	// its owner.
	var once sync.Once
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id:       "goyangiRailwayDrain",
		Priority: -100,
		Func: func(e *core.TerminateEvent) error {
			once.Do(func() { drainForRailway(budget) })
			return e.Next()
		},
	})
}

// railwayDrainMargin is kept back from Railway's draining time for PocketBase's
// own shutdown: stopping the HTTP server, flushing R2 deletes, closing the
// databases.
const railwayDrainMargin = 15 * time.Second

// railwayDrainBudget is how long a SIGTERM may wait for in-flight work:
// RAILWAY_DEPLOYMENT_DRAINING_SECONDS (a service variable on Railway), minus
// the margin. Zero when unset or too short to be worth waiting.
func railwayDrainBudget() time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RAILWAY_DEPLOYMENT_DRAINING_SECONDS")))
	if err != nil {
		return 0
	}
	return max(time.Duration(secs)*time.Second-railwayDrainMargin, 0)
}

func drainForRailway(budget time.Duration) {
	draining.Store(true)
	log.Printf("🔄 deploy: SIGTERM — refusing new uploads, waiting up to %s for work in flight", budget)
	time.Sleep(drainGrace)
	if waitIdle(0, time.Now().Add(budget-drainGrace)) {
		log.Printf("🔄 deploy: nothing in flight — shutting down")
		return
	}
	busy := busyReport()
	log.Printf("⚠️  deploy: draining time up — shutting down with %s still in flight", busy)
	LogServerWarning("Deploy cut off work in flight", map[string]any{"inFlight": busy})
}

func listenSupervisor(app *pocketbase.PocketBase) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "drain":
			drainAndExit(app)
		case strings.HasPrefix(line, "warn "):
			// The updater's own problems (a failed build) that leave this
			// process running, so they still reach system_logs.
			LogServerWarning(strings.TrimPrefix(line, "warn "), nil)
		}
	}
	// EOF: the updater is gone (crashed or killed). Exit now so its restart
	// doesn't find an orphan still holding the port. In-flight encodes are
	// lost here, but this is the abnormal path.
	log.Printf("⚠️  deploy: supervisor stdin closed — shutting down")
	terminate(app)
}

// drainAndExit waits for idle, stops taking uploads, waits out the stragglers
// and exits. It never returns.
func drainAndExit(app *pocketbase.PocketBase) {
	log.Printf("🔄 deploy: restart requested — waiting for uploads and bot work to finish")
	waitIdle(drainQuietPeriod, time.Time{})

	draining.Store(true)
	log.Printf("🔄 deploy: idle — refusing new uploads while the last requests finish")
	time.Sleep(drainGrace)
	waitIdle(0, time.Time{})

	log.Printf("🔄 deploy: nothing in flight — exiting for restart")
	terminate(app)
}

// waitIdle blocks until busyReport has been empty for quiet (or just once, for
// quiet == 0) and reports true. With a zero deadline it never gives up: a long
// queue delays the deploy, never the uploads in it. Past a deadline it reports
// false.
func waitIdle(quiet time.Duration, deadline time.Time) bool {
	var idleSince time.Time
	lastReport := time.Now()
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false
		}
		busy := busyReport()
		switch {
		case busy != "":
			idleSince = time.Time{}
			if time.Since(lastReport) >= drainReportGap {
				log.Printf("⏳ deploy: still waiting — %s", busy)
				lastReport = time.Now()
			}
		case idleSince.IsZero():
			idleSince = time.Now()
		}
		if busy == "" && time.Since(idleSince) >= quiet {
			return true
		}
		time.Sleep(drainPoll)
	}
}

// busyReport describes the work a restart would lose, or "" when there is none.
func busyReport() string {
	var parts []string
	if n := queueSnapshot().Total; n > 0 {
		parts = append(parts, fmt.Sprintf("%d encode job(s)", n))
	}
	if n := len(convertGate); n > 0 {
		parts = append(parts, fmt.Sprintf("%d convert request(s)", n))
	}
	if n := len(r2DeleteQueue); n > 0 {
		parts = append(parts, fmt.Sprintf("%d queued R2 delete(s)", n))
	}
	if BotBusy != nil {
		if n := BotBusy(); n > 0 {
			parts = append(parts, fmt.Sprintf("%d bot task(s)", n))
		}
	}
	return strings.Join(parts, ", ")
}

// terminate runs PocketBase's own shutdown, as Ctrl+C would: the
// OnTerminate chain stops the HTTP server gracefully and drains the R2 delete
// queue. Stopping the server makes app.Start() return in main, which runs the
// chain again (harmless — every handler tolerates it) with the finalizer that
// closes the databases, and the process exits from there. The exit below only
// fires if that somehow doesn't happen.
func terminate(app *pocketbase.PocketBase) {
	event := new(core.TerminateEvent)
	event.App = app
	if err := app.OnTerminate().Trigger(event, func(*core.TerminateEvent) error { return nil }); err != nil {
		log.Printf("⚠️  deploy: terminate hooks: %v", err)
	}
	time.Sleep(15 * time.Second)
	os.Exit(0)
}

// RegisterDeployInfo records which commit this process runs, in the log and in
// system_logs — the maintainer reads deploys and rollbacks from the admin UI,
// having no access to the machine. commit is stamped by the updater's build
// (-ldflags "-X main.commit=…"); GOYANGI_DEPLOY_NOTE is the updater's account
// of how this boot came about, and GOYANGI_DEPLOY_LEVEL=warning flags a
// rollback or crash restart.
func RegisterDeployInfo(app *pocketbase.PocketBase, commit string) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		note := os.Getenv("GOYANGI_DEPLOY_NOTE")
		if note == "" {
			note = "server started"
		}
		ctx := map[string]any{"commit": commit, "go": runtime.Version()}
		// The toolchain the updater built with is whatever that host has
		// installed, and its JSON engine is how a collection save from the
		// admin UI crashed production. GOEXPERIMENT and friends live in the
		// build settings.
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "GOEXPERIMENT" || s.Key == "GOOS" || s.Key == "GOARCH" || s.Key == "-tags" {
					ctx[s.Key] = s.Value
				}
			}
		}
		if os.Getenv("GOYANGI_DEPLOY_LEVEL") == "warning" {
			LogServerWarning(note, ctx)
		} else {
			LogServerInfo(note, ctx)
		}
		return e.Next()
	})
}
