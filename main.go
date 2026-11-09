package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
)

// version is overridden at build time: -ldflags "-X main.version=v1.0.0"
var (
	version = "dev"
	commit  = "" // -ldflags "-X main.commit=<sha>"
)

// currentCommit returns the commit this binary was built from.
func currentCommit() string {
	if commit != "" {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

// requestShutdown asks the process to stop gracefully (same path as SIGTERM).
func requestShutdown() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }

//go:embed all:web
var webFS embed.FS

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var cfg Config
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&cfg.Addr, "addr", env("STUDIO_ADDR", ":8080"), "listen address (env STUDIO_ADDR)")
	flag.StringVar(&cfg.DataDir, "data", env("STUDIO_DATA", "./data"), "data directory (env STUDIO_DATA)")
	flag.StringVar(&cfg.RecDir, "rec", os.Getenv("STUDIO_RECORDINGS"), "recordings directory (env STUDIO_RECORDINGS, default <data>/recordings)")
	flag.Parse()
	if *showVersion {
		c := currentCommit()
		if len(c) > 7 {
			c = c[:7]
		}
		if c != "" {
			fmt.Printf("streamlink-studio %s (%s)\n", version, c)
		} else {
			fmt.Println("streamlink-studio", version)
		}
		return
	}
	if cfg.RecDir == "" {
		cfg.RecDir = filepath.Join(cfg.DataDir, "recordings")
	}
	for _, d := range []string{cfg.DataDir, cfg.RecDir} {
		if err := ensureDir(d); err != nil {
			log.Fatalf("cannot create %s: %v", d, err)
		}
	}
	st, err := OpenStore(filepath.Join(cfg.DataDir, "studio.json"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	// leftovers of interrupted runs may contain secrets
	_ = os.RemoveAll(filepath.Join(cfg.DataDir, "tmp"))

	m := NewManager(cfg, st)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	static := pwaHeaders(http.FileServer(http.FS(sub)))
	auth := NewAuth(os.Getenv("STUDIO_USER"), os.Getenv("STUDIO_PASS"), cfg.DataDir)
	upd := NewUpdater(m)
	api := &API{m: m, auth: auth, upd: upd}
	handler := auth.Middleware(api.Routes(static))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)
	go upd.Run(ctx)
	api.tg = NewTelegram(api)
	m.extraSink = api.tg.Enqueue
	go api.tg.Run(ctx)
	go m.RunAutoCompress(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Streamlink Studio %s listening on %s (data: %s, recordings: %s)", version, cfg.Addr, cfg.DataDir, cfg.RecDir)
		if !auth.Enabled() {
			log.Println("WARNING: no STUDIO_USER/STUDIO_PASS set - the panel is open to anyone who can reach it")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down: stopping recordings...")
	m.Shutdown(30 * time.Second)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if restartRequested.Load() {
		log.Println("restarting after update")
		os.Exit(restartExitCode)
	}
}
