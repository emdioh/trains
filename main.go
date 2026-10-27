// Command trains tracks the on-time status of Italian trains through
// ViaggiaTreno, notifies when they are late and serves a status page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // the binary must not depend on the host's zoneinfo

	"trains/internal/config"
	"trains/internal/notify"
	"trains/internal/store"
	"trains/internal/tracker"
	vt "trains/internal/viaggiatreno"
	"trains/internal/web"
)

var version = "dev"

const defaultConfig = "/etc/trains/config.yaml"

// stopTimeout bounds a graceful shutdown; the OpenRC script waits a little
// longer before killing the process.
const stopTimeout = 8 * time.Second

const usage = `Usage: trains <command> [flags]

Commands:
  run           track the configured trains and serve the status page
  check NUMBER  print the live status of a train
  test-notify   send a test message to every configured notifier
  version       print the version

Flags:
  -config PATH  configuration file (default /etc/trains/config.yaml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	flags := flag.NewFlagSet(cmd, flag.ExitOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configPath := flags.String("config", defaultConfig, "configuration file")
	flags.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch cmd {
	case "run":
		err = run(*configPath, log)
	case "check":
		err = check(*configPath, flags.Args())
	case "test-notify":
		err = testNotify(*configPath)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trains:", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if len(cfg.Notifiers) == 0 {
		log.Warn("no notifiers configured: notifications are off")
	}
	switch imported, err := st.ImportTrains(context.Background(), cfg.Trains); {
	case err != nil:
		return fmt.Errorf("importing trains: %w", err)
	case imported:
		log.Info("imported the trains from the config file; edit them on the status page from now on",
			"trains", len(cfg.Trains))
	case len(cfg.Trains) > 0:
		log.Warn("the trains listed in the config file are ignored: the list is edited on the status page")
	}
	source := vt.New(cfg.BaseURL)
	t := tracker.New(configPath, cfg, source, st, log)
	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: (&web.Server{Config: t.Config, Store: st, Source: source, Log: log, Now: time.Now,
			Changed: t.Wake, Refresh: t.Refresh, Version: version}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("started", "version", version, "listen", cfg.Listen)

	done := make(chan struct{})
	go func() { t.Run(ctx); close(done) }()

	select {
	case err = <-errc:
		stop()
	case <-ctx.Done():
		log.Info("stopping")
	}
	// Whatever happens below, don't outlive the init system's patience.
	time.AfterFunc(stopTimeout, func() {
		log.Error("shutdown is taking too long; exiting anyway", "after", stopTimeout)
		os.Exit(1)
	})
	<-done
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func check(configPath string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: trains check NUMBER")
	}
	number, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("%q is not a train number", args[0])
	}
	baseURL := ""
	if cfg, err := config.Load(configPath); err == nil {
		baseURL = cfg.BaseURL
	}
	client := vt.New(baseURL)
	ctx := context.Background()
	runs, err := client.FindRuns(ctx, number)
	if err != nil {
		return err
	}
	for _, run := range runs {
		s, _, err := client.Status(ctx, run)
		if err != nil {
			fmt.Printf("%d from %s (%s): %v\n", run.Number, run.OriginName, run.Origin, err)
			continue
		}
		station, _ := s.LastSeen()
		fmt.Printf("%s  %s → %s  (origin %s, %s)\n", s.Name, tracker.Title(string(s.Origin)),
			tracker.Title(string(s.Destination)), run.Origin, run.Date.Format("2006-01-02"))
		fmt.Printf("  scheduled %s → %s\n", s.ScheduledDeparture().Format("15:04"), s.ScheduledArrival().Format("15:04"))
		fmt.Printf("  state %s, delay %d min", s.State(), s.Delay)
		if s.PartiallyCancelled() {
			fmt.Print(", partly cancelled")
		}
		if station != "" {
			fmt.Printf(", last seen %s at %s", tracker.Title(station), s.LastSeenMs.Time().Format("15:04"))
		}
		fmt.Println()
	}
	if len(runs) > 1 {
		fmt.Printf("\n%d trains have number %d. In the config, choose one with its origin, e.g.:\n", len(runs), number)
		fmt.Printf("  - number: %d\n    origin: %s   # %s\n", number, runs[0].Origin, tracker.Title(runs[0].OriginName))
	}
	return nil
}

func testNotify(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ns := notify.New(cfg.Notifiers)
	if len(ns) == 0 {
		return errors.Join(append([]error{errors.New("no notifiers configured")}, warningErrors(cfg)...)...)
	}
	return notify.All(context.Background(), ns, notify.Message{
		Title: "🚆 trains: test notification",
		Body:  "If you can read this, notifications work.",
	})
}

func warningErrors(cfg *config.Config) []error {
	var errs []error
	for _, w := range cfg.Warnings {
		errs = append(errs, errors.New(w))
	}
	return errs
}
