// cmd/visualizer_main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sriharib128/distributed-tracer/pkg"
	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
)

// VisualizerConfig holds configuration for the visualizer
type VisualizerConfig struct {
	HTTPPort         int      `yaml:"http_port"`
	CollectorAddress string   `yaml:"collector_address"`
	BackupCollectors []string `yaml:"backup_collectors"`
	TemplatesDir     string   `yaml:"templates_dir"`
	StaticDir        string   `yaml:"static_dir"`
	Debug            bool     `yaml:"debug"`
}

// Connection info
type collectorConnection struct {
	primary     proto.CollectorServiceClient
	backups     []proto.CollectorServiceClient
	connections []*grpc.ClientConn
}

// Global collector client
var collectorConn collectorConnection

// Template data
type templateData struct {
	Title   string
	Content interface{}
}

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "", "Path to config file")
	port := flag.Int("port", 8080, "HTTP port")
	collectorAddr := flag.String("collector", "localhost:7777", "Collector gRPC address")
	templatesDir := flag.String("templates", "./templates", "Templates directory")
	staticDir := flag.String("static", "./static", "Static files directory")
	debug := flag.Bool("debug", false, "Enable debug mode")
	flag.Parse()

	// Initialize config with defaults
	config := VisualizerConfig{
		HTTPPort:         *port,
		CollectorAddress: *collectorAddr,
		TemplatesDir:     *templatesDir,
		StaticDir:        *staticDir,
		Debug:            *debug,
	}

	// Load config file if specified
	if *configPath != "" {
		if err := loadConfig(*configPath, &config); err != nil {
			fmt.Printf("Error loading config: %v\n", err)
			os.Exit(1)
		}
	}

	// Override with command-line flags if specified
	if *port != 8080 {
		config.HTTPPort = *port
	}
	if *collectorAddr != "localhost:7777" {
		config.CollectorAddress = *collectorAddr
	}
	if *templatesDir != "./templates" {
		config.TemplatesDir = *templatesDir
	}
	if *staticDir != "./static" {
		config.StaticDir = *staticDir
	}

	// Connect to collector
	if err := connectToCollector(config); err != nil {
		fmt.Printf("Failed to connect to collector: %v\n", err)
		os.Exit(1)
	}
	defer closeConnections()

	// Setup HTTP server
	router := setupRoutes(config)

	// Start HTTP server
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.HTTPPort),
		Handler: router,
	}

	go func() {
		fmt.Printf("Visualizer started on http://localhost:%d\n", config.HTTPPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	handleSignals(server)
}

// loadConfig loads configuration from a YAML file
func loadConfig(path string, config *VisualizerConfig) error {
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

// connectToCollector connects to the collector service
func connectToCollector(config VisualizerConfig) error {
	// Connect to primary collector
	primaryConn, err := grpc.Dial(
		config.CollectorAddress,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to primary collector: %v", err)
	}

	collectorConn.primary = proto.NewCollectorServiceClient(primaryConn)
	collectorConn.connections = append(collectorConn.connections, primaryConn)

	// Connect to backup collectors
	for _, addr := range config.BackupCollectors {
		backupConn, err := grpc.Dial(
			addr,
			grpc.WithInsecure(),
			grpc.WithTimeout(3*time.Second),
		)
		if err != nil {
			fmt.Printf("Warning: Failed to connect to backup collector at %s: %v\n", addr, err)
			continue
		}

		client := proto.NewCollectorServiceClient(backupConn)
		collectorConn.backups = append(collectorConn.backups, client)
		collectorConn.connections = append(collectorConn.connections, backupConn)
	}

	return nil
}

// closeConnections closes all gRPC connections
func closeConnections() {
	for _, conn := range collectorConn.connections {
		conn.Close()
	}
}

// setupRoutes configures the HTTP routes
func setupRoutes(config VisualizerConfig) *mux.Router {
	router := mux.NewRouter()

	// Static files
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(config.StaticDir))))

	// Routes
	router.HandleFunc("/", handleHome)
	router.HandleFunc("/traces", handleTraceList)
	router.HandleFunc("/traces/{id}", handleTraceView)
	router.HandleFunc("/system", handleSystemDashboard)
	router.HandleFunc("/dependencies", handleServiceDependencies)
	router.HandleFunc("/anomalies", handleAnomalyDetection)
	router.HandleFunc("/api/traces", apiListTraces)
	router.HandleFunc("/api/traces/{id}", apiGetTrace)
	router.HandleFunc("/api/system", apiGetSystemStatus)
	router.HandleFunc("/api/performance", apiGetPerformanceReport)

	return router
}

// handleSignals handles shutdown signals
func handleSignals(server *http.Server) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		fmt.Printf("Server shutdown error: %v\n", err)
	}
}

// getCollectorClient gets an available collector client
func getCollectorClient() (proto.CollectorServiceClient, error) {
	// Try primary first
	if collectorConn.primary != nil {
		// Check if primary is healthy
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := collectorConn.primary.GetCollectorStatus(ctx, &proto.GetCollectorStatusRequest{})
		if err == nil {
			return collectorConn.primary, nil
		}

		fmt.Printf("Primary collector unavailable: %v\n", err)
	}

	// Try backups
	for _, backup := range collectorConn.backups {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := backup.GetCollectorStatus(ctx, &proto.GetCollectorStatusRequest{})
		cancel()

		if err == nil {
			return backup, nil
		}
	}

	return nil, fmt.Errorf("no collector available")
}

// --- HTTP Handlers ---

func handleHome(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "home.html", templateData{
		Title: "Distributed Tracer",
	})
}

func handleTraceList(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		http.Error(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Parse query parameters
	limit := 20
	if limitParam := r.URL.Query().Get("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 {
			limit = l
		}
	}

	offset := 0
	if offsetParam := r.URL.Query().Get("offset"); offsetParam != "" {
		if o, err := strconv.Atoi(offsetParam); err == nil && o >= 0 {
			offset = o
		}
	}

	service := r.URL.Query().Get("service")
	operation := r.URL.Query().Get("operation")
	status := r.URL.Query().Get("status")

	// Create filter
	filter := &proto.FilterCriteria{
		Limit:         int32(limit),
		Offset:        int32(offset),
		OperationName: operation,
	}

	if service != "" {
		filter.Services = []string{service}
	}

	if status != "" {
		switch status {
		case "error":
			filter.Status = proto.Status_STATUS_ERROR
		case "timeout":
			filter.Status = proto.Status_STATUS_TIMEOUT
		}
	}

	// Get traces
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Prepare template data
	data := struct {
		Traces     []*proto.Trace
		TotalCount int32
		Limit      int
		Offset     int
		Service    string
		Operation  string
		Status     string
	}{
		Traces:     resp.Traces,
		TotalCount: resp.TotalCount,
		Limit:      limit,
		Offset:     offset,
		Service:    service,
		Operation:  operation,
		Status:     status,
	}

	renderTemplate(w, "traces.html", templateData{
		Title:   "Traces",
		Content: data,
	})
}

func handleTraceView(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		http.Error(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get trace ID from URL
	vars := mux.Vars(r)
	traceID := vars["id"]
	if traceID == "" {
		http.Error(w, "Trace ID is required", http.StatusBadRequest)
		return
	}

	// Get trace
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.GetTrace(ctx, &proto.GetTraceRequest{
		TraceId: traceID,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch trace: %v", err), http.StatusInternalServerError)
		return
	}

	if resp.Trace == nil {
		http.Error(w, "Trace not found", http.StatusNotFound)
		return
	}

	// Analyze the trace
	criticalPath := pkg.IdentifyCriticalPath(resp.Trace)
	bottlenecks := pkg.DetectBottlenecks(resp.Trace)
	latencyInfo := pkg.AnalyzeLatency(resp.Trace)

	// Prepare data for template
	data := struct {
		Trace        *proto.Trace
		CriticalPath []*proto.Span
		Bottlenecks  []pkg.Bottleneck
		LatencyInfo  map[string]*pkg.LatencyInfo
	}{
		Trace:        resp.Trace,
		CriticalPath: criticalPath,
		Bottlenecks:  bottlenecks,
		LatencyInfo:  latencyInfo,
	}

	renderTemplate(w, "trace.html", templateData{
		Title:   fmt.Sprintf("Trace %s", traceID),
		Content: data,
	})
}

func handleSystemDashboard(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		http.Error(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get system status
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status, err := client.GetCollectorStatus(ctx, &proto.GetCollectorStatusRequest{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch system status: %v", err), http.StatusInternalServerError)
		return
	}

	// Get recent traces for performance analysis
	filter := &proto.FilterCriteria{
		Limit: 100,
	}

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Generate performance report
	var report *pkg.PerformanceReport
	if len(resp.Traces) > 0 {
		report = pkg.GeneratePerformanceReport(resp.Traces)
	}

	// Prepare data for template
	data := struct {
		Status            *proto.GetCollectorStatusResponse
		PerformanceReport *pkg.PerformanceReport
	}{
		Status:            status,
		PerformanceReport: report,
	}

	renderTemplate(w, "system.html", templateData{
		Title:   "System Dashboard",
		Content: data,
	})
}

func handleServiceDependencies(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		http.Error(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get recent traces for dependency analysis
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	filter := &proto.FilterCriteria{
		Limit: 200, // More traces for better dependency graph
	}

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Generate service dependency graph
	var graph *pkg.ServiceGraph
	if len(resp.Traces) > 0 {
		graph = pkg.GenerateServiceDependencyGraph(resp.Traces)
	} else {
		graph = &pkg.ServiceGraph{
			Services:     []string{},
			Dependencies: []pkg.ServiceDependency{},
		}
	}

	renderTemplate(w, "dependencies.html", templateData{
		Title:   "Service Dependencies",
		Content: graph,
	})
}

func handleAnomalyDetection(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		http.Error(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get recent traces for anomaly detection
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	filter := &proto.FilterCriteria{
		Limit: 100,
	}

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Detect anomalies
	var anomalies []pkg.Anomaly
	if len(resp.Traces) > 0 {
		anomalies = pkg.DetectAnomalies(resp.Traces)
	}

	renderTemplate(w, "anomalies.html", templateData{
		Title:   "Anomaly Detection",
		Content: anomalies,
	})
}

// --- API Handlers ---

func apiListTraces(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		sendJSONError(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Parse query parameters
	limit := 20
	if limitParam := r.URL.Query().Get("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 {
			limit = l
		}
	}

	offset := 0
	if offsetParam := r.URL.Query().Get("offset"); offsetParam != "" {
		if o, err := strconv.Atoi(offsetParam); err == nil && o >= 0 {
			offset = o
		}
	}

	service := r.URL.Query().Get("service")
	operation := r.URL.Query().Get("operation")

	// Create filter
	filter := &proto.FilterCriteria{
		Limit:         int32(limit),
		Offset:        int32(offset),
		OperationName: operation,
	}

	if service != "" {
		filter.Services = []string{service}
	}

	// Get traces
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func apiGetTrace(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		sendJSONError(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get trace ID from URL
	vars := mux.Vars(r)
	traceID := vars["id"]
	if traceID == "" {
		sendJSONError(w, "Trace ID is required", http.StatusBadRequest)
		return
	}

	// Get trace
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.GetTrace(ctx, &proto.GetTraceRequest{
		TraceId: traceID,
	})

	if err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to fetch trace: %v", err), http.StatusInternalServerError)
		return
	}

	if resp.Trace == nil {
		sendJSONError(w, "Trace not found", http.StatusNotFound)
		return
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func apiGetSystemStatus(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		sendJSONError(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get system status
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status, err := client.GetCollectorStatus(ctx, &proto.GetCollectorStatusRequest{})
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to fetch system status: %v", err), http.StatusInternalServerError)
		return
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func apiGetPerformanceReport(w http.ResponseWriter, r *http.Request) {
	client, err := getCollectorClient()
	if err != nil {
		sendJSONError(w, "Collector service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get recent traces for performance analysis
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	filter := &proto.FilterCriteria{
		Limit: 100,
	}

	resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
		Filter: filter,
	})

	if err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to fetch traces: %v", err), http.StatusInternalServerError)
		return
	}

	// Generate performance report
	var report *pkg.PerformanceReport
	if len(resp.Traces) > 0 {
		report = pkg.GeneratePerformanceReport(resp.Traces)
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

// Add this before your renderTemplate function
func createTemplateFuncsMap() template.FuncMap {
    return template.FuncMap{
        "add": func(a, b int) int {
            return a + b
        },
        "subtract": func(a, b int) int {
            return a - b
        },
        "spanDuration": func(span *proto.Span) float64 {
            if span == nil || span.StartTime == nil || span.EndTime == nil {
                return 0.0
            }
            duration := span.EndTime.AsTime().Sub(span.StartTime.AsTime())
            return float64(duration.Milliseconds())
        },
        "spanPosition": func(span *proto.Span, traceStart time.Time) float64 {
            if span == nil || span.StartTime == nil {
                return 0.0
            }
            return span.StartTime.AsTime().Sub(traceStart).Seconds() * 1000
        },
        "spanWidth": func(span *proto.Span) float64 {
            if span == nil || span.StartTime == nil || span.EndTime == nil {
                return 0.0
            }
            duration := span.EndTime.AsTime().Sub(span.StartTime.AsTime())
            return float64(duration.Milliseconds())
        },
    }
}

func renderTemplate(w http.ResponseWriter, tmpl string, data templateData) {
    // In a real application, templates would be compiled once and reused
    t, err := template.New("layout.html").Funcs(createTemplateFuncsMap()).ParseFiles("templates/layout.html", "templates/"+tmpl)
    if err != nil {
        log.Printf("Template error: %v", err)
        http.Error(w, "Template error", http.StatusInternalServerError)
        return
    }

    if err := t.ExecuteTemplate(w, "layout", data); err != nil {
        log.Printf("Template execution error: %v", err)
        return
    }
}

// sendJSONError sends a JSON error response
func sendJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
