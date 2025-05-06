// pkg/analysis.go
package pkg

import (
	"fmt"
	"sort"
	"time"

	proto "github.com/sriharib128/distributed-tracer/proto"
)

// LatencyInfo contains latency analysis results
type LatencyInfo struct {
	ServiceName      string
	OperationName    string
	AvgLatencyMs     float64
	MinLatencyMs     int64
	MaxLatencyMs     int64
	P90LatencyMs     int64
	P95LatencyMs     int64
	P99LatencyMs     int64
	TotalInvocations int
	ErrorRate        float64
}

// AnalyzeLatency analyzes trace for latency information
func AnalyzeLatency(trace *proto.Trace) map[string]*LatencyInfo {
	if trace == nil || len(trace.Spans) == 0 {
		return nil
	}

	// Group spans by service+operation
	latencyByOp := make(map[string]*LatencyInfo)

	for _, span := range trace.Spans {
		if span.StartTime == nil || span.EndTime == nil {
			continue
		}

		key := fmt.Sprintf("%s:%s", span.ServiceName, span.OperationName)
		info, ok := latencyByOp[key]
		if !ok {
			info = &LatencyInfo{
				ServiceName:   span.ServiceName,
				OperationName: span.OperationName,
				MinLatencyMs:  9223372036854775807, // Max int64
			}
			latencyByOp[key] = info
		}

		latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()

		// Update stats
		info.TotalInvocations++
		info.AvgLatencyMs = ((info.AvgLatencyMs * float64(info.TotalInvocations-1)) + float64(latencyMs)) / float64(info.TotalInvocations)

		if latencyMs < info.MinLatencyMs {
			info.MinLatencyMs = latencyMs
		}

		if latencyMs > info.MaxLatencyMs {
			info.MaxLatencyMs = latencyMs
		}

		if span.Status == proto.Status_STATUS_ERROR {
			info.ErrorRate = ((info.ErrorRate * float64(info.TotalInvocations-1)) + 1.0) / float64(info.TotalInvocations)
		} else {
			info.ErrorRate = (info.ErrorRate * float64(info.TotalInvocations-1)) / float64(info.TotalInvocations)
		}
	}

	// Calculate percentiles
	for key, info := range latencyByOp {
		latencies := make([]int64, 0, info.TotalInvocations)

		// Collect all latencies for this operation
		for _, span := range trace.Spans {
			if span.ServiceName == info.ServiceName && span.OperationName == info.OperationName &&
				span.StartTime != nil && span.EndTime != nil {
				latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()
				latencies = append(latencies, latencyMs)
			}
		}

		// Sort latencies
		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		// Calculate percentiles
		info.P90LatencyMs = percentile(latencies, 90)
		info.P95LatencyMs = percentile(latencies, 95)
		info.P99LatencyMs = percentile(latencies, 99)

		latencyByOp[key] = info
	}

	return latencyByOp
}

// percentile calculates the specified percentile from sorted values
func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}

	idx := int(float64(p) / 100.0 * float64(len(values)))
	if idx >= len(values) {
		idx = len(values) - 1
	}

	return values[idx]
}

// Bottleneck represents a performance bottleneck
type Bottleneck struct {
	ServiceName    string
	OperationName  string
	LatencyMs      int64
	PercentOfTotal float64
	ErrorRate      float64
	Dependencies   []string
}

// DetectBottlenecks identifies performance bottlenecks
func DetectBottlenecks(trace *proto.Trace) []Bottleneck {
	if trace == nil || len(trace.Spans) == 0 {
		return nil
	}

	// First, order spans by hierarchy
	orderedSpans := OrderSpans(trace.Spans)

	// Calculate total trace time
	totalLatencyMs := int64(0)
	if trace.StartTime != nil && trace.EndTime != nil {
		totalLatencyMs = trace.EndTime.AsTime().Sub(trace.StartTime.AsTime()).Milliseconds()
	}

	// Build a map of child spans by parent
	childMap := make(map[string][]*proto.Span)
	for _, span := range orderedSpans {
		if span.ParentSpanId != "" {
			childMap[span.ParentSpanId] = append(childMap[span.ParentSpanId], span)
		}
	}

	// Calculate service latencies and identify bottlenecks
	bottlenecks := make([]Bottleneck, 0)

	for _, span := range orderedSpans {
		// Skip spans with no timing information
		if span.StartTime == nil || span.EndTime == nil {
			continue
		}

		spanLatencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()

		// Sum up child latencies
		childLatencyMs := int64(0)
		children := childMap[span.SpanId]
		childServices := make([]string, 0)

		for _, child := range children {
			if child.StartTime != nil && child.EndTime != nil {
				childLatencyMs += child.EndTime.AsTime().Sub(child.StartTime.AsTime()).Milliseconds()
			}

			childKey := fmt.Sprintf("%s:%s", child.ServiceName, child.OperationName)
			childServices = append(childServices, childKey)
		}

		// Own time = span time - child time
		ownLatencyMs := spanLatencyMs - childLatencyMs

		// If own time is significant, it might be a bottleneck
		if ownLatencyMs > 0 && float64(ownLatencyMs)/float64(totalLatencyMs) > 0.1 { // >10% of total time
			bottleneck := Bottleneck{
				ServiceName:    span.ServiceName,
				OperationName:  span.OperationName,
				LatencyMs:      ownLatencyMs,
				PercentOfTotal: float64(ownLatencyMs) / float64(totalLatencyMs) * 100.0,
				Dependencies:   childServices,
			}

			if span.Status == proto.Status_STATUS_ERROR {
				bottleneck.ErrorRate = 1.0
			}

			bottlenecks = append(bottlenecks, bottleneck)
		}
	}

	// Sort by latency impact, highest first
	sort.Slice(bottlenecks, func(i, j int) bool {
		return bottlenecks[i].LatencyMs > bottlenecks[j].LatencyMs
	})

	return bottlenecks
}

// ServiceDependency represents a dependency between services
type ServiceDependency struct {
	FromService   string
	ToService     string
	OperationName string
	CallCount     int
	ErrorCount    int
	AvgLatencyMs  float64
}

// ServiceGraph is a graph of service dependencies
type ServiceGraph struct {
	Services     []string
	Dependencies []ServiceDependency
}

// GenerateServiceDependencyGraph creates service dependency graph
func GenerateServiceDependencyGraph(traces []*proto.Trace) *ServiceGraph {
	if len(traces) == 0 {
		return nil
	}

	// Track unique services
	serviceSet := make(map[string]bool)

	// Track dependencies between services
	depMap := make(map[string]map[string]map[string]*ServiceDependency)

	for _, trace := range traces {
		// Process each span in the trace
		for _, span := range trace.Spans {
			// Add service to set of services
			serviceSet[span.ServiceName] = true

			// Check for inter-service dependencies
			if span.ParentSpanId != "" {
				// Find parent span to determine caller service
				var parentService string
				for _, parentSpan := range trace.Spans {
					if parentSpan.SpanId == span.ParentSpanId {
						parentService = parentSpan.ServiceName
						break
					}
				}

				// Only record if it's a cross-service call
				if parentService != "" && parentService != span.ServiceName {
					// Initialize maps if needed
					if _, ok := depMap[parentService]; !ok {
						depMap[parentService] = make(map[string]map[string]*ServiceDependency)
					}

					if _, ok := depMap[parentService][span.ServiceName]; !ok {
						depMap[parentService][span.ServiceName] = make(map[string]*ServiceDependency)
					}

					// Get or create dependency record
					dep, ok := depMap[parentService][span.ServiceName][span.OperationName]
					if !ok {
						dep = &ServiceDependency{
							FromService:   parentService,
							ToService:     span.ServiceName,
							OperationName: span.OperationName,
						}
						depMap[parentService][span.ServiceName][span.OperationName] = dep
					}

					// Update stats
					dep.CallCount++

					if span.Status == proto.Status_STATUS_ERROR {
						dep.ErrorCount++
					}

					if span.StartTime != nil && span.EndTime != nil {
						latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()
						dep.AvgLatencyMs = ((dep.AvgLatencyMs * float64(dep.CallCount-1)) + float64(latencyMs)) / float64(dep.CallCount)
					}
				}
			}
		}
	}

	// Build result
	graph := &ServiceGraph{
		Services:     make([]string, 0, len(serviceSet)),
		Dependencies: make([]ServiceDependency, 0),
	}

	for service := range serviceSet {
		graph.Services = append(graph.Services, service)
	}

	// Sort services for stable output
	sort.Strings(graph.Services)

	// Flatten dependencies
	for _, fromMap := range depMap {
		for _, toMap := range fromMap {
			for _, dep := range toMap {
				graph.Dependencies = append(graph.Dependencies, *dep)
			}
		}
	}

	return graph
}

// AnomalyType defines the type of anomaly detected
type AnomalyType string

const (
	// AnomalyHighLatency indicates unusually high latency
	AnomalyHighLatency AnomalyType = "high_latency"
	// AnomalyError indicates an error occurred
	AnomalyError AnomalyType = "error"
	// AnomalyTimeout indicates a timeout occurred
	AnomalyTimeout AnomalyType = "timeout"
	// AnomalyConcurrencyIssue indicates potential concurrency issue
	AnomalyConcurrencyIssue AnomalyType = "concurrency_issue"
	// AnomalyUnusualPattern indicates unusual calling pattern
	AnomalyUnusualPattern AnomalyType = "unusual_pattern"
)

// Anomaly represents a detected anomaly in traces
type Anomaly struct {
	Type          AnomalyType
	ServiceName   string
	OperationName string
	TraceID       string
	Description   string
	Severity      int // 1-10, 10 being most severe
	Timestamp     time.Time
}

// DetectAnomalies identifies anomalous traces
func DetectAnomalies(traces []*proto.Trace) []Anomaly {
	if len(traces) == 0 {
		return nil
	}

	anomalies := make([]Anomaly, 0)

	// First, calculate baseline metrics for each service/operation
	baselineLatencies := make(map[string]float64)
	baselineErrors := make(map[string]float64)

	// Count total operations by service/operation
	opCounts := make(map[string]int)

	for _, trace := range traces {
		for _, span := range trace.Spans {
			key := fmt.Sprintf("%s:%s", span.ServiceName, span.OperationName)

			// Only consider spans with proper timing
			if span.StartTime != nil && span.EndTime != nil {
				opCounts[key]++

				// Add to baseline latency
				latencyMs := float64(span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds())
				baselineLatencies[key] += latencyMs

				// Track errors
				if span.Status == proto.Status_STATUS_ERROR {
					baselineErrors[key]++
				}
			}
		}
	}

	// Calculate averages
	for key, total := range baselineLatencies {
		count := opCounts[key]
		if count > 0 {
			baselineLatencies[key] = total / float64(count)
		}
	}

	for key, errorCount := range baselineErrors {
		count := opCounts[key]
		if count > 0 {
			baselineErrors[key] = errorCount / float64(count)
		}
	}

	// Now detect anomalies
	for _, trace := range traces {
		// Check for concurrent events which might indicate issues
		concurrentEvents := DetectConcurrentEvents(trace.Spans)
		if len(concurrentEvents) > 0 {
			// Potential concurrency issue
			for _, group := range concurrentEvents {
				if len(group) >= 3 { // Significant concurrency
					anomalies = append(anomalies, Anomaly{
						Type:        AnomalyConcurrencyIssue,
						ServiceName: group[0].ServiceName,
						TraceID:     trace.TraceId,
						Description: fmt.Sprintf("Potential concurrency issue with %d concurrent operations", len(group)),
						Severity:    7,
						Timestamp:   group[0].StartTime.AsTime(),
					})
				}
			}
		}

		// Check each span for anomalies
		for _, span := range trace.Spans {
			key := fmt.Sprintf("%s:%s", span.ServiceName, span.OperationName)

			// Skip spans without timing info
			if span.StartTime == nil || span.EndTime == nil {
				continue
			}

			latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()
			baselineLatency := baselineLatencies[key]

			// Detect high latency (3x baseline or more)
			if baselineLatency > 0 && float64(latencyMs) > baselineLatency*3 {
				severity := 5
				if float64(latencyMs) > baselineLatency*10 {
					severity = 9 // Very severe latency issue
				}

				anomalies = append(anomalies, Anomaly{
					Type:          AnomalyHighLatency,
					ServiceName:   span.ServiceName,
					OperationName: span.OperationName,
					TraceID:       trace.TraceId,
					Description:   fmt.Sprintf("High latency: %dms (%.1fx baseline)", latencyMs, float64(latencyMs)/baselineLatency),
					Severity:      severity,
					Timestamp:     span.StartTime.AsTime(),
				})
			}

			// Detect errors
			if span.Status == proto.Status_STATUS_ERROR {
				errorRate := baselineErrors[key]

				if errorRate < 0.1 { // Errors are rare for this operation
					anomalies = append(anomalies, Anomaly{
						Type:          AnomalyError,
						ServiceName:   span.ServiceName,
						OperationName: span.OperationName,
						TraceID:       trace.TraceId,
						Description:   fmt.Sprintf("Error in operation with %.1f%% baseline error rate", errorRate*100),
						Severity:      8,
						Timestamp:     span.StartTime.AsTime(),
					})
				}
			}

			// Detect timeouts
			if span.Status == proto.Status_STATUS_TIMEOUT {
				anomalies = append(anomalies, Anomaly{
					Type:          AnomalyTimeout,
					ServiceName:   span.ServiceName,
					OperationName: span.OperationName,
					TraceID:       trace.TraceId,
					Description:   "Operation timed out",
					Severity:      7,
					Timestamp:     span.StartTime.AsTime(),
				})
			}
		}
	}

	// Sort anomalies by severity (highest first)
	sort.Slice(anomalies, func(i, j int) bool {
		return anomalies[i].Severity > anomalies[j].Severity
	})

	return anomalies
}

// ServiceSLA represents SLA metrics for a service
type ServiceSLA struct {
	ServiceName      string
	OperationName    string
	AvgLatencyMs     float64
	P99LatencyMs     int64
	ErrorRate        float64
	TimeoutRate      float64
	SuccessRate      float64
	TotalInvocations int
	Period           time.Duration
}

// CalculateServiceSLAs calculates service performance metrics
func CalculateServiceSLAs(traces []*proto.Trace) map[string]*ServiceSLA {
	if len(traces) == 0 {
		return nil
	}

	// Group metrics by service:operation
	slaMap := make(map[string]*ServiceSLA)

	for _, trace := range traces {
		for _, span := range trace.Spans {
			key := fmt.Sprintf("%s:%s", span.ServiceName, span.OperationName)

			sla, ok := slaMap[key]
			if !ok {
				sla = &ServiceSLA{
					ServiceName:   span.ServiceName,
					OperationName: span.OperationName,
					Period:        24 * time.Hour, // Default to daily SLAs
				}
				slaMap[key] = sla
			}

			// Skip spans without timing info
			if span.StartTime == nil || span.EndTime == nil {
				continue
			}

			// Update invocation count
			sla.TotalInvocations++

			// Update latency
			latencyMs := float64(span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds())
			sla.AvgLatencyMs = ((sla.AvgLatencyMs * float64(sla.TotalInvocations-1)) + latencyMs) / float64(sla.TotalInvocations)

			// Update status counters
			switch span.Status {
			case proto.Status_STATUS_OK:
				sla.SuccessRate = ((sla.SuccessRate * float64(sla.TotalInvocations-1)) + 1) / float64(sla.TotalInvocations)
			case proto.Status_STATUS_ERROR:
				sla.ErrorRate = ((sla.ErrorRate * float64(sla.TotalInvocations-1)) + 1) / float64(sla.TotalInvocations)
			case proto.Status_STATUS_TIMEOUT:
				sla.TimeoutRate = ((sla.TimeoutRate * float64(sla.TotalInvocations-1)) + 1) / float64(sla.TotalInvocations)
			}
		}
	}

	// Calculate p99 latencies
	for key, sla := range slaMap {
		latencies := make([]int64, 0, sla.TotalInvocations)

		for _, trace := range traces {
			for _, span := range trace.Spans {
				if span.ServiceName == sla.ServiceName && span.OperationName == sla.OperationName &&
					span.StartTime != nil && span.EndTime != nil {
					latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()
					latencies = append(latencies, latencyMs)
				}
			}
		}

		if len(latencies) > 0 {
			// Sort latencies
			sort.Slice(latencies, func(i, j int) bool {
				return latencies[i] < latencies[j]
			})

			sla.P99LatencyMs = percentile(latencies, 99)
		}

		slaMap[key] = sla
	}

	return slaMap
}

// PathNode represents a node in the critical path
type PathNode struct {
	Span         *proto.Span
	LatencyMs    int64
	Children     []*PathNode
	CriticalPath bool
}

// IdentifyCriticalPath finds the critical path through a trace
func IdentifyCriticalPath(trace *proto.Trace) []*proto.Span {
	if trace == nil || len(trace.Spans) == 0 {
		return nil
	}

	// Build span hierarchy
	spanMap := make(map[string]*proto.Span)
	childMap := make(map[string][]*proto.Span)

	var rootSpan *proto.Span

	for _, span := range trace.Spans {
		spanMap[span.SpanId] = span

		if span.ParentSpanId == "" {
			rootSpan = span
		} else {
			childMap[span.ParentSpanId] = append(childMap[span.ParentSpanId], span)
		}
	}

	// If we couldn't identify a root, use the first span
	if rootSpan == nil && len(trace.Spans) > 0 {
		rootSpan = trace.Spans[0]
	}

	// Build tree with latency information
	var buildTree func(span *proto.Span) *PathNode
	buildTree = func(span *proto.Span) *PathNode {
		if span == nil || span.StartTime == nil || span.EndTime == nil {
			return nil
		}

		latencyMs := span.EndTime.AsTime().Sub(span.StartTime.AsTime()).Milliseconds()

		node := &PathNode{
			Span:      span,
			LatencyMs: latencyMs,
			Children:  make([]*PathNode, 0),
		}

		children := childMap[span.SpanId]
		for _, childSpan := range children {
			if childNode := buildTree(childSpan); childNode != nil {
				node.Children = append(node.Children, childNode)
			}
		}

		return node
	}

	rootNode := buildTree(rootSpan)
	if rootNode == nil {
		return nil
	}

	// Find critical path - the path with the highest latency
	var findCriticalPath func(node *PathNode) int64
	findCriticalPath = func(node *PathNode) int64 {
		if node == nil {
			return 0
		}

		maxChildLatency := int64(0)
		var criticalChild *PathNode

		for _, child := range node.Children {
			childLatency := findCriticalPath(child)
			if childLatency > maxChildLatency {
				maxChildLatency = childLatency
				criticalChild = child
			}
		}

		// Mark the critical child
		if criticalChild != nil {
			criticalChild.CriticalPath = true
		}

		node.LatencyMs += maxChildLatency
		return node.LatencyMs
	}

	rootNode.CriticalPath = true
	findCriticalPath(rootNode)

	// Extract critical path
	var criticalSpans []*proto.Span
	var extractCriticalPath func(node *PathNode)
	extractCriticalPath = func(node *PathNode) {
		if node == nil || !node.CriticalPath {
			return
		}

		criticalSpans = append(criticalSpans, node.Span)

		for _, child := range node.Children {
			if child.CriticalPath {
				extractCriticalPath(child)
				break
			}
		}
	}

	extractCriticalPath(rootNode)
	return criticalSpans
}

// PerformanceReport contains comprehensive performance analysis
type PerformanceReport struct {
	ServiceLatencies  map[string]*LatencyInfo
	Bottlenecks       []Bottleneck
	ServiceGraph      *ServiceGraph
	Anomalies         []Anomaly
	ServiceSLAs       map[string]*ServiceSLA
	TraceCount        int
	ErrorTraceCount   int
	SlowTraceCount    int
	AvgTraceLatencyMs float64
	P95TraceLatencyMs int64
	Period            time.Duration
}

// GeneratePerformanceReport creates comprehensive performance analysis
func GeneratePerformanceReport(traces []*proto.Trace) *PerformanceReport {
	if len(traces) == 0 {
		return &PerformanceReport{
			TraceCount: 0,
			Period:     time.Hour * 24,
		}
	}

	report := &PerformanceReport{
		ServiceLatencies: make(map[string]*LatencyInfo),
		TraceCount:       len(traces),
		Period:           time.Hour * 24,
	}

	// Analyze individual traces
	traceLatencies := make([]int64, 0, len(traces))
	totalLatencyMs := int64(0)

	for _, trace := range traces {
		// Skip traces without timing info
		if trace.StartTime == nil || trace.EndTime == nil {
			continue
		}

		// Calculate trace latency
		traceLatencyMs := trace.EndTime.AsTime().Sub(trace.StartTime.AsTime()).Milliseconds()
		traceLatencies = append(traceLatencies, traceLatencyMs)
		totalLatencyMs += traceLatencyMs

		// Check if trace has errors
		hasError := false
		for _, span := range trace.Spans {
			if span.Status == proto.Status_STATUS_ERROR || span.Status == proto.Status_STATUS_TIMEOUT {
				hasError = true
				break
			}
		}

		if hasError {
			report.ErrorTraceCount++
		}

		// Get service latencies
		latencyInfo := AnalyzeLatency(trace)
		for key, info := range latencyInfo {
			if existing, ok := report.ServiceLatencies[key]; ok {
				// Merge latency info
				existing.TotalInvocations += info.TotalInvocations
				existing.AvgLatencyMs = ((existing.AvgLatencyMs * float64(existing.TotalInvocations-info.TotalInvocations)) +
					(info.AvgLatencyMs * float64(info.TotalInvocations))) /
					float64(existing.TotalInvocations)

				if info.MinLatencyMs < existing.MinLatencyMs {
					existing.MinLatencyMs = info.MinLatencyMs
				}

				if info.MaxLatencyMs > existing.MaxLatencyMs {
					existing.MaxLatencyMs = info.MaxLatencyMs
				}

				// For simplicity, just take the highest percentile values
				if info.P90LatencyMs > existing.P90LatencyMs {
					existing.P90LatencyMs = info.P90LatencyMs
				}

				if info.P95LatencyMs > existing.P95LatencyMs {
					existing.P95LatencyMs = info.P95LatencyMs
				}

				if info.P99LatencyMs > existing.P99LatencyMs {
					existing.P99LatencyMs = info.P99LatencyMs
				}

				// Weighted average for error rate
				existing.ErrorRate = ((existing.ErrorRate * float64(existing.TotalInvocations-info.TotalInvocations)) +
					(info.ErrorRate * float64(info.TotalInvocations))) /
					float64(existing.TotalInvocations)
			} else {
				// Copy the latency info
				report.ServiceLatencies[key] = info
			}
		}

		// Identify bottlenecks (just collect the most severe from each trace)
		bottlenecks := DetectBottlenecks(trace)
		if len(bottlenecks) > 0 {
			report.Bottlenecks = append(report.Bottlenecks, bottlenecks[0])
		}
	}

	// Calculate aggregate trace latency stats
	if len(traceLatencies) > 0 {
		report.AvgTraceLatencyMs = float64(totalLatencyMs) / float64(len(traceLatencies))

		// Sort for percentile calculation
		sort.Slice(traceLatencies, func(i, j int) bool {
			return traceLatencies[i] < traceLatencies[j]
		})

		report.P95TraceLatencyMs = percentile(traceLatencies, 95)

		// Count slow traces (>2x average)
		for _, latency := range traceLatencies {
			if float64(latency) > report.AvgTraceLatencyMs*2 {
				report.SlowTraceCount++
			}
		}
	}

	// Generate other analyses
	report.ServiceGraph = GenerateServiceDependencyGraph(traces)
	report.Anomalies = DetectAnomalies(traces)
	report.ServiceSLAs = CalculateServiceSLAs(traces)

	return report
}
