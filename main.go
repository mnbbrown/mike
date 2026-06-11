package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func usage() {
	fmt.Fprintf(os.Stderr, `mike — a friendly log viewer

Usage:
  mike <command> [args...]    run a command and watch its logs
  <command> | mike            pipe logs in
  mike --demo                 watch some pretend logs

Keys inside:
  /      search — plain words, field=value, or field>n / field>=n / field<n
         e.g. "service=api latency_ms>200 timeout"
  k      pick which JSON fields to show (tick boxes with space)
  ↑↓     pick a line; enter expands it, y copies it
  e / w  show only errors / warnings+errors
  j      expand all JSON to pretty multi-line
  f      pause/resume following
  r      restart the command
  q      quit

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	maxLines := flag.Int("max-lines", 50000, "how many lines to keep in memory")
	demo := flag.Bool("demo", false, "generate pretend logs to play with")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()

	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}

	var (
		ch      <-chan Entry
		exitCh  <-chan procExitMsg
		kill    func()
		restart func() (<-chan Entry, <-chan procExitMsg, func())
		cmdline string
	)

	switch {
	case *demo:
		cmdline = "demo logs"
		c := make(chan Entry, 1024)
		go runDemo(c)
		ch, exitCh = c, make(chan procExitMsg)

	case len(args) > 0:
		cmdline = strings.Join(args, " ")
		ch, exitCh, kill = startCmd(args)
		restart = func() (<-chan Entry, <-chan procExitMsg, func()) { return startCmd(args) }

	default:
		// piped input: logs come from stdin, keyboard comes from the tty
		if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			usage()
			os.Exit(2)
		}
		tty, err := os.Open("/dev/tty")
		if err != nil {
			fatal(fmt.Errorf("cannot open terminal for keyboard input: %w", err))
		}
		defer tty.Close()
		opts = append(opts, tea.WithInput(tty))
		cmdline = "stdin"
		c := make(chan Entry, 1024)
		e := make(chan procExitMsg, 1)
		go func() {
			readLines(os.Stdin, SrcStdout, c)
			close(c)
			e <- procExitMsg{}
		}()
		ch, exitCh = c, e
	}

	m := newModel(cmdline, *maxLines, ch, exitCh, kill, restart)
	if _, err := tea.NewProgram(m, opts...).Run(); err != nil {
		fatal(err)
	}
}

// startCmd launches the command and streams its output. Failures surface in
// the TUI via the exit channel rather than tearing the whole thing down.
func startCmd(args []string) (<-chan Entry, <-chan procExitMsg, func()) {
	ch := make(chan Entry, 1024)
	exitCh := make(chan procExitMsg, 1)
	fail := func(err error) (<-chan Entry, <-chan procExitMsg, func()) {
		close(ch)
		exitCh <- procExitMsg{err: err.Error()}
		return ch, exitCh, nil
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readLines(stdout, SrcStdout, ch) }()
	go func() { defer wg.Done(); readLines(stderr, SrcStderr, ch) }()
	go func() {
		wg.Wait()
		msg := procExitMsg{}
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				msg.code = ee.ExitCode()
			} else {
				msg.err = err.Error()
			}
		}
		close(ch)
		exitCh <- msg
	}()

	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	return ch, exitCh, kill
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mike:", err)
	os.Exit(1)
}

func readLines(r io.Reader, src Source, ch chan<- Entry) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		ch <- NewEntry(line, src)
	}
}

// runDemo emits pretend JSON (and the odd plain) log lines forever.
func runDemo(ch chan<- Entry) {
	services := []string{"api", "worker", "billing", "mailer"}
	msgs := []string{
		"request handled", "cache miss", "user logged in", "job finished",
		"retrying upstream", "connection reset", "payment captured",
	}
	levels := []string{"debug", "info", "info", "info", "warn", "error"}
	i := 0
	for {
		i++
		lvl := levels[rand.Intn(len(levels))]
		line := fmt.Sprintf(
			`{"time":%q,"level":%q,"msg":%q,"service":%q,"request_id":"req-%04d","latency_ms":%d,"user":{"id":%d,"plan":"pro"}}`,
			time.Now().Format(time.RFC3339), lvl, msgs[rand.Intn(len(msgs))],
			services[rand.Intn(len(services))], i, rand.Intn(900)+10, rand.Intn(5000),
		)
		src := SrcStdout
		if lvl == "error" {
			src = SrcStderr
		}
		ch <- NewEntry(line, src)
		if i%17 == 0 {
			ch <- NewEntry("plain text line: something non-JSON happened", SrcStdout)
		}
		time.Sleep(time.Duration(rand.Intn(400)+80) * time.Millisecond)
	}
}
