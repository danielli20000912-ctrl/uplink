// Command uplink runs the uplink exporter, publisher and conformance check.
//
//	uplink export  -config export.yaml [-once]
//	uplink publish -config publish.yaml
//	uplink check   -base https://api.example.com -key-env UPLINK_KEY [-since 24h] [-save dir]
//	uplink sync    -config sync.yaml [-once] [-adopt m,..|all] [-approve m,..|all] [-release m,..|all] [-ack]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"uplink/internal/check"
	"uplink/internal/downstream"
	"uplink/internal/export"
	"uplink/internal/publish"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "export":
		runExport(args)
	case "publish":
		runPublish(args)
	case "check":
		runCheck(args)
	case "sync":
		runSync(args)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: uplink export|publish|check|sync [flags]")
	os.Exit(2)
}

func loadYAML(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		log.Fatalf("%s: %v", path, err)
	}
}

func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	cfgPath := fs.String("config", "export.yaml", "config file")
	once := fs.Bool("once", false, "run one cycle and exit")
	fs.Parse(args)
	var cfg export.Config
	loadYAML(*cfgPath, &cfg)
	e, err := export.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		if err := e.Once(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := e.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func runPublish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	cfgPath := fs.String("config", "publish.yaml", "config file")
	fs.Parse(args)
	var cfg publish.Config
	loadYAML(*cfgPath, &cfg)
	s, err := publish.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Fatal(s.ListenAndServe())
}

func runCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	base := fs.String("base", "", "publisher base URL (where /.well-known/uplink.json lives)")
	keyEnv := fs.String("key-env", "UPLINK_KEY", "environment variable holding the API key")
	since := fs.Duration("since", 24*time.Hour, "check usage from this long ago")
	save := fs.String("save", "", "directory to save raw responses")
	fs.Parse(args)
	key := os.Getenv(*keyEnv)
	if *base == "" || key == "" {
		log.Fatalf("check: -base and a key in $%s are required", *keyEnv)
	}
	now := time.Now()
	rep, err := check.Run(check.Options{BaseURL: *base, Key: key, Since: now.Add(-*since), Until: now.Add(time.Minute), SaveDir: *save})
	if err != nil {
		log.Fatal(err)
	}
	rep.Print(os.Stdout)
	if !rep.OK() {
		os.Exit(1)
	}
}

func splitArg(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	cfgPath := fs.String("config", "sync.yaml", "config file")
	once := fs.Bool("once", false, "run one cycle and exit")
	adopt := fs.String("adopt", "", "take over pricing of these models (comma-separated, or all)")
	approve := fs.String("approve", "", "apply held price changes for these models (comma-separated, or all)")
	release := fs.String("release", "", "stop managing these models and restore their previous pricing (comma-separated, or all)")
	ack := fs.Bool("ack", false, "clear holds after checking the alerts that raised them")
	fs.Parse(args)
	var cfg downstream.Config
	loadYAML(*cfgPath, &cfg)
	s, err := downstream.New(cfg, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	act := downstream.Actions{Adopt: splitArg(*adopt), Approve: splitArg(*approve), Release: splitArg(*release), Ack: *ack}
	if *once || len(act.Adopt)+len(act.Approve)+len(act.Release) > 0 || act.Ack {
		if err := s.Once(ctx, act); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
