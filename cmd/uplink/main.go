// Command uplink runs the uplink exporter, publisher and conformance check.
//
//	uplink export  -config export.yaml [-once]
//	uplink publish -config publish.yaml
//	uplink check   -base https://api.example.com -key-env UPLINK_KEY [-since 24h] [-save dir]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"uplink/internal/check"
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: uplink export|publish|check [flags]")
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
