// Command updater runs the goyangi server and keeps it on the latest commit of
// a branch, so pushing to main is the whole deploy.
//
// It is meant to be the long-running service on the host (NSSM on Windows,
// systemd elsewhere), started from the repo root, which must be a deploy-only
// clone: every deploy runs `git reset --hard`. pb_data, .env and logs are
// gitignored and untouched.
//
//	go build -o goyangi-updater.exe ./scripts/updater
//	goyangi-updater.exe -http 127.0.0.1:8090
//
// Loop:
//  1. Start the server (goyangi[.exe] serve) as a child with GOYANGI_SUPERVISED=1
//     and restart it with backoff if it dies on its own.
//  2. Every -interval, fetch the branch. On a new commit: reset to it and build
//     goyangi.next. A failed build marks the commit bad; the server keeps running.
//  3. Write "drain" to the child's stdin and wait for it to exit. The server
//     only does so once no upload, convert or bot work is in flight
//     (hooks/deploy.go), however long that takes.
//  4. Swap the binaries (the old one is kept as goyangi.prev), start the new one
//     and require /api/health to answer and the process to stay up for
//     -health-hold. Otherwise swap back, mark the commit bad and restart the old
//     binary with a ROLLBACK note.
//
// Every boot's commit and the updater's note land in the server's system_logs
// collection, which is how deploys are followed without access to the host.
//
// The updater does not replace itself. When scripts/updater changes, the deploy
// note says so and it has to be rebuilt by hand.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

var (
	dir          = flag.String("dir", ".", "repo root (deploy-only clone; reset --hard on every deploy)")
	remote       = flag.String("remote", "origin", "git remote to watch")
	branch       = flag.String("branch", "main", "branch to deploy")
	interval     = flag.Duration("interval", 2*time.Minute, "how often to check for new commits")
	httpAddr     = flag.String("http", "127.0.0.1:8090", "address the server listens on")
	healthWait   = flag.Duration("health-timeout", 90*time.Second, "how long a new build has to answer /api/health")
	healthHold   = flag.Duration("health-hold", 2*time.Minute, "how long a new build must stay up to count as deployed")
	childStopMax = 30 * time.Second
)

// Binary names, relative to -dir.
var (
	exe     = exeName("goyangi")
	exeNext = exeName("goyangi.next")
	exePrev = exeName("goyangi.prev")
)

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// state survives updater restarts.
type state struct {
	Deployed string   `json:"deployed"`
	Bad      []string `json:"bad"`
}

func statePath() string { return filepath.Join(*dir, ".deploy", "state.json") }

func loadState() state {
	var s state
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (s state) save() {
	_ = os.MkdirAll(filepath.Dir(statePath()), 0o755)
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(statePath(), b, 0o644); err != nil {
		log.Printf("⚠️  could not save state: %v", err)
	}
}

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("🛠️  updater: ")
	flag.Parse()

	abs, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	*dir = abs

	// Ctrl+C from NSSM on service stop (Windows), SIGINT/SIGTERM elsewhere.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := loadState()

	// First run (or lost state): build what is checked out, so the running
	// binary is known to match a commit and carries its stamp.
	if st.Deployed == "" || !fileExists(exe) {
		head, err := git("rev-parse", "HEAD")
		if err != nil {
			log.Fatalf("not a git checkout? %v", err)
		}
		log.Printf("building %s from HEAD %s", exe, short(head))
		if err := build(head); err != nil {
			log.Fatalf("initial build failed: %v", err)
		}
		if err := replace(exeNext, exe); err != nil {
			log.Fatal(err)
		}
		st.Deployed = head
		st.save()
	}

	log.Printf("running %s, watching %s/%s every %s", short(st.Deployed), *remote, *branch, *interval)
	c := mustStart("", "")

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	backoff := 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Printf("stopping")
			c.stop()
			return

		case <-c.done:
			log.Printf("⚠️  server exited unexpectedly (%v) — restarting in %s", c.err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			c = mustStart(fmt.Sprintf("restarted after unexpected exit (%v)", c.err), "warning")

		case <-ticker.C:
			// A server that has been up for a full interval is healthy again.
			backoff = 5 * time.Second
			c = checkForUpdate(ctx, &st, c)
		}
	}
}

// checkForUpdate deploys the branch tip if it is new, returning the child that
// is running afterwards.
func checkForUpdate(ctx context.Context, st *state, c *child) *child {
	if _, err := git("fetch", "--quiet", *remote, *branch); err != nil {
		log.Printf("⚠️  fetch failed: %v", err)
		return c
	}
	target, err := git("rev-parse", *remote+"/"+*branch)
	if err != nil {
		log.Printf("⚠️  rev-parse failed: %v", err)
		return c
	}
	if target == st.Deployed || slices.Contains(st.Bad, target) {
		return c
	}

	prev := st.Deployed
	log.Printf("new commit %s (running %s) — building", short(target), short(prev))
	if _, err := git("reset", "--hard", "--quiet", target); err != nil {
		log.Printf("⚠️  reset to %s failed: %v", short(target), err)
		return c
	}
	if err := build(target); err != nil {
		log.Printf("❌ build of %s failed, staying on %s: %v", short(target), short(prev), err)
		c.warn(fmt.Sprintf("build of %s failed, still running %s: %s", short(target), short(prev), oneLine(err)))
		markBad(st, target)
		_, _ = git("reset", "--hard", "--quiet", prev)
		return c
	}

	note := fmt.Sprintf("deployed %s (was %s)", short(target), short(prev))
	if updaterChanged(prev, target) {
		note += " — scripts/updater changed: rebuild goyangi-updater by hand"
		log.Printf("⚠️  scripts/updater changed in %s; this updater keeps running the old code until rebuilt", short(target))
	}

	log.Printf("asking the server to finish in-flight work and exit…")
	if !c.drain(ctx) {
		// Service is stopping mid-drain. The tree is at target and the new
		// binary is built; the next start redeploys from state (still prev).
		return c
	}

	if err := swapIn(); err != nil {
		log.Printf("❌ swap failed, restarting %s: %v", short(prev), err)
		markBad(st, target)
		_, _ = git("reset", "--hard", "--quiet", prev)
		return mustStart("", "")
	}

	log.Printf("starting %s", short(target))
	next, err := startChild(note, "")
	if err == nil {
		err = waitHealthy(ctx, next)
		if err == nil {
			st.Deployed = target
			st.save()
			log.Printf("✅ deployed %s", short(target))
			return next
		}
		next.stop()
	}

	log.Printf("❌ %s failed its health check (%v) — rolling back to %s", short(target), err, short(prev))
	markBad(st, target)
	if rerr := rollBack(); rerr != nil {
		log.Printf("❌ rollback swap failed: %v", rerr)
	}
	_, _ = git("reset", "--hard", "--quiet", prev)
	return mustStart(fmt.Sprintf("ROLLBACK: %s failed its health check (%v); running %s again",
		short(target), err, short(prev)), "warning")
}

func markBad(st *state, commit string) {
	st.Bad = append(st.Bad, commit)
	if len(st.Bad) > 50 {
		st.Bad = st.Bad[len(st.Bad)-50:]
	}
	st.save()
}

// waitHealthy requires /api/health to answer 200 within -health-timeout and
// the process to still be running -health-hold after start.
func waitHealthy(ctx context.Context, c *child) error {
	url := "http://" + *httpAddr + "/api/health"
	client := &http.Client{Timeout: 5 * time.Second}
	started := time.Now()

	for {
		select {
		case <-c.done:
			return fmt.Errorf("exited during startup: %v", c.err)
		case <-ctx.Done():
			return errors.New("updater stopping")
		case <-time.After(2 * time.Second):
		}
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Since(started) > *healthWait {
			return fmt.Errorf("no healthy /api/health within %s", *healthWait)
		}
	}

	select {
	case <-c.done:
		return fmt.Errorf("exited %s after start: %v", time.Since(started).Round(time.Second), c.err)
	case <-ctx.Done():
		return errors.New("updater stopping")
	case <-time.After(*healthHold - time.Since(started)):
		return nil
	}
}

// child is a running server process.
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{} // closed on exit; err is valid after
	err   error
}

func startChild(note, level string) (*child, error) {
	cmd := exec.Command(filepath.Join(*dir, exe), "serve", "--http="+*httpAddr)
	cmd.Dir = *dir
	cmd.Env = append(os.Environ(),
		"GOYANGI_SUPERVISED=1",
		"GOYANGI_DEPLOY_NOTE="+note,
		"GOYANGI_DEPLOY_LEVEL="+level,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// mustStart starts the current binary; failing to even launch it is fatal,
// and the service manager's restart is the only recovery left.
func mustStart(note, level string) *child {
	c, err := startChild(note, level)
	if err != nil {
		log.Fatalf("cannot start %s: %v", exe, err)
	}
	return c
}

// warn has the server record msg in system_logs (hooks/deploy.go).
func (c *child) warn(msg string) {
	if _, err := io.WriteString(c.stdin, "warn "+msg+"\n"); err != nil {
		log.Printf("⚠️  could not pass a warning to the server: %v", err)
	}
}

// drain asks the server to exit once idle and waits for it, without limit.
// Returns false if the updater is told to stop meanwhile (the server is then
// stopped right away).
func (c *child) drain(ctx context.Context) bool {
	if _, err := io.WriteString(c.stdin, "drain\n"); err != nil {
		log.Printf("⚠️  could not signal the server (%v); waiting for it to exit anyway", err)
	}
	select {
	case <-c.done:
		log.Printf("server exited (%v)", c.err)
		return true
	case <-ctx.Done():
		c.stop()
		return false
	}
}

// stop shuts the server down now: closing stdin makes it run PocketBase's
// terminate path (hooks/deploy.go). Killed if it doesn't exit in time.
func (c *child) stop() {
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(childStopMax):
		log.Printf("⚠️  server didn't exit within %s — killing it", childStopMax)
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// build compiles the checked-out tree into goyangi.next, stamped with commit.
func build(commit string) error {
	cmd := exec.Command("go", "build", "-ldflags", "-X main.commit="+commit, "-o", exeNext, ".")
	cmd.Dir = *dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(string(out), 2000))
	}
	return nil
}

// swapIn makes goyangi.next the live binary, keeping the old one as
// goyangi.prev. The server must not be running (Windows locks a running .exe).
func swapIn() error {
	_ = os.Remove(path(exePrev))
	if err := os.Rename(path(exe), path(exePrev)); err != nil {
		return err
	}
	return os.Rename(path(exeNext), path(exe))
}

// rollBack restores goyangi.prev as the live binary.
func rollBack() error {
	_ = os.Remove(path(exe))
	return os.Rename(path(exePrev), path(exe))
}

func replace(from, to string) error {
	_ = os.Remove(path(to))
	return os.Rename(path(from), path(to))
}

// updaterChanged reports whether this program's source differs between two
// commits.
func updaterChanged(from, to string) bool {
	cmd := exec.Command("git", "diff", "--quiet", from, to, "--", "scripts/updater")
	cmd.Dir = *dir
	var exitErr *exec.ExitError
	return errors.As(cmd.Run(), &exitErr) && exitErr.ExitCode() == 1
}

// git runs a git command in -dir and returns its trimmed stdout. Prompts are
// disabled so a credential problem fails instead of hanging the service.
func git(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = *dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func path(name string) string { return filepath.Join(*dir, name) }

func fileExists(name string) bool {
	_, err := os.Stat(path(name))
	return err == nil
}

// oneLine flattens a multi-line error (build output) for a single log line.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Keep whole lines where possible.
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "…\n" + s
}
