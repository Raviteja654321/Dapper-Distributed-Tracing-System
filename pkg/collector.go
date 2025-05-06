// pkg/collector.go
package pkg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CollectorConfig holds configuration for the collector
type CollectorConfig struct {
	// ID is the unique identifier for this collector
	ID string
	// StoragePath is where traces are stored
	StoragePath string
	// LogScanInterval is how often to scan for log files
	LogScanInterval time.Duration
	// LogDirectories are directories to scan for log files
	LogDirectories []string
	// ReplicaAddresses are addresses of other collector replicas
	ReplicaAddresses []string
	// MaxConcurrentProcessors is max number of goroutines processing logs
	MaxConcurrentProcessors int
	// RetentionPeriod is how long to keep traces
	RetentionPeriod time.Duration
	// Storage is the storage backend to use
	Storage StorageInterface
}

// Collector is the main trace collection system
type Collector struct {
	config             CollectorConfig
	storage            StorageInterface
	replicaClients     map[string]proto.CollectorServiceClient
	replicaConnections map[string]*grpc.ClientConn
	stopCh             chan struct{}
	spansProcessed     int32
	tracesStored       int32
	startTime          time.Time
	queueDepth         int32
	mu                 sync.RWMutex
	processorSem       chan struct{}
}

// NewCollector creates a new collector instance
func NewCollector(config CollectorConfig) (*Collector, error) {
	// Set defaults for unspecified values
	if config.ID == "" {
		config.ID = fmt.Sprintf("collector-%s", time.Now().Format("20060102-150405"))
	}

	if config.StoragePath == "" {
		config.StoragePath = "./trace-storage"
	}

	if config.LogScanInterval == 0 {
		config.LogScanInterval = 1 * time.Minute
	}

	if config.MaxConcurrentProcessors == 0 {
		config.MaxConcurrentProcessors = 5
	}

	if config.RetentionPeriod == 0 {
		config.RetentionPeriod = 7 * 24 * time.Hour // 7 days
	}

	// Create storage backend if not provided
	var storage StorageInterface
	if config.Storage != nil {
		storage = config.Storage
	} else {
		var err error
		storage, err = NewFileStorage(config.StoragePath)
		if err != nil {
			return nil, fmt.Errorf("failed to create storage: %v", err)
		}
	}

	collector := &Collector{
		config:             config,
		storage:            storage,
		replicaClients:     make(map[string]proto.CollectorServiceClient),
		replicaConnections: make(map[string]*grpc.ClientConn),
		stopCh:             make(chan struct{}),
		startTime:          time.Now(),
		processorSem:       make(chan struct{}, config.MaxConcurrentProcessors),
	}

	// Create storage directory if it doesn't exist
	if err := os.MkdirAll(config.StoragePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %v", err)
	}

	// Connect to replicas
	for _, addr := range config.ReplicaAddresses {
		if err := collector.connectToReplica(addr); err != nil {
			fmt.Printf("Warning: Failed to connect to replica at %s: %v\n", addr, err)
		}
	}

	return collector, nil
}

// StartCollection starts the collection process
func (c *Collector) StartCollection() error {
	// Start log scanning
	go c.startLogScanningLoop()

	// Start pruning old traces
	go c.startPruningLoop()

	return nil
}

// Stop stops the collector
func (c *Collector) Stop() {
	close(c.stopCh)

	// Close all replica connections
	for addr, conn := range c.replicaConnections {
		conn.Close()
		delete(c.replicaConnections, addr)
		delete(c.replicaClients, addr)
	}
}

// connectToReplica establishes a connection to a replica collector
func (c *Collector) connectToReplica(addr string) error {
	conn, err := grpc.Dial(
		addr,
		grpc.WithInsecure(), // For development. Use TLS in production
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to replica: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.replicaConnections[addr] = conn
	c.replicaClients[addr] = proto.NewCollectorServiceClient(conn)

	return nil
}

// startLogScanningLoop periodically scans for log files
func (c *Collector) startLogScanningLoop() {
	ticker := time.NewTicker(c.config.LogScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.ScanLogFiles()
		}
	}
}

// startPruningLoop periodically prunes old traces
func (c *Collector) startPruningLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.PruneOldTraces()
		}
	}
}

// ScanLogFiles scans directories for new log files
func (c *Collector) ScanLogFiles() {
	for _, dir := range c.config.LogDirectories {
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			fmt.Printf("Error scanning directory %s: %v\n", dir, err)
			continue
		}

		for _, file := range files {
			if file.IsDir() {
				continue
			}

			if strings.HasPrefix(file.Name(), "spans-") && strings.HasSuffix(file.Name(), ".log") {
				// Process file in a goroutine
				filePath := filepath.Join(dir, file.Name())
				c.queueFileProcessing(filePath)
			}
		}
	}
}

// queueFileProcessing queues a file for processing
func (c *Collector) queueFileProcessing(filePath string) {
	// Use semaphore to limit concurrency
	go func() {
		c.processorSem <- struct{}{}
		defer func() { <-c.processorSem }()

		if err := c.ProcessLogFile(filePath); err != nil {
			fmt.Printf("Error processing log file %s: %v\n", filePath, err)
		}
	}()
}

// ProcessLogFile processes a log file and extracts spans
func (c *Collector) ProcessLogFile(filePath string) error {
	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return fmt.Errorf("file does not exist: %s", filePath)
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Create a temporary file for processed spans
	processedPath := filePath + ".processed"
	processed, err := os.OpenFile(processedPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create processed file: %v", err)
	}
	defer processed.Close()

	// Read file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// Parse JSON into span
		var protoSpan proto.Span
		if err := json.Unmarshal([]byte(line), &protoSpan); err != nil {
			fmt.Printf("Error parsing span JSON: %v\n", err)
			continue
		}

		// Store the span
		err := c.StoreSpan(&protoSpan)
		if err != nil {
			fmt.Printf("Error storing span: %v\n", err)
			continue
		}

		c.mu.Lock()
		c.spansProcessed++
		c.mu.Unlock()

		// Write to processed file to mark as processed
		if _, err := processed.WriteString(line + "\n"); err != nil {
			fmt.Printf("Error writing to processed file: %v\n", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error scanning file: %v", err)
	}

	// Rename the original file to indicate it's processed
	backupPath := filePath + ".processed." + time.Now().Format("20060102-150405")
	if err := os.Rename(filePath, backupPath); err != nil {
		return fmt.Errorf("failed to rename processed file: %v", err)
	}

	// Remove the temporary processed file
	if err := os.Remove(processedPath); err != nil {
		fmt.Printf("Warning: Failed to remove temporary file: %v\n", err)
	}

	return nil
}

// StoreSpan stores a span in the central repository
func (c *Collector) StoreSpan(span *proto.Span) error {
	if span == nil {
		return fmt.Errorf("span is nil")
	}

	// Get the trace or create a new one
	trace, err := c.storage.GetTrace(span.TraceId)
	if err != nil {
		// Create a new trace
		trace = &proto.Trace{
			TraceId: span.TraceId,
			Spans:   make([]*proto.Span, 0, 10),
		}

		// If this is a root span, set trace metadata
		if span.ParentSpanId == "" {
			trace.RootService = span.ServiceName
			trace.RootOperation = span.OperationName
			trace.StartTime = span.StartTime
		}
	}

	// Add span to trace
	trace.Spans = append(trace.Spans, span)

	// Update trace end time if needed
	if trace.EndTime == nil || span.EndTime.AsTime().After(trace.EndTime.AsTime()) {
		trace.EndTime = span.EndTime
	}

	// Store the updated trace
	if err := c.storage.StoreTrace(trace); err != nil {
		return err
	}

	c.mu.Lock()
	c.tracesStored++
	c.mu.Unlock()

	// Replicate to other collectors if needed
	go c.ReplicateToBackups([]*proto.Span{span})

	return nil
}

// GetTrace retrieves a trace by ID
func (c *Collector) GetTrace(traceID string) (*proto.Trace, error) {
	trace, err := c.storage.GetTrace(traceID)
	if err != nil {
		return nil, err
	}

	// Build hierarchical trace tree
	trace = c.BuildTraceTree(trace)

	return trace, nil
}

// BuildTraceTree builds a hierarchical structure from flat spans
func (c *Collector) BuildTraceTree(trace *proto.Trace) *proto.Trace {
	if trace == nil || len(trace.Spans) <= 1 {
		return trace
	}

	// First order spans based on vector clocks
	trace.Spans = OrderSpans(trace.Spans)

	return trace
}

// ListTraces lists traces matching filter criteria
func (c *Collector) ListTraces(filter *proto.FilterCriteria) ([]*proto.Trace, error) {
	return c.storage.ListTraces(filter)
}

// ReplicateToBackups replicates spans to backup collectors
func (c *Collector) ReplicateToBackups(spans []*proto.Span) {
	if len(spans) == 0 {
		return
	}

	c.mu.RLock()
	clients := make(map[string]proto.CollectorServiceClient)
	for addr, client := range c.replicaClients {
		clients[addr] = client
	}
	c.mu.RUnlock()

	for addr, client := range clients {
		go func(addr string, client proto.CollectorServiceClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := client.ReplicateSpans(ctx, &proto.ReplicateSpansRequest{
				Spans:       spans,
				CollectorId: c.config.ID,
			})

			if err != nil {
				fmt.Printf("Failed to replicate to %s: %v\n", addr, err)

				// Try to reconnect if needed
				if strings.Contains(err.Error(), "unavailable") {
					c.connectToReplica(addr)
				}
			}
		}(addr, client)
	}
}

// MergeFromReplicas reconciles data from multiple collector instances
func (c *Collector) MergeFromReplicas() {
	// For each replica
	c.mu.RLock()
	clients := make(map[string]proto.CollectorServiceClient)
	for addr, client := range c.replicaClients {
		clients[addr] = client
	}
	c.mu.RUnlock()

	// Use a filter to get recent traces
	filter := &proto.FilterCriteria{
		StartTime: timestamppb.New(time.Now().Add(-1 * time.Hour)),
		Limit:     1000,
	}

	for addr, client := range clients {
		go func(addr string, client proto.CollectorServiceClient) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.ListTraces(ctx, &proto.ListTracesRequest{
				Filter: filter,
			})

			if err != nil {
				fmt.Printf("Failed to get traces from %s: %v\n", addr, err)
				return
			}

			// Process each trace
			for _, trace := range resp.Traces {
				localTrace, err := c.storage.GetTrace(trace.TraceId)
				if err != nil || localTrace == nil {
					// Trace doesn't exist locally, store it
					if err := c.storage.StoreTrace(trace); err != nil {
						fmt.Printf("Failed to store trace %s: %v\n", trace.TraceId, err)
					}
					continue
				}

				// Trace exists, merge spans
				spanMap := make(map[string]bool)
				for _, span := range localTrace.Spans {
					spanMap[span.SpanId] = true
				}

				// Add missing spans
				for _, span := range trace.Spans {
					if !spanMap[span.SpanId] {
						localTrace.Spans = append(localTrace.Spans, span)
					}
				}

				// Update trace metadata if needed
				if trace.EndTime.AsTime().After(localTrace.EndTime.AsTime()) {
					localTrace.EndTime = trace.EndTime
				}

				if err := c.storage.StoreTrace(localTrace); err != nil {
					fmt.Printf("Failed to update trace %s: %v\n", trace.TraceId, err)
				}
			}
		}(addr, client)
	}
}

// PruneOldTraces removes traces older than the retention period
func (c *Collector) PruneOldTraces() error {
	cutoff := time.Now().Add(-c.config.RetentionPeriod)
	return c.storage.PruneTraces(cutoff)
}

// GetCollectorStatus returns the current collector status
func (c *Collector) GetCollectorStatus() *proto.GetCollectorStatusResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return &proto.GetCollectorStatusResponse{
		Healthy:        true,
		SpansProcessed: c.spansProcessed,
		TracesStored:   c.tracesStored,
		UpSince:        timestamppb.New(c.startTime),
		QueueDepth:     c.queueDepth,
	}
}

// CollectorServer implements the gRPC collector service
type CollectorServer struct {
	proto.UnimplementedCollectorServiceServer
	collector *Collector
}

// NewCollectorServer creates a new collector server
func NewCollectorServer(collector *Collector) *CollectorServer {
	return &CollectorServer{
		collector: collector,
	}
}

// CollectSpan implements CollectorService.CollectSpan
func (s *CollectorServer) CollectSpan(ctx context.Context, req *proto.CollectSpanRequest) (*proto.CollectSpanResponse, error) {
	if req.Span == nil {
		return nil, status.Error(codes.InvalidArgument, "span is required")
	}

	err := s.collector.StoreSpan(req.Span)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to store span: %v", err)
	}

	return &proto.CollectSpanResponse{Success: true}, nil
}

// GetTrace implements CollectorService.GetTrace
func (s *CollectorServer) GetTrace(ctx context.Context, req *proto.GetTraceRequest) (*proto.GetTraceResponse, error) {
	if req.TraceId == "" {
		return nil, status.Error(codes.InvalidArgument, "trace ID is required")
	}

	trace, err := s.collector.GetTrace(req.TraceId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "trace not found: %v", err)
	}

	return &proto.GetTraceResponse{Trace: trace}, nil
}

// ListTraces implements CollectorService.ListTraces
func (s *CollectorServer) ListTraces(ctx context.Context, req *proto.ListTracesRequest) (*proto.ListTracesResponse, error) {
	filter := req.Filter
	if filter == nil {
		filter = &proto.FilterCriteria{
			Limit: 100, // Default limit
		}
	}

	traces, err := s.collector.ListTraces(filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list traces: %v", err)
	}

	return &proto.ListTracesResponse{
		Traces:     traces,
		TotalCount: int32(len(traces)),
	}, nil
}

// GetCollectorStatus implements CollectorService.GetCollectorStatus
func (s *CollectorServer) GetCollectorStatus(ctx context.Context, req *proto.GetCollectorStatusRequest) (*proto.GetCollectorStatusResponse, error) {
	return s.collector.GetCollectorStatus(), nil
}

// ReplicateSpans implements CollectorService.ReplicateSpans
func (s *CollectorServer) ReplicateSpans(ctx context.Context, req *proto.ReplicateSpansRequest) (*proto.ReplicateSpansResponse, error) {
	if req.CollectorId == s.collector.config.ID {
		// Skip replication from ourselves to avoid loops
		return &proto.ReplicateSpansResponse{Success: true}, nil
	}

	failedSpans := make([]string, 0)

	for _, span := range req.Spans {
		if err := s.collector.StoreSpan(span); err != nil {
			failedSpans = append(failedSpans, span.SpanId)
		}
	}

	return &proto.ReplicateSpansResponse{
		Success:       len(failedSpans) == 0,
		FailedSpanIds: failedSpans,
	}, nil
}

// SyncClock implements CollectorService.SyncClock
func (s *CollectorServer) SyncClock(ctx context.Context, req *proto.SyncClockRequest) (*proto.SyncClockResponse, error) {
	localTime := time.Now()
	clientTime := req.LocalTime.AsTime()

	// Calculate estimated drift (positive means client is ahead)
	driftMs := clientTime.Sub(localTime).Milliseconds()

	return &proto.SyncClockResponse{
		CollectorTime:    timestamppb.New(localTime),
		EstimatedDriftMs: driftMs,
	}, nil
}
