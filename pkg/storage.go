// pkg/storage.go
package pkg

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	proto "github.com/sriharib128/distributed-tracer/proto"
)

// StorageInterface defines the interface for storing traces
type StorageInterface interface {
	// StoreTrace stores a complete trace
	StoreTrace(trace *proto.Trace) error
	// GetTrace retrieves a trace by ID
	GetTrace(traceID string) (*proto.Trace, error)
	// ListTraces lists traces matching criteria
	ListTraces(filter *proto.FilterCriteria) ([]*proto.Trace, error)
	// PruneTraces removes traces older than the specified time
	PruneTraces(olderThan time.Time) error
}

// MemoryStorage is an in-memory implementation for development/testing
type MemoryStorage struct {
	traces map[string]*proto.Trace
	mu     sync.RWMutex
}

// NewMemoryStorage creates a new in-memory storage
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		traces: make(map[string]*proto.Trace),
	}
}

// StoreTrace implements StorageInterface.StoreTrace
func (s *MemoryStorage) StoreTrace(trace *proto.Trace) error {
	if trace == nil {
		return fmt.Errorf("trace is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.traces[trace.TraceId] = trace
	return nil
}

// GetTrace implements StorageInterface.GetTrace
func (s *MemoryStorage) GetTrace(traceID string) (*proto.Trace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	trace, ok := s.traces[traceID]
	if !ok {
		return nil, fmt.Errorf("trace not found: %s", traceID)
	}

	// Create a deep copy to prevent concurrent modification
	traceCopy := &proto.Trace{
		TraceId:       trace.TraceId,
		RootService:   trace.RootService,
		RootOperation: trace.RootOperation,
		StartTime:     trace.StartTime,
		EndTime:       trace.EndTime,
		Spans:         make([]*proto.Span, len(trace.Spans)),
	}

	copy(traceCopy.Spans, trace.Spans)
	return traceCopy, nil
}

// ListTraces implements StorageInterface.ListTraces
func (s *MemoryStorage) ListTraces(filter *proto.FilterCriteria) ([]*proto.Trace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*proto.Trace, 0, len(s.traces))

	for _, trace := range s.traces {
		if matchesFilter(trace, filter) {
			// Create a deep copy
			traceCopy := &proto.Trace{
				TraceId:       trace.TraceId,
				RootService:   trace.RootService,
				RootOperation: trace.RootOperation,
				StartTime:     trace.StartTime,
				EndTime:       trace.EndTime,
				Spans:         make([]*proto.Span, len(trace.Spans)),
			}

			copy(traceCopy.Spans, trace.Spans)
			result = append(result, traceCopy)
		}
	}

	// Apply sorting - newest traces first by default
	sort.Slice(result, func(i, j int) bool {
		// If start times aren't available, sort by ID for stability
		if result[i].StartTime == nil || result[j].StartTime == nil {
			return result[i].TraceId > result[j].TraceId
		}
		return result[i].StartTime.AsTime().After(result[j].StartTime.AsTime())
	})

	// Apply limit and offset
	if filter != nil {
		offset := int(filter.Offset)
		if offset >= len(result) {
			return []*proto.Trace{}, nil
		}

		limit := int(filter.Limit)
		if limit <= 0 {
			limit = len(result)
		}

		end := offset + limit
		if end > len(result) {
			end = len(result)
		}

		result = result[offset:end]
	}

	return result, nil
}

// PruneTraces implements StorageInterface.PruneTraces
func (s *MemoryStorage) PruneTraces(olderThan time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, trace := range s.traces {
		if trace.EndTime != nil && trace.EndTime.AsTime().Before(olderThan) {
			delete(s.traces, id)
		}
	}

	return nil
}

// FileStorage is a file-based persistent storage implementation
type FileStorage struct {
	basePath     string
	indexMutex   sync.RWMutex
	traceMutexes map[string]*sync.RWMutex
	mutexLock    sync.RWMutex
}

// NewFileStorage creates a new file-based storage
func NewFileStorage(basePath string) (*FileStorage, error) {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %v", err)
	}

	// Create necessary subdirectories
	tracesDir := filepath.Join(basePath, "traces")
	if err := os.MkdirAll(tracesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create traces directory: %v", err)
	}

	indexDir := filepath.Join(basePath, "index")
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create index directory: %v", err)
	}

	return &FileStorage{
		basePath:     basePath,
		traceMutexes: make(map[string]*sync.RWMutex),
	}, nil
}

// getTraceMutex gets or creates a mutex for a trace ID
func (s *FileStorage) getTraceMutex(traceID string) *sync.RWMutex {
	s.mutexLock.Lock()
	defer s.mutexLock.Unlock()

	if mutex, ok := s.traceMutexes[traceID]; ok {
		return mutex
	}

	mutex := &sync.RWMutex{}
	s.traceMutexes[traceID] = mutex
	return mutex
}

// StoreTrace implements StorageInterface.StoreTrace
func (s *FileStorage) StoreTrace(trace *proto.Trace) error {
	if trace == nil || trace.TraceId == "" {
		return fmt.Errorf("invalid trace")
	}

	// Get mutex for this trace
	mutex := s.getTraceMutex(trace.TraceId)
	mutex.Lock()
	defer mutex.Unlock()

	// Prepare trace path
	tracePath := filepath.Join(s.basePath, "traces", trace.TraceId+".json")

	// Marshal to JSON
	data, err := json.Marshal(trace)
	if err != nil {
		return fmt.Errorf("failed to marshal trace: %v", err)
	}

	// Write to temporary file
	tempPath := tracePath + ".tmp"
	if err := ioutil.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write trace file: %v", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, tracePath); err != nil {
		return fmt.Errorf("failed to finalize trace file: %v", err)
	}

	// Update index
	return s.updateTraceIndex(trace)
}

// GetTrace implements StorageInterface.GetTrace
func (s *FileStorage) GetTrace(traceID string) (*proto.Trace, error) {
	if traceID == "" {
		return nil, fmt.Errorf("traceID is empty")
	}

	// Get mutex for this trace
	mutex := s.getTraceMutex(traceID)
	mutex.RLock()
	defer mutex.RUnlock()

	tracePath := filepath.Join(s.basePath, "traces", traceID+".json")

	// Read the file
	data, err := ioutil.ReadFile(tracePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("trace not found: %s", traceID)
		}
		return nil, fmt.Errorf("failed to read trace file: %v", err)
	}

	// Unmarshal
	var trace proto.Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		return nil, fmt.Errorf("failed to unmarshal trace: %v", err)
	}

	return &trace, nil
}

// TraceIndex represents the index of all traces
type TraceIndex struct {
	Traces []TraceIndexEntry `json:"traces"`
}

// TraceIndexEntry is a single entry in the trace index
type TraceIndexEntry struct {
	TraceID       string    `json:"trace_id"`
	RootService   string    `json:"root_service"`
	RootOperation string    `json:"root_operation"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	Status        int       `json:"status"`
}

// updateTraceIndex updates the trace index
func (s *FileStorage) updateTraceIndex(trace *proto.Trace) error {
	s.indexMutex.Lock()
	defer s.indexMutex.Unlock()

	indexPath := filepath.Join(s.basePath, "index", "traces.json")

	// Read existing index
	var index TraceIndex
	data, err := ioutil.ReadFile(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read index: %v", err)
	}

	if err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("failed to unmarshal index: %v", err)
		}
	}

	// Find and update or add entry
	entry := TraceIndexEntry{
		TraceID:       trace.TraceId,
		RootService:   trace.RootService,
		RootOperation: trace.RootOperation,
	}

	if trace.StartTime != nil {
		entry.StartTime = trace.StartTime.AsTime()
	}

	if trace.EndTime != nil {
		entry.EndTime = trace.EndTime.AsTime()
	}

	// Determine status - use highest status from all spans
	status := int(proto.Status_STATUS_OK)
	for _, span := range trace.Spans {
		if int(span.Status) > status {
			status = int(span.Status)
		}
	}
	entry.Status = status

	// Update or add entry
	found := false
	for i, e := range index.Traces {
		if e.TraceID == trace.TraceId {
			index.Traces[i] = entry
			found = true
			break
		}
	}

	if !found {
		index.Traces = append(index.Traces, entry)
	}

	// Write back to file
	data, err = json.Marshal(index)
	if err != nil {
		return fmt.Errorf("failed to marshal index: %v", err)
	}

	tempPath := indexPath + ".tmp"
	if err := ioutil.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write index file: %v", err)
	}

	return os.Rename(tempPath, indexPath)
}

// ListTraces implements StorageInterface.ListTraces
func (s *FileStorage) ListTraces(filter *proto.FilterCriteria) ([]*proto.Trace, error) {
	s.indexMutex.RLock()

	// Read the index
	indexPath := filepath.Join(s.basePath, "index", "traces.json")
	data, err := ioutil.ReadFile(indexPath)

	s.indexMutex.RUnlock()

	if err != nil {
		if os.IsNotExist(err) {
			return []*proto.Trace{}, nil
		}
		return nil, fmt.Errorf("failed to read index: %v", err)
	}

	var index TraceIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to unmarshal index: %v", err)
	}

	// Filter and sort the index
	filteredEntries := make([]TraceIndexEntry, 0)
	for _, entry := range index.Traces {
		if matchesFilterEntry(entry, filter) {
			filteredEntries = append(filteredEntries, entry)
		}
	}

	// Sort by start time (newest first)
	sort.Slice(filteredEntries, func(i, j int) bool {
		return filteredEntries[i].StartTime.After(filteredEntries[j].StartTime)
	})

	// Apply offset and limit
	offset := 0
	limit := 100

	if filter != nil {
		offset = int(filter.Offset)
		if filter.Limit > 0 {
			limit = int(filter.Limit)
		}
	}

	if offset >= len(filteredEntries) {
		return []*proto.Trace{}, nil
	}

	end := offset + limit
	if end > len(filteredEntries) {
		end = len(filteredEntries)
	}

	entries := filteredEntries[offset:end]

	// Load full traces
	result := make([]*proto.Trace, 0, len(entries))
	for _, entry := range entries {
		trace, err := s.GetTrace(entry.TraceID)
		if err != nil {
			// Skip traces we can't load
			continue
		}
		result = append(result, trace)
	}

	return result, nil
}

// PruneTraces implements StorageInterface.PruneTraces
func (s *FileStorage) PruneTraces(olderThan time.Time) error {
	s.indexMutex.RLock()

	// Read the index
	indexPath := filepath.Join(s.basePath, "index", "traces.json")
	data, err := ioutil.ReadFile(indexPath)

	s.indexMutex.RUnlock()

	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read index: %v", err)
	}

	var index TraceIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("failed to unmarshal index: %v", err)
	}

	// Filter out old traces
	var newEntries []TraceIndexEntry
	var toDelete []string

	for _, entry := range index.Traces {
		if !entry.EndTime.IsZero() && entry.EndTime.Before(olderThan) {
			toDelete = append(toDelete, entry.TraceID)
		} else {
			newEntries = append(newEntries, entry)
		}
	}

	// Delete the trace files
	for _, traceID := range toDelete {
		tracePath := filepath.Join(s.basePath, "traces", traceID+".json")
		os.Remove(tracePath) // Ignore errors, best effort

		// Clean up mutex
		s.mutexLock.Lock()
		delete(s.traceMutexes, traceID)
		s.mutexLock.Unlock()
	}

	// Update the index
	if len(toDelete) > 0 {
		s.indexMutex.Lock()
		defer s.indexMutex.Unlock()

		index.Traces = newEntries

		data, err = json.Marshal(index)
		if err != nil {
			return fmt.Errorf("failed to marshal index: %v", err)
		}

		tempPath := indexPath + ".tmp"
		if err := ioutil.WriteFile(tempPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write index file: %v", err)
		}

		return os.Rename(tempPath, indexPath)
	}

	return nil
}

// matchesFilter checks if a trace matches filter criteria
func matchesFilter(trace *proto.Trace, filter *proto.FilterCriteria) bool {
	if filter == nil {
		return true
	}

	// Check start time
	if filter.StartTime != nil && trace.StartTime != nil {
		if trace.StartTime.AsTime().Before(filter.StartTime.AsTime()) {
			return false
		}
	}

	// Check end time
	if filter.EndTime != nil && trace.EndTime != nil {
		if trace.EndTime.AsTime().After(filter.EndTime.AsTime()) {
			return false
		}
	}

	// Check services
	if len(filter.Services) > 0 {
		// Check if any span has one of the services
		found := false
		for _, span := range trace.Spans {
			for _, service := range filter.Services {
				if span.ServiceName == service {
					found = true
					break
				}
			}
			if found {
				break
			}
		}

		if !found {
			return false
		}
	}

	// Check status
	if filter.Status != proto.Status_STATUS_UNSPECIFIED {
		// Check if any span has the status
		found := false
		for _, span := range trace.Spans {
			if span.Status == filter.Status {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	// Check operation name
	if filter.OperationName != "" {
		// Check if any span has the operation
		found := false
		for _, span := range trace.Spans {
			if span.OperationName == filter.OperationName {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// matchesFilterEntry checks if a trace index entry matches filter criteria
func matchesFilterEntry(entry TraceIndexEntry, filter *proto.FilterCriteria) bool {
	if filter == nil {
		return true
	}

	// Check start time
	if filter.StartTime != nil && !entry.StartTime.IsZero() {
		if entry.StartTime.Before(filter.StartTime.AsTime()) {
			return false
		}
	}

	// Check end time
	if filter.EndTime != nil && !entry.EndTime.IsZero() {
		if entry.EndTime.After(filter.EndTime.AsTime()) {
			return false
		}
	}

	// Check service (only root service in index)
	if len(filter.Services) > 0 {
		found := false
		for _, service := range filter.Services {
			if entry.RootService == service {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	// Check status
	if filter.Status != proto.Status_STATUS_UNSPECIFIED {
		if int(filter.Status) != entry.Status {
			return false
		}
	}

	// Check operation name (only root operation in index)
	if filter.OperationName != "" && entry.RootOperation != filter.OperationName {
		return false
	}

	return true
}
