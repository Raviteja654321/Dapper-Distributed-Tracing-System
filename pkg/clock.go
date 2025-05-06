package pkg

import (
	"context"
	"encoding/json"
	"fmt"

	// "fmt"
	"sync"
	"time"

	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// VectorClock represents a vector clock for event ordering
type VectorClock struct {
	Timestamps map[string]uint64
	mu         sync.RWMutex
}

// NewVectorClock creates a new vector clock
func NewVectorClock(nodeID string) *VectorClock {
	vc := &VectorClock{
		Timestamps: make(map[string]uint64),
	}
	vc.Timestamps[nodeID] = 1
	return vc
}

// Increment increases the local component of the clock
func (vc *VectorClock) Increment(nodeID string) {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	if _, exists := vc.Timestamps[nodeID]; !exists {
		vc.Timestamps[nodeID] = 0
	}

	vc.Timestamps[nodeID]++
}

// Merge combines two vector clocks (taking maximum values)
func (vc *VectorClock) Merge(other *VectorClock) {
	if other == nil {
		return
	}

	vc.mu.Lock()
	defer vc.mu.Unlock()

	other.mu.RLock()
	defer other.mu.RUnlock()

	for node, time := range other.Timestamps {
		if currentTime, exists := vc.Timestamps[node]; !exists || time > currentTime {
			vc.Timestamps[node] = time
		}
	}
}

// Compare determines the relationship between two vector clocks
// Returns:
//
//	-1: vc happens before other
//	 0: vc and other are concurrent
//	 1: vc happens after other
func (vc *VectorClock) Compare(other *VectorClock) int {
	if other == nil {
		return 1
	}

	vc.mu.RLock()
	defer vc.mu.RUnlock()

	other.mu.RLock()
	defer other.mu.RUnlock()

	// Check if vc <= other (all values in vc are <= corresponding values in other)
	lessThanOrEqual := true
	for node, time := range vc.Timestamps {
		if otherTime, exists := other.Timestamps[node]; !exists || time > otherTime {
			lessThanOrEqual = false
			break
		}
	}

	// Check if other <= vc (all values in other are <= corresponding values in vc)
	greaterThanOrEqual := true
	for node, time := range other.Timestamps {
		if vcTime, exists := vc.Timestamps[node]; !exists || time > vcTime {
			greaterThanOrEqual = false
			break
		}
	}

	if lessThanOrEqual && !greaterThanOrEqual {
		return -1 // vc happens before other
	} else if !lessThanOrEqual && greaterThanOrEqual {
		return 1 // vc happens after other
	} else {
		return 0 // concurrent or equal
	}
}

// Copy creates a deep copy of the vector clock
func (vc *VectorClock) Copy() *VectorClock {
	fmt.Println("---line 105 in clock.go----")
	if vc == nil {
		return &VectorClock{
			Timestamps: make(map[string]uint64),
		}
	}
	// modify to use rlock only if vc.mu !=nil	
	vc.mu.RLock()
	defer vc.mu.RUnlock()
	
	fmt.Println("---line 108 in clock.go----")

	newVC := &VectorClock{
		Timestamps: make(map[string]uint64),
	}

	for node, time := range vc.Timestamps {
		newVC.Timestamps[node] = time
	}

	return newVC
}

// ToProto converts the vector clock to its protobuf representation
func (vc *VectorClock) ToProto() *proto.VectorClock {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	timestamps := make(map[string]uint64)
	for node, time := range vc.Timestamps {
		timestamps[node] = time
	}

	return &proto.VectorClock{
		Timestamps: timestamps,
	}
}

// FromProto creates a VectorClock from its protobuf representation
func VectorClockFromProto(protoVC *proto.VectorClock) *VectorClock {
	vc := &VectorClock{
		Timestamps: make(map[string]uint64),
	}

	if protoVC != nil {
		for node, time := range protoVC.Timestamps {
			vc.Timestamps[node] = time
		}
	}

	return vc
}

// SynchronizeClock periodically synchronizes with the collector
func SynchronizeClock(ctx context.Context, collectorClient proto.CollectorServiceClient, nodeID string) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			resp, err := collectorClient.SyncClock(syncCtx, &proto.SyncClockRequest{
				LocalTime: timestamppb.New(time.Now()),
				NodeId:    nodeID,
			})
			cancel()

			if err == nil && resp != nil {
				// Log the drift for monitoring
				driftMs := resp.EstimatedDriftMs
				if driftMs > 100 || driftMs < -100 {
					// Significant drift, log a warning
					// In a real system, you might adjust local time operations
				}
			}
		}
	}
}

// OrderSpans orders spans based on causal relationships from vector clocks
func OrderSpans(spans []*proto.Span) []*proto.Span {
	if len(spans) <= 1 {
		return spans
	}

	// Create a directed graph representing "happens-before" relationships
	graph := make(map[string][]string)
	spanMap := make(map[string]*proto.Span)

	// Initialize the graph
	for _, span := range spans {
		spanMap[span.SpanId] = span
		graph[span.SpanId] = []string{}
	}

	// Build the graph edges based on vector clock relationships and parent-child
	for i, span1 := range spans {
		vc1 := VectorClockFromProto(span1.VectorClock)

		// Parent-child relationship
		if span1.ParentSpanId != "" {
			graph[span1.ParentSpanId] = append(graph[span1.ParentSpanId], span1.SpanId)
		}

		for j, span2 := range spans {
			if i == j {
				continue
			}

			vc2 := VectorClockFromProto(span2.VectorClock)
			if vc1.Compare(vc2) == -1 {
				// span1 happens before span2
				graph[span1.SpanId] = append(graph[span1.SpanId], span2.SpanId)
			}
		}
	}

	// Perform topological sort
	ordered := make([]*proto.Span, 0, len(spans))
	visited := make(map[string]bool)
	temp := make(map[string]bool)

	var visit func(spanID string) bool
	visit = func(spanID string) bool {
		if temp[spanID] {
			// Cycle detected, break it arbitrarily
			return false
		}
		if visited[spanID] {
			return true
		}

		temp[spanID] = true
		for _, neighbor := range graph[spanID] {
			visit(neighbor)
		}

		temp[spanID] = false
		visited[spanID] = true
		ordered = append(ordered, spanMap[spanID])
		return true
	}

	// Visit all nodes
	for spanID := range graph {
		if !visited[spanID] {
			visit(spanID)
		}
	}

	// Reverse the result to get the correct order
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}

	return ordered
}

// DetectConcurrentEvents identifies concurrent events in a trace
func DetectConcurrentEvents(spans []*proto.Span) [][]*proto.Span {
	// Group spans by their concurrent relationships
	concurrentGroups := make([][]*proto.Span, 0)

	// Build a map of spans by their ID for quick access
	spanMap := make(map[string]*proto.Span)
	for _, span := range spans {
		spanMap[span.SpanId] = span
	}

	// Create a map to track processed spans
	processed := make(map[string]bool)

	for _, span1 := range spans {
		if processed[span1.SpanId] {
			continue
		}

		// Create a new group starting with this span
		group := []*proto.Span{span1}
		processed[span1.SpanId] = true

		vc1 := VectorClockFromProto(span1.VectorClock)

		// Find all spans concurrent with this one
		for _, span2 := range spans {
			if span1.SpanId == span2.SpanId || processed[span2.SpanId] {
				continue
			}

			vc2 := VectorClockFromProto(span2.VectorClock)

			// If spans are concurrent, add to the group
			if vc1.Compare(vc2) == 0 {
				group = append(group, span2)
				processed[span2.SpanId] = true
			}
		}

		// Only add groups with more than one span (actual concurrency)
		if len(group) > 1 {
			concurrentGroups = append(concurrentGroups, group)
		}
	}

	return concurrentGroups
}

// TimestampConverter provides utilities for clock conversion
type TimestampConverter struct {
	baseTime      time.Time
	driftEstimate time.Duration
	mu            sync.RWMutex
}

// NewTimestampConverter creates a new converter
func NewTimestampConverter() *TimestampConverter {
	return &TimestampConverter{
		baseTime:      time.Now(),
		driftEstimate: 0,
	}
}

// UpdateDrift updates the estimated drift
func (tc *TimestampConverter) UpdateDrift(estimatedDriftMs int64) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.driftEstimate = time.Duration(estimatedDriftMs) * time.Millisecond
}

// AdjustTimestamp corrects a timestamp based on known drift
func (tc *TimestampConverter) AdjustTimestamp(timestamp time.Time) time.Time {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return timestamp.Add(-tc.driftEstimate)
}

// LogicalToPhysical converts logical time to physical time
func (tc *TimestampConverter) LogicalToPhysical(logicalTime uint64) time.Time {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.baseTime.Add(time.Duration(logicalTime) * time.Millisecond)
}

// PhysicalToLogical converts physical time to logical time
func (tc *TimestampConverter) PhysicalToLogical(physicalTime time.Time) uint64 {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	duration := physicalTime.Sub(tc.baseTime)
	return uint64(duration.Milliseconds())
}


// ToJSON serializes the vector clock to a JSON string for transmission in metadata
func (vc *VectorClock) ToJSON() (string, error) {
	if vc == nil {
		return "{}", nil
	}

	vc.mu.RLock()
	defer vc.mu.RUnlock()

	jsonBytes, err := json.Marshal(vc.Timestamps)
	if err != nil {
		return "", fmt.Errorf("failed to marshal vector clock: %v", err)
	}

	return string(jsonBytes), nil
}

// VectorClockFromJSON creates a VectorClock from a JSON string
func VectorClockFromJSON(jsonStr string) (*VectorClock, error) {
	timestamps := make(map[string]uint64)
	err := json.Unmarshal([]byte(jsonStr), &timestamps)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal vector clock: %v", err)
	}

	return &VectorClock{
		Timestamps: timestamps,
	}, nil
}