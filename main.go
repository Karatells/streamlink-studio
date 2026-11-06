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
	"syscall"
	"time"
)

// version is overridden at build time: -ldflags "-X main.version=v1.0.0"
var version = "dev"

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
		fmt.Println("streamlink-studio", version)
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
	static := http.FileServer(http.FS(sub))
	api := &API{m: m}
	handler := basicAuth(os.Getenv("STUDIO_USER"), os.Getenv("STUDIO_PASS"), api.Routes(static))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Streamlink Studio %s listening on %s (data: %s, recordings: %s)", version, cfg.Addr, cfg.DataDir, cfg.RecDir)
		if os.Getenv("STUDIO_USER") == "" && os.Getenv("STUDIO_PASS") == "" {
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
}
