package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/mockaws"
)

func main() {
	var (
		listen     = flag.String("listen", ":18080", "listen address")
		scale      = flag.String("scale", "small", "account size profile: small|medium|large")
		instances  = flag.Int("instances", 0, "override instance count")
		regions    = flag.String("regions", "", "comma-separated regions (default profile regions)")
		seed       = flag.Int64("seed", time.Now().UnixNano(), "random seed")
		tick       = flag.Duration("tick", 2*time.Second, "simulation tick interval")
		minRun     = flag.Duration("min-run", 20*time.Minute, "minimum instance runtime before natural termination")
		maxRun     = flag.Duration("max-run", 2*time.Hour, "maximum instance runtime before natural termination")
		churnProb  = flag.Float64("churn-probability", 0.01, "extra per-tick random natural termination probability")
		fisWarning = flag.Duration("fis-warning-window", 2*time.Minute, "time between warning and termination in mock FIS")
	)
	flag.Parse()

	cfg := mockaws.DefaultConfig(mockaws.ScaleProfile(strings.ToLower(strings.TrimSpace(*scale))))
	cfg.Seed = *seed
	cfg.TickInterval = *tick
	cfg.MinRunTime = *minRun
	cfg.MaxRunTime = *maxRun
	cfg.ChurnProbability = *churnProb
	cfg.FISWarningWindow = *fisWarning
	if *instances > 0 {
		cfg.InstanceCount = *instances
	}
	if strings.TrimSpace(*regions) != "" {
		cfg.Regions = splitCSV(*regions)
	}

	sim := mockaws.New(cfg)
	sim.Start()
	server := mockaws.NewServer(sim)
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("mockaws started on %s", *listen)
	log.Printf("scale=%s instances=%d regions=%s seed=%d", cfg.Scale, cfg.InstanceCount, strings.Join(cfg.Regions, ","), cfg.Seed)
	log.Printf("example endpoints: /api/ec2/instances , /api/fis/experiments")
	endpoint := endpointURL(*listen)
	log.Printf("Set endpoint override: export ENDPOINT=%s", endpoint)
	log.Printf("AWS CLI example: aws ec2 describe-instances --endpoint-url \"$ENDPOINT\"")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sim.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
	fmt.Println("mockaws stopped")
}

func splitCSV(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func endpointURL(listen string) string {
	trimmed := strings.TrimSpace(listen)
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	if strings.HasPrefix(trimmed, ":") {
		return "http://127.0.0.1" + trimmed
	}
	if strings.Contains(trimmed, "://") {
		return trimmed
	}
	return "http://" + trimmed
}
