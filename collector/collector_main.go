// cmd/collector_main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sriharib128/distributed-tracer/pkg"
	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"gopkg.in/yaml.v2"
)

// CollectorConfig holds the collector configuration
type CollectorConfig struct {
	ID               string   `yaml:"id"`
	GRPCPort         int      `yaml:"grpc_port"`
	StoragePath      string   `yaml:"storage_path"`
	LogDirectories   []string `yaml:"log_directories"`
	ReplicaAddresses []string `yaml:"replica_addresses"`
	MaxProcessors    int      `yaml:"max_processors"`
	RetentionDays    int      `yaml:"retention_days"`
	Debug            bool     `yaml:"debug"`
}

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "", "Path to config file")
	port := flag.Int("port", 7777, "gRPC port")
	storagePath := flag.String("storage", "./trace-storage", "Storage path")
	logDirs := flag.String("log-dirs", "", "Comma-separated log directories")
	replicas := flag.String("replicas", "", "Comma-separated replica addresses")
	debug := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()

	// Initialize config with defaults
	config := CollectorConfig{
		ID:            fmt.Sprintf("collector-%s", time.Now().Format("20060102-150405")),
		GRPCPort:      *port,
		StoragePath:   *storagePath,
		MaxProcessors: 5,
		RetentionDays: 7,
		Debug:         *debug,
	}

	// Load config file if specified
	if *configPath != "" {
		if err := loadConfig(*configPath, &config); err != nil {
			fmt.Printf("Error loading config: %v\n", err)
			os.Exit(1)
		}
	}

	// Override with command-line flags if specified
	if *port != 7777 {
		config.GRPCPort = *port
	}
	if *storagePath != "./trace-storage" {
		config.StoragePath = *storagePath
	}
	if *logDirs != "" {
		config.LogDirectories = strings.Split(*logDirs, ",")
	}
	if *replicas != "" {
		config.ReplicaAddresses = strings.Split(*replicas, ",")
	}

	// Setup collector
	collector, err := setupCollector(config)
	if err != nil {
		fmt.Printf("Failed to setup collector: %v\n", err)
		os.Exit(1)
	}

	// Create a main context that will be used for coordination
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a WaitGroup to coordinate shutdown
	var wg sync.WaitGroup

	// Start gRPC server
	server, err := startGRPCServer(ctx, &wg, collector, config.GRPCPort)
	if err != nil {
		fmt.Printf("Failed to start gRPC server: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Collector %s started on port %d\n", config.ID, config.GRPCPort)

	// Start collection processes
	err = collector.StartCollection()
	if err != nil {
		fmt.Printf("Failed to start collection: %v\n", err)
		os.Exit(1)
	}

	// Start periodic analysis
	wg.Add(1)
	go func() {
		defer wg.Done()
		startAnalysisWorker(ctx, collector)
	}()

	// Wait for shutdown signal and handle shutdown
	handleSignals(ctx, cancel, server, collector, &wg)
}

// loadConfig loads configuration from a YAML file
func loadConfig(path string, config *CollectorConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	err = yaml.Unmarshal(data, config)
	if err != nil {
		return fmt.Errorf("failed to parse config file: %v", err)
	}

	return nil
}

// setupCollector creates and configures a collector
func setupCollector(config CollectorConfig) (*pkg.Collector, error) {
	// Setup storage
	storage, err := pkg.NewFileStorage(config.StoragePath)
	fmt.Printf("storage setup at %v ",config.StoragePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %v", err)
	}

	// Create collector config
	collectorConfig := pkg.CollectorConfig{
		ID:                      config.ID,
		StoragePath:             config.StoragePath,
		LogScanInterval:         1 * time.Minute,
		LogDirectories:          config.LogDirectories,
		ReplicaAddresses:        config.ReplicaAddresses,
		MaxConcurrentProcessors: config.MaxProcessors,
		RetentionPeriod:         time.Duration(config.RetentionDays) * 24 * time.Hour,
		Storage:                 storage,
	}

	// Create collector
	collector, err := pkg.NewCollector(collectorConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create collector: %v", err)
	}

	return collector, nil
}

// startGRPCServer starts the gRPC server for the collector
func startGRPCServer(ctx context.Context, wg *sync.WaitGroup, collector *pkg.Collector, port int) (*grpc.Server, error) {
	// Create gRPC server
	server := grpc.NewServer(
		grpc.UnaryInterceptor(pkg.RecoveryInterceptor()),
	)

	// Register collector service
	collectorServer := pkg.NewCollectorServer(collector)
	proto.RegisterCollectorServiceServer(server, collectorServer)

	// Enable reflection for debugging
	reflection.Register(server)

	// Start listening
	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %v", err)
	}

	// Start server in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		
		// Serve will block until the server is stopped or errors
		if err := server.Serve(listener); err != nil {
			fmt.Printf("gRPC server error: %v\n", err)
		}
	}()

	// Monitor context for cancellation
	go func() {
		<-ctx.Done()
		fmt.Println("Context cancelled, stopping gRPC server...")
		server.GracefulStop()
	}()

	return server, nil
}

// handleSignals handles shutdown signals
func handleSignals(ctx context.Context, cancel context.CancelFunc, server *grpc.Server, collector *pkg.Collector, wg *sync.WaitGroup) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	fmt.Printf("Received signal %v, initiating shutdown...\n", sig)
	
	// Cancel context to notify all goroutines
	cancel()
	
	// Stop gRPC server gracefully
	fmt.Println("Stopping gRPC server...")
	server.GracefulStop()
	
	// Stop collector
	fmt.Println("Stopping collector...")
	collector.Stop()
	// if err := collector.Stop(); err != nil {
	// 	fmt.Printf("Error stopping collector: %v\n", err)
	// }
	
	// Wait for all goroutines to finish with timeout
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()
	
	select {
	case <-waitCh:
		fmt.Println("All tasks completed, shutdown successful")
	case <-time.After(10 * time.Second):
		fmt.Println("Some tasks did not complete in time, forcing shutdown")
	}
}

// startAnalysisWorker periodically runs analysis on collected traces
func startAnalysisWorker(ctx context.Context, collector *pkg.Collector) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Analysis worker shutting down...")
			return
		case <-ticker.C:
			runAnalysis(collector)
		}
	}
}

// runAnalysis performs analysis on collected traces
func runAnalysis(collector *pkg.Collector) {
	// Get recent traces for analysis
	filter := &proto.FilterCriteria{
		Limit: 100,
	}

	traces, err := collector.ListTraces(filter)
	if err != nil {
		fmt.Printf("Failed to list traces for analysis: %v\n", err)
		return
	}

	if len(traces) == 0 {
		fmt.Println("No traces available for analysis")
		return
	}

	// Generate performance report
	report := pkg.GeneratePerformanceReport(traces)

	// Log anomalies
	if len(report.Anomalies) > 0 {
		fmt.Printf("Detected %d anomalies:\n", len(report.Anomalies))
		for i, anomaly := range report.Anomalies {
			if i >= 5 {
				fmt.Printf("... and %d more\n", len(report.Anomalies)-5)
				break
			}
			fmt.Printf("- [%s] %s/%s: %s (severity: %d)\n",
				anomaly.Type, anomaly.ServiceName, anomaly.OperationName,
				anomaly.Description, anomaly.Severity)
		}
	}

	// Log bottlenecks
	if len(report.Bottlenecks) > 0 {
		fmt.Printf("Top performance bottlenecks:\n")
		for i, bottleneck := range report.Bottlenecks {
			if i >= 3 {
				break
			}
			fmt.Printf("- %s/%s: %.2fms (%.1f%% of total time)\n",
				bottleneck.ServiceName, bottleneck.OperationName,
				float64(bottleneck.LatencyMs), bottleneck.PercentOfTotal)
		}
	}
}