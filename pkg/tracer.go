package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TraceContextKey is the key used to store spans in context
type TraceContextKey string

const (
	// SpanKey is the key used to store span in context
	SpanKey TraceContextKey = "trace-span"
	// SpanIDHeader is the header name for span ID
	SpanIDHeader = "x-trace-span-id"
	// TraceIDHeader is the header name for trace ID
	TraceIDHeader = "x-trace-trace-id"
	// ParentSpanIDHeader is the header name for parent span ID
	ParentSpanIDHeader = "x-trace-parent-span-id"
	// VectorClockHeader is the key for vector clock timestamps in metadata
	VectorClockHeader = "x-vector-clock"
)

// TracerConfig holds configuration for the tracer
type TracerConfig struct {
	// Service name for this tracer
	ServiceName string
	// NodeID is the unique identifier for this node
	NodeID string
	// CollectorAddress is gRPC address of the collector
	CollectorAddress string
	// SamplingRate is the rate at which traces are sampled (0.0-1.0)
	SamplingRate float64
	// BufferSize is the size of the span buffer
	BufferSize int
	// LogDirectory is where span logs are written when collector unavailable
	LogDirectory string
	// FlushInterval is how often to attempt flushing spans
	FlushInterval time.Duration
	// MaxBatchSize is maximum spans to send in one batch
	MaxBatchSize int
	// Sampler is the sampling implementation to use
	Sampler Sampler
	// DebugMode enables verbose logging
	DebugMode bool
}

// SpanOption is a functional option for configuring spans
type SpanOption func(*Span)

// WithParentSpan sets the parent span
func WithParentSpan(parent *Span) SpanOption {
	return func(s *Span) {
		if parent != nil {
			s.ParentSpanID = parent.SpanID
			s.TraceID = parent.TraceID
			// Copy and increment vector clock
			fmt.Println("----line 68 in tracer.go----")
			fmt.Printf("%v",parent.VectorClock)
			s.VectorClock = parent.VectorClock.Copy()
			fmt.Println("----line 71 in tracer.go----")
			fmt.Printf("%v",s)
			s.VectorClock.Increment(s.NodeID)
			
		}
	}
}

// WithAnnotation adds an annotation to the span
func WithAnnotation(key string, value string) SpanOption {
	return func(s *Span) {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations[key] = value
	}
}

// Sampler defines sampling behavior
type Sampler interface {
	// ShouldSample decides whether to sample this trace
	ShouldSample(operation string, traceID string) bool
}

// ProbabilitySampler samples traces based on probability
type ProbabilitySampler struct {
	Rate float64
}

// ShouldSample implements Sampler interface
func (s *ProbabilitySampler) ShouldSample(operation string, traceID string) bool {
	return rand.Float64() < s.Rate
}

// RateLimitingSampler limits traces to N per second
type RateLimitingSampler struct {
	tracesPerSecond int
	currentSecond   int64
	currentCount    int
	mu              sync.Mutex
}

// NewRateLimitingSampler creates a new rate-limiting sampler
func NewRateLimitingSampler(tracesPerSecond int) *RateLimitingSampler {
	return &RateLimitingSampler{
		tracesPerSecond: tracesPerSecond,
		currentSecond:   time.Now().Unix(),
	}
}

// ShouldSample implements Sampler interface
func (s *RateLimitingSampler) ShouldSample(operation string, traceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if now != s.currentSecond {
		s.currentSecond = now
		s.currentCount = 0
	}

	if s.currentCount < s.tracesPerSecond {
		s.currentCount++
		return true
	}

	return false
}

// PrioritySampler samples based on operation priority
type PrioritySampler struct {
	HighPriorityOps map[string]bool
	DefaultRate     float64
}

// ShouldSample implements Sampler interface
func (s *PrioritySampler) ShouldSample(operation string, traceID string) bool {
	if s.HighPriorityOps[operation] {
		return true
	}
	return rand.Float64() < s.DefaultRate
}

// Span represents a tracing span
type Span struct {
	SpanID        string
	TraceID       string
	ParentSpanID  string
	ServiceName   string
	OperationName string
	StartTime     time.Time
	EndTime       time.Time
	Annotations   map[string]string
	Status        proto.Status
	VectorClock   *VectorClock
	NodeID        string
	tracer        *Tracer
}

// AddAnnotation adds an annotation to a span
func (s *Span) AddAnnotation(key string, value interface{}) {
	if s.Annotations == nil {
		s.Annotations = make(map[string]string)
	}

	strValue := fmt.Sprintf("%v", value)
	s.Annotations[key] = strValue
}

// SetStatus sets the status of a span
func (s *Span) SetStatus(status proto.Status) {
	s.Status = status
}

// Finish completes the span and sends it for collection
func (s *Span) Finish() {
	s.EndTime = time.Now()

	// The tracer may be nil in edge cases
	if s.tracer != nil {
		s.tracer.finishSpan(s)
	}
}

// ToProto converts the span to its protobuf representation
func (s *Span) ToProto() *proto.Span {
	annotations := make(map[string]string)
	for k, v := range s.Annotations {
		annotations[k] = v
	}

	return &proto.Span{
		SpanId:        s.SpanID,
		TraceId:       s.TraceID,
		ParentSpanId:  s.ParentSpanID,
		ServiceName:   s.ServiceName,
		OperationName: s.OperationName,
		StartTime:     timestamppb.New(s.StartTime),
		EndTime:       timestamppb.New(s.EndTime),
		Annotations:   annotations,
		Status:        s.Status,
		VectorClock:   s.VectorClock.ToProto(),
		NodeId:        s.NodeID,
	}
}

// Tracer is the main tracing object
type Tracer struct {
	config     TracerConfig
	buffer     *SpanBuffer
	client     proto.CollectorServiceClient
	clientConn *grpc.ClientConn
	cb         *CircuitBreaker
	sync.RWMutex
}

// NewTracer creates a new tracer instance
func NewTracer(config TracerConfig) (*Tracer, error) {
	// Initialize with defaults if not provided
	if config.ServiceName == "" {
		return nil, fmt.Errorf("service name must be provided")
	}

	if config.NodeID == "" {
		config.NodeID = fmt.Sprintf("%s-%s", config.ServiceName, uuid.New().String()[:8])
	}

	if config.SamplingRate == 0 {
		config.SamplingRate = 1.0 // Default to sampling everything
	}

	if config.BufferSize == 0 {
		config.BufferSize = 1000
	}

	if config.LogDirectory == "" {
		config.LogDirectory = os.TempDir()
	}

	if config.FlushInterval == 0 {
		config.FlushInterval = 5 * time.Second
	}

	if config.MaxBatchSize == 0 {
		config.MaxBatchSize = 100
	}

	if config.Sampler == nil {
		config.Sampler = &ProbabilitySampler{Rate: config.SamplingRate}
	}

	// Create the directory if it doesn't exist
	if err := os.MkdirAll(config.LogDirectory, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %v", err)
	}

	// Create buffer and circuit breaker
	buffer := NewSpanBuffer(BufferConfig{
		Size:         config.BufferSize,
		LogDirectory: config.LogDirectory,
	})

	cb := NewCircuitBreaker(CircuitBreakerConfig{
		Threshold:        3,
		Timeout:          10 * time.Second,
		HalfOpenMaxCalls: 1,
	})

	tracer := &Tracer{
		config: config,
		buffer: buffer,
		cb:     cb,
	}

	// Connect to collector if address is provided
	if config.CollectorAddress != "" {
		if err := tracer.connectToCollector(); err != nil {
			fmt.Printf("Warning: Failed to connect to collector: %v, will use local buffering\n", err)
		}
	}

	// Start buffer flush worker
	go tracer.startFlushWorker()

	return tracer, nil
}

// connectToCollector establishes connection to the collector service
func (t *Tracer) connectToCollector() error {
	conn, err := grpc.Dial(
		t.config.CollectorAddress,
		grpc.WithInsecure(), // For development. Use TLS in production
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to collector: %v", err)
	}

	t.Lock()
	defer t.Unlock()

	// Close any existing connection
	if t.clientConn != nil {
		t.clientConn.Close()
	}

	t.clientConn = conn
	t.client = proto.NewCollectorServiceClient(conn)
	return nil
}

// StartSpan creates a new span
func (t *Tracer) StartSpan(operationName string, options ...SpanOption) *Span {
	span := &Span{
		SpanID:        uuid.New().String(),
		TraceID:       uuid.New().String(),
		ServiceName:   t.config.ServiceName,
		OperationName: operationName,
		StartTime:     time.Now(),
		Status:        proto.Status_STATUS_OK,
		VectorClock:   NewVectorClock(t.config.NodeID),
		NodeID:        t.config.NodeID,
		tracer:        t,
	}

	// Apply options
	for _, option := range options {
		option(span)
	}

	// Initialize vector clock if not set via options
	if span.VectorClock == nil {
		span.VectorClock = NewVectorClock(t.config.NodeID)
	}

	// Increment for this operation
	span.VectorClock.Increment(t.config.NodeID)

	// Apply sampling
	if !t.config.Sampler.ShouldSample(operationName, span.TraceID) {
		// Mark as non-sampled but return operable span
		span.AddAnnotation("sampled", "false")
	}

	return span
}

// StartSpanFromContext creates a span linked to parent in context
func (t *Tracer) StartSpanFromContext(ctx context.Context, operationName string, options ...SpanOption) (*Span, context.Context) {
	var opts []SpanOption

	// Try to get parent span from context
	if parentSpan, ok := SpanFromContext(ctx); ok {
		opts = append(opts, WithParentSpan(parentSpan))
	} else {
		// Try to extract from metadata (for RPC calls)
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			spanID := getStringFromMetadata(md, SpanIDHeader)
			traceID := getStringFromMetadata(md, TraceIDHeader)
			parentID := getStringFromMetadata(md, ParentSpanIDHeader)

			if spanID != "" && traceID != "" {
				// Create a virtual parent span from metadata
				parentSpan := &Span{
					SpanID:       spanID,
					TraceID:      traceID,
					ParentSpanID: parentID,
					VectorClock:  NewVectorClock(t.config.NodeID), // Will be populated properly later
				}
				opts = append(opts, WithParentSpan(parentSpan))
			}
		}
	}

	// Add custom options
	opts = append(opts, options...)

	// Create the span
	span := t.StartSpan(operationName, opts...)

	// Create new context with span
	newCtx := ContextWithSpan(ctx, span)

	return span, newCtx
}

// finishSpan handles span completion
func (t *Tracer) finishSpan(span *Span) {
	// Don't process non-sampled spans
	if val, exists := span.Annotations["sampled"]; exists && val == "false" {
		return
	}

	// Try direct submission with fallback to buffer
	if err := t.trySubmitSpan(span); err != nil {
		t.buffer.Add(span)

		if t.config.DebugMode {
			fmt.Printf("Span buffered due to submission error: %v\n", err)
		}
	}
}

// trySubmitSpan attempts to submit span to collector
func (t *Tracer) trySubmitSpan(span *Span) error {
	t.RLock()
	client := t.client
	t.RUnlock()

	// If no client or circuit breaker is open, go to buffer
	if client == nil || t.cb.IsOpen() {
		return fmt.Errorf("collector unavailable")
	}

	// Use circuit breaker pattern
	return t.cb.Execute(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := client.CollectSpan(ctx, &proto.CollectSpanRequest{
			Span: span.ToProto(),
		})

		return err
	})
}

// LogSpan writes span to local log file
func (t *Tracer) LogSpan(span *Span) error {
	filename := filepath.Join(
		t.config.LogDirectory,
		fmt.Sprintf("spans-%s-%s.log",
			t.config.ServiceName,
			time.Now().Format("2006-01-02"),
		),
	)

	// Convert span to JSON
	data, err := json.Marshal(span.ToProto())
	if err != nil {
		return fmt.Errorf("failed to marshal span: %v", err)
	}

	// Append to log file
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to log file: %v", err)
	}

	return nil
}

// startFlushWorker periodically attempts to flush buffer
func (t *Tracer) startFlushWorker() {
	ticker := time.NewTicker(t.config.FlushInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Reconnect if needed
		t.RLock()
		hasClient := t.client != nil
		t.RUnlock()

		if !hasClient {
			if err := t.connectToCollector(); err != nil {
				if t.config.DebugMode {
					fmt.Printf("Failed to reconnect to collector: %v\n", err)
				}
				continue
			}
		}

		// Attempt to flush
		t.RLock()
		client := t.client
		t.RUnlock()

		if client != nil && !t.cb.IsOpen() {
			if err := t.buffer.Flush(client, t.config.MaxBatchSize); err != nil && t.config.DebugMode {
				fmt.Printf("Failed to flush buffer: %v\n", err)
			}
		}
	}
}

// Close finalizes the tracer, flushing any pending spans
func (t *Tracer) Close() error {
	// Final flush attempt
	if t.client != nil {
		if err := t.buffer.Flush(t.client, t.buffer.Size()); err != nil {
			// On failure, ensure all spans are written to logs
			t.buffer.mu.Lock()
			for _, span := range t.buffer.spans {
				t.LogSpan(span)
			}
			t.buffer.mu.Unlock()
		}
	} else {
		// No client, log all spans
		t.buffer.mu.Lock()
		for _, span := range t.buffer.spans {
			t.LogSpan(span)
		}
		t.buffer.mu.Unlock()
	}

	// Close client connection
	if t.clientConn != nil {
		t.clientConn.Close()
	}

	return nil
}

// ContextWithSpan adds a span to context
func ContextWithSpan(ctx context.Context, span *Span) context.Context {
	return context.WithValue(ctx, SpanKey, span)
}

// SpanFromContext extracts a span from context
func SpanFromContext(ctx context.Context) (*Span, bool) {
	span, ok := ctx.Value(SpanKey).(*Span)
	return span, ok
}

func getStringFromMetadata(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}
