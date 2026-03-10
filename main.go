package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
)

var (
	configFile = flag.String(
		"config.file", "ipmi.yml",
		"Path to configuration file.",
	)
	executablesPath = flag.String(
		"path", "",
		"Path to FreeIPMI executables (default: rely on $PATH).",
	)
	listenAddress = flag.String(
		"web.listen-address", ":9290",
		"Address to listen on for web interface and telemetry.",
	)
	logLevel = flag.String(
		"log.level", "info",
		"Only log messages with the given severity or above. One of: [debug, info, warn, error]",
	)
	logFormat = flag.String(
		"log.format", "logfmt",
		"Output format of log messages. One of: [logfmt, json]",
	)

	sc = &SafeConfig{
		C: &Config{},
	}
	reloadCh chan chan error
)

func remoteIPMIHandler(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "'target' parameter must be specified", 400)
		return
	}
	logger.Debug(fmt.Sprintf("Scraping target '%s'", target))

	registry := prometheus.NewRegistry()
	remoteCollector := collector{target: target, config: sc, logger: logger}
	registry.MustRegister(remoteCollector)
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func updateConfiguration(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	switch r.Method {
	case "POST":
		rc := make(chan error)
		reloadCh <- rc
		if err := <-rc; err != nil {
			http.Error(w, fmt.Sprintf("failed to reload config: %s", err), http.StatusInternalServerError)
		}
	default:
		logger.Error(fmt.Sprintf("Only POST requests allowed for %s", r.URL))
		w.Header().Set("Allow", "POST")
		http.Error(w, "Only POST requests allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	flag.Parse()

	// Set up promslog logger.
	promslogConfig := &promslog.Config{}
	promslogLevel := promslog.NewLevel()
	if err := promslogLevel.Set(*logLevel); err != nil {
		fmt.Fprintf(os.Stderr, "Error setting log level: %s\n", err)
		os.Exit(1)
	}
	promslogConfig.Level = promslogLevel
	promslogFormat := promslog.NewFormat()
	if err := promslogFormat.Set(*logFormat); err != nil {
		fmt.Fprintf(os.Stderr, "Error setting log format: %s\n", err)
		os.Exit(1)
	}
	promslogConfig.Format = promslogFormat
	logger := promslog.New(promslogConfig)
	slog.SetDefault(logger)

	logger.Info("Starting ipmi_exporter")

	// Bail early if the config is bad.
	sc.logger = logger
	if err := sc.ReloadConfig(*configFile); err != nil {
		logger.Error(fmt.Sprintf("Error parsing config file: %s", err))
		os.Exit(1)
	}

	hup := make(chan os.Signal, 1)
	reloadCh = make(chan chan error)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-hup:
				if err := sc.ReloadConfig(*configFile); err != nil {
					logger.Error(fmt.Sprintf("Error reloading config: %s", err))
				}
			case rc := <-reloadCh:
				if err := sc.ReloadConfig(*configFile); err != nil {
					logger.Error(fmt.Sprintf("Error reloading config: %s", err))
					rc <- err
				} else {
					rc <- nil
				}
			}
		}
	}()

	localCollector := collector{target: targetLocal, config: sc, logger: logger}
	prometheus.MustRegister(&localCollector)

	http.Handle("/metrics", promhttp.Handler()) // Regular metrics endpoint for local IPMI metrics.
	http.HandleFunc("/ipmi", func(w http.ResponseWriter, r *http.Request) {
		remoteIPMIHandler(w, r, logger)
	})
	http.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		updateConfiguration(w, r, logger)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
            <head>
            <title>IPMI Exporter</title>
            <style>
            label{
            display:inline-block;
            width:75px;
            }
            form label {
            margin: 10px;
            }
            form input {
            margin: 10px;
            }
            </style>
            </head>
            <body>
            <h1>IPMI Exporter</h1>
            <form action="/ipmi">
            <label>Target:</label> <input type="text" name="target" placeholder="X.X.X.X" value="1.2.3.4"><br>
            <input type="submit" value="Submit">
			</form>
			<p><a href="/metrics">Local metrics</a></p>
			<p><a href="/config">Config</a></p>
            </body>
            </html>`))
	})

	logger.Info(fmt.Sprintf("Listening on %s", *listenAddress))
	err := http.ListenAndServe(*listenAddress, nil)
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}
