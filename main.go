package main

import (
	"context"
	"fmt"
	"log"
	"os"

	flag "github.com/spf13/pflag"
)

var (
	BuiltTime = "__now__"
)

var (
	configFilename = flag.StringP("config", "c", "", "configuration filename (default config is generated if config file not exists)")
	showVersion    = flag.BoolP("version", "v", false, "show built time")
	debugMode      = flag.BoolP("debug", "d", false, "debug mode")
)

var (
	datalabPool *DatalabPool
	store       DataStore
)

func main() {
	var (
		err        error
		config     *Config
		apiService *APIService
	)

	log.SetFlags(log.LstdFlags)

	parseCommandLine()

	if config, err = setupConfig(*configFilename); err != nil {
		log.Fatalf("read configuration failure: %v", err)
	}

	ctx, ctxDone := context.WithCancel(context.Background())
	defer ctxDone()

	datalabPool = NewDatalabPool(&config.Datalab)
	if err = datalabPool.Initialize(ctx); err != nil {
		log.Fatalf("datalab pool initialize failure: %v", err)
	}

	apiService = NewAPIService(&config.Service)
	if err = apiService.Run(ctx, *debugMode); err != nil {
		log.Fatalf("api service initialize failure: %v", err)
	}
}

func parseCommandLine() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s (built %s):\n", os.Args[0], BuiltTime)
		flag.PrintDefaults()
	}
	flag.Parse()
	if !flag.Parsed() {
		flag.Usage()
		os.Exit(0)
	}
	if *showVersion {
		fmt.Fprintf(os.Stdout, "%s\n", BuiltTime)
		os.Exit(0)
	}
}
