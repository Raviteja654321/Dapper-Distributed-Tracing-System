package pkg

import (
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
)

// BufferConfig holds configuration for span buffer
type BufferConfig struct {
	// Size is the maximum number of spans to buffer
	Size int
	// LogDirectory is where buffer is persisted
	LogDirectory string
}

// SpanBuffer buffers spans when collector is unavailable
type SpanBuffer struct {
	spans        []*Span
	config       BufferConfig
	mu           sync.Mutex
	persistTimer *time.Timer
	dirty           bool  // Indicates if buffer has changed since last persist
}

// NewSpanBuffer creates a new buffer for spans
func NewSpanBuffer(config BufferConfig) *SpanBuffer {
	buffer := &SpanBuffer{
		spans:  make([]*Span, 0, config.Size),
		config: config,
	}

	// Attempt to load persisted buffer
	buffer.loadFromDisk()

	// Setup persistence timer
	buffer.persistTimer = time.AfterFunc(30*time.Second, buffer.persistToDisk)

	return buffer
}

// Add adds a span to the buffer
func (b *SpanBuffer) Add(span *Span) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Drop oldest span if buffer is full
	if len(b.spans) >= b.config.Size {
		b.spans = b.spans[1:]
	}

	b.spans = append(b.spans, span)
	b.dirty = true

	// Reset persist timer
	b.persistTimer.Reset(30 * time.Second)
}

// Size returns the number of spans in the buffer
func (b *SpanBuffer) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.spans)
}

// Flush attempts to send buffered spans to collector
func (b *SpanBuffer) Flush(client proto.CollectorServiceClient, batchSize int) error {
	if client == nil {
		return fmt.Errorf("collector client is nil")
	}

	b.mu.Lock()
	if len(b.spans) == 0 {
		b.mu.Unlock()
		return nil
	}

	// Take spans up to batchSize
	count := min(len(b.spans), batchSize)
	batch := make([]*Span, count)
	copy(batch, b.spans[:count])
	b.mu.Unlock()

	// Convert spans to proto and send
	protoSpans := make([]*proto.Span, len(batch))
	for i, span := range batch {
		protoSpans[i] = span.ToProto()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.ReplicateSpans(ctx, &proto.ReplicateSpansRequest{
		Spans:       protoSpans,
		CollectorId: "direct-client", // Identifies this as a client submission
	})

	if err != nil {
		return err
	}

	// Remove successfully sent spans from buffer
	b.mu.Lock()
	// defer b.mu.Unlock()

	// Make sure buffer hasn't changed drastically since we took the batch
	if len(b.spans) >= count {
		b.spans = b.spans[count:]
	} else {
		// Buffer has been modified, just remove the spans we know we sent
		spanMap := make(map[string]bool)
		for _, span := range batch {
			spanMap[span.SpanID] = true
		}

		newBuffer := make([]*Span, 0, len(b.spans))
		for _, span := range b.spans {
			if !spanMap[span.SpanID] {
				newBuffer = append(newBuffer, span)
			}
		}
		b.spans = newBuffer
	}

	// Mark buffer as needing persistence after modification
    b.dirty = true
	b.mu.Unlock()
	// Persist immediately after successful send
	go b.persistToDisk()

	return nil
}

// persistToDisk saves buffer to disk
func (b *SpanBuffer) persistToDisk() {
	b.mu.Lock()
	spans := make([]*Span, len(b.spans))
	copy(spans, b.spans)
	b.mu.Unlock()

	if len(spans) == 0 {
		// even if empty, schedule next persist
        // b.persistTimer = time.AfterFunc(30*time.Second, b.persistToDisk)
		return
	}

	// Convert spans to JSON
	data, err := json.Marshal(spans)
	if err != nil {
		fmt.Printf("Failed to marshal buffer: %v\n", err)
		return
	}

	// Write to file
	bufferPath := filepath.Join(b.config.LogDirectory, "span-buffer.json")
	tmpPath := bufferPath + ".tmp"

	err = ioutil.WriteFile(tmpPath, data, 0644)
	if err != nil {
		fmt.Printf("Failed to write buffer: %v\n", err)
		return
	}

	// Atomic rename
	err = os.Rename(tmpPath, bufferPath)
	if err != nil {
		fmt.Printf("Failed to finalize buffer: %v\n", err)
	}

	// After successful write
    fmt.Printf("Persisted %d spans to disk\n", len(spans))
	
}

// loadFromDisk loads persisted buffer from disk
func (b *SpanBuffer) loadFromDisk() {
	bufferPath := filepath.Join(b.config.LogDirectory, "span-buffer.json")
	data, err := ioutil.ReadFile(bufferPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("Failed to read buffer: %v\n", err)
		}
		return
	}

	var spans []*Span
	if err := json.Unmarshal(data, &spans); err != nil {
		fmt.Printf("Failed to unmarshal buffer: %v\n", err)
		return
	}

	b.mu.Lock()
	b.spans = spans
	b.mu.Unlock()

	fmt.Printf("Loaded %d spans from persisted buffer\n", len(spans))
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	Threshold        int
	Timeout          time.Duration
	HalfOpenMaxCalls int
}

// CircuitBreakerState represents the state of a circuit breaker
type CircuitBreakerState int

const (
	// CircuitClosed means the circuit is closed and requests flow normally
	CircuitClosed CircuitBreakerState = iota
	// CircuitOpen means the circuit is open and requests are blocked
	CircuitOpen
	// CircuitHalfOpen means the circuit is testing if it can be closed
	CircuitHalfOpen
)

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	state                CircuitBreakerState
	failureCount         int
	lastFailureTime      time.Time
	failureThreshold     int
	resetTimeout         time.Duration
	halfOpenMaxCalls     int
	halfOpenSuccessCount int
	halfOpenCallCount    int
	mu                   sync.RWMutex
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		failureCount:     0,
		failureThreshold: config.Threshold,
		resetTimeout:     config.Timeout,
		halfOpenMaxCalls: config.HalfOpenMaxCalls,
	}
}

// Execute runs a function with circuit breaker protection
func (cb *CircuitBreaker) Execute(fn func() error) error {
	cb.mu.RLock()
	state := cb.state
	cb.mu.RUnlock()

	// Check if circuit is open
	if state == CircuitOpen {
		// Check if timeout has elapsed
		cb.mu.Lock()
		if time.Since(cb.lastFailureTime) > cb.resetTimeout {
			cb.state = CircuitHalfOpen
			cb.halfOpenCallCount = 0
			cb.halfOpenSuccessCount = 0
			state = CircuitHalfOpen
		}
		cb.mu.Unlock()

		if state == CircuitOpen {
			return fmt.Errorf("circuit breaker is open")
		}
	}

	// For half-open state, check if we've exceeded the call limit
	if state == CircuitHalfOpen {
		cb.mu.Lock()
		if cb.halfOpenCallCount >= cb.halfOpenMaxCalls {
			cb.mu.Unlock()
			return fmt.Errorf("circuit breaker is half-open and call limit reached")
		}
		cb.halfOpenCallCount++
		cb.mu.Unlock()
	}

	// Execute the function
	err := fn()

	// Handle the result
	if err != nil {
		cb.recordFailure()
		return err
	}

	// Record success
	cb.recordSuccess()
	return nil
}

// recordFailure updates state after a failure
func (cb *CircuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		cb.failureCount++
		if cb.failureCount >= cb.failureThreshold {
			cb.state = CircuitOpen
			cb.lastFailureTime = time.Now()
		}
	case CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.lastFailureTime = time.Now()
	}
}

// recordSuccess updates state after a success
func (cb *CircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		cb.failureCount = 0
	case CircuitHalfOpen:
		cb.halfOpenSuccessCount++
		// If all permitted half-open calls succeed, close the circuit
		if cb.halfOpenSuccessCount >= cb.halfOpenMaxCalls {
			cb.state = CircuitClosed
			cb.failureCount = 0
		}
	}
}

// IsOpen checks if the circuit breaker is open
func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state == CircuitOpen
}

// ExponentialBackoff implements retry with exponential backoff
type ExponentialBackoff struct {
	initial     time.Duration
	max         time.Duration
	multiplier  float64
	currentWait time.Duration
	attempts    int
}

// NewExponentialBackoff creates a new backoff instance
func NewExponentialBackoff(initial, max time.Duration, multiplier float64) *ExponentialBackoff {
	return &ExponentialBackoff{
		initial:     initial,
		max:         max,
		multiplier:  multiplier,
		currentWait: initial,
		attempts:    0,
	}
}

// NextBackoff returns the next backoff duration
func (eb *ExponentialBackoff) NextBackoff() time.Duration {
	wait := eb.currentWait
	eb.attempts++

	// Increase wait time for next attempt
	nextWait := time.Duration(float64(eb.currentWait) * eb.multiplier)
	if nextWait > eb.max {
		nextWait = eb.max
	}
	eb.currentWait = nextWait

	return wait
}

// Reset resets the backoff to initial values
func (eb *ExponentialBackoff) Reset() {
	eb.currentWait = eb.initial
	eb.attempts = 0
}

// RetryWithExponentialBackoff retries a function with exponential backoff
func RetryWithExponentialBackoff(ctx context.Context, fn func() error, maxAttempts int) error {
	backoff := NewExponentialBackoff(100*time.Millisecond, 10*time.Second, 2.0)
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check context before attempting
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := fn()
		if err == nil {
			return nil
		}

		lastErr = err

		// Get next backoff duration
		wait := backoff.NextBackoff()

		// Wait with context cancellation support
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			// Continue to next attempt
		}
	}

	return fmt.Errorf("max attempts reached: %w", lastErr)
}

// DetectCollectorFailure determines if an error indicates collector failure
func DetectCollectorFailure(err error) bool {
	if err == nil {
		return false
	}

	// Check for common gRPC errors that indicate service unavailability
	errStr := err.Error()
	failureIndicators := []string{
		"connection refused",
		"unavailable",
		"deadline exceeded",
		"broken pipe",
		"transport is closing",
		"connection reset",
		"connection closed",
	}

	for _, indicator := range failureIndicators {
		if containsIgnoreCase(errStr, indicator) {
			return true
		}
	}

	return false
}

// containsIgnoreCase checks if a string contains a substring (case-insensitive)
func containsIgnoreCase(s, substr string) bool {
	s, substr = strings.ToLower(s), strings.ToLower(substr)
	return strings.Contains(s, substr)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
