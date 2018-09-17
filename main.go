package main

import (
	"context"
	"flag"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	prom "github.com/prometheus/client_golang/prometheus"
	"go.opencensus.io/exporter/prometheus"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const httpTimeout = 2 * time.Minute

var (
	listenAddr = flag.String("listen", ":8080", "Listen Address")
	logLevel   = flag.String("log_level", "INFO", "Log level")
	prod       = flag.Bool("prod", false, "Production mode")

	componentKey, _ = tag.NewKey("component")
	statusKey, _    = tag.NewKey("status")

	requestCount    = stats.Int64("request_total", "Number of requests", stats.UnitDimensionless)
	requestDuration = stats.Float64("request_duration", "Duration of a request", stats.UnitMilliseconds)

	requestCountView = &view.View{
		Measure:     requestCount,
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{componentKey, statusKey},
	}

	requestDurationView = &view.View{
		Measure:     requestDuration,
		Aggregation: view.Distribution(0, 0.01, 0.05, 0.1, 0.3, 0.6, 0.8, 1, 2, 3, 4, 5, 6, 8, 10, 13, 16, 20, 25, 30, 40, 50, 65, 80, 100, 130, 160, 200, 250, 300, 400, 500, 650, 800, 1000, 2000, 5000, 10000, 20000, 50000, 100000),
		TagKeys:     []tag.Key{componentKey, statusKey},
	}
)

func main() {
	flag.Parse()
	initLogging(*logLevel)

	httpServer := startHTTPServer()

	components := []string{"component_a", "component_b", "component_c"}
	statuses := []string{"success", "failure"}

	for i := 0; i < 100; i++ {
		component := components[rand.Intn(len(components))]
		status := statuses[rand.Intn(len(statuses))]
		duration := rand.Intn(5000) + 1

		zap.S().Infow("Doing work", "component", component, "status", status)
		if ctx, err := tag.New(context.Background(), tag.Insert(componentKey, component), tag.Insert(statusKey, status)); err == nil {
			stats.Record(ctx, requestCount.M(1))
			stats.Record(ctx, requestDuration.M(float64(duration)))
		}
	}

	// await interruption
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt)
	<-shutdownChan

	zap.S().Info("Shutting down")

	ctx, cancelFunc := context.WithTimeout(context.Background(), httpTimeout)
	defer cancelFunc()
	httpServer.Shutdown(ctx)
}

func initLogging(level string) {
	var logger *zap.Logger
	var err error
	errorPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.ErrorLevel
	})

	minLogLevel := zapcore.InfoLevel
	switch strings.ToUpper(level) {
	case "DEBUG":
		minLogLevel = zapcore.DebugLevel
	case "INFO":
		minLogLevel = zapcore.InfoLevel
	case "WARN":
		minLogLevel = zapcore.WarnLevel
	case "ERROR":
		minLogLevel = zapcore.ErrorLevel
	}

	infoPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl < zapcore.ErrorLevel && lvl >= minLogLevel
	})

	consoleErrors := zapcore.Lock(os.Stderr)
	consoleInfo := zapcore.Lock(os.Stdout)

	var consoleEncoder zapcore.Encoder
	if *prod || !isatty.IsTerminal(os.Stdout.Fd()) {
		encoderConf := zap.NewProductionEncoderConfig()
		encoderConf.MessageKey = "message"
		encoderConf.EncodeTime = zapcore.TimeEncoder(zapcore.ISO8601TimeEncoder)
		consoleEncoder = zapcore.NewJSONEncoder(encoderConf)
	} else {
		encoderConf := zap.NewDevelopmentEncoderConfig()
		encoderConf.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleEncoder = zapcore.NewConsoleEncoder(encoderConf)
	}

	core := zapcore.NewTee(
		zapcore.NewCore(consoleEncoder, consoleErrors, errorPriority),
		zapcore.NewCore(consoleEncoder, consoleInfo, infoPriority),
	)

	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	stackTraceEnabler := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl > zapcore.ErrorLevel
	})
	logger = zap.New(core, zap.Fields(zap.String("host", host)), zap.AddStacktrace(stackTraceEnabler))

	if err != nil {
		zap.S().Fatalw("Failed to create logger", "error", err)
	}

	zap.ReplaceGlobals(logger.Named("app"))
	zap.RedirectStdLog(logger.Named("stdlog"))
}

func startHTTPServer() *http.Server {
	logger := zap.L().Named("http")

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(ioutil.Discard, r.Body)
		r.Body.Close()

		w.WriteHeader(http.StatusOK)
	})

	promExporter, err := initOCPromExporter()
	if err != nil {
		zap.S().Fatalw("Failed to create Prom exporter", "error", err)
	}
	mux.Handle("/metrics", promExporter)

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ErrorLog:          zap.NewStdLog(logger),
		ReadHeaderTimeout: httpTimeout,
		WriteTimeout:      httpTimeout,
		IdleTimeout:       httpTimeout,
	}

	go func() {
		zap.S().Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			zap.S().Fatalw("Failed to start HTTP server", "error", err)
		}
	}()

	return httpServer
}

func initOCPromExporter() (*prometheus.Exporter, error) {
	if err := view.Register(requestCountView, requestDurationView); err != nil {
		return nil, err
	}

	registry, ok := prom.DefaultRegisterer.(*prom.Registry)
	if !ok {
		zap.S().Warn("Unable to obtain default Prometheus registry. Creating new one.")
		registry = nil
	}

	exporter, err := prometheus.NewExporter(prometheus.Options{Registry: registry})
	if err != nil {
		return nil, err
	}

	view.RegisterExporter(exporter)
	view.SetReportingPeriod(1 * time.Second)

	return exporter, nil
}
