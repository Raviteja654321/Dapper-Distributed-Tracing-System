// services/service-c.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
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
)

// ServiceC represents the service C implementation
type ServiceC struct {
	proto.UnimplementedServiceCServer
	tracer     *pkg.Tracer
	dataStore  map[string]string
	storeMutex sync.RWMutex
}

// Service C: Data Service/Backend
func main() {
	// Parse command-line flags
	port := flag.Int("port", 8002, "gRPC port")
	collectorAddr := flag.String("collector", "localhost:7777", "Collector address")
	flag.Parse()

	// Seed random
	rand.Seed(time.Now().UnixNano())

	// Setup tracer
	tracer, err := setupTracer("service-c", *collectorAddr)
	if err != nil {
		log.Fatalf("Failed to setup tracer: %v", err)
	}
	defer tracer.Close()

	// Create service
	service := &ServiceC{
		tracer:    tracer,
		dataStore: make(map[string]string),
	}

	// Setup gRPC server
	server := grpc.NewServer(
		grpc.UnaryInterceptor(pkg.UnaryServerInterceptor(tracer)),
		grpc.StreamInterceptor(pkg.StreamServerInterceptor(tracer)),
	)

	// Register service
	proto.RegisterServiceCServer(server, service)

	// Enable reflection for debugging
	reflection.Register(server)

	// Start listening
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}

	go func() {
		log.Printf("Service C gRPC server listening on port %d", *port)
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	server.GracefulStop()
	log.Println("Service C shutdown complete")
}

// setupTracer configures the tracer
func setupTracer(serviceName string, collectorAddr string) (*pkg.Tracer, error) {
	config := pkg.TracerConfig{
		ServiceName:      serviceName,
		CollectorAddress: collectorAddr,
		SamplingRate:     1.0, // Sample everything for demo
		BufferSize:       1000,
		LogDirectory:     "./logs",
		FlushInterval:    5 * time.Second,
		MaxBatchSize:     100,
		DebugMode:        true,
	}

	return pkg.NewTracer(config)
}

// GRPC Service implementation

// QueryDatabase implements the QueryDatabase method for ServiceC
func (s *ServiceC) QueryDatabase(ctx context.Context, req *proto.QueryDatabaseRequest) (*proto.QueryDatabaseResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.query_database")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}
	fmt.Printf("line 117 in Service C")
	query := req.Query
	queryType := req.Type
	span.AddAnnotation("db.query", query)
	span.AddAnnotation("db.type", queryType)

	// Create child span for database operation
	dbSpan := s.tracer.StartSpan("db.operation", pkg.WithParentSpan(span))
	defer dbSpan.Finish()

	// Simulate database access
	time.Sleep(time.Duration(20+rand.Intn(30)) * time.Millisecond)

	// Simulate different operations
	var results int
	s.storeMutex.RLock()
	switch queryType {
	case "search":
		// Count matches in datastore
		for key := range s.dataStore {
			if strings.Contains(key, query) {
				results++
			}
		}
		// Add some random results
		results += rand.Intn(10)
	case "count":
		results = len(s.dataStore)
	default:
		results = rand.Intn(100)
	}
	s.storeMutex.RUnlock()

	dbSpan.AddAnnotation("db.results", results)

	// Simulate error for certain queries
	if strings.Contains(query, "error") {
		span.SetStatus(proto.Status_STATUS_ERROR)
		dbSpan.SetStatus(proto.Status_STATUS_ERROR)
		dbSpan.AddAnnotation("error", "database_error")
		return nil, fmt.Errorf("database error for query: %s", query)
	}

	// Simulate timeout for certain queries
	if strings.Contains(query, "slow") {
		time.Sleep(time.Duration(100+rand.Intn(100)) * time.Millisecond)
	}

	return &proto.QueryDatabaseResponse{
		Results: int32(results),
	}, nil
}

// StoreData implements the StoreData method for ServiceC
func (s *ServiceC) StoreData(ctx context.Context, req *proto.StoreDataRequest) (*proto.StoreDataResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.store_data")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	data := req.Data
	dataLen := len(data)
	span.AddAnnotation("store.data_size", dataLen)

	// Create child span for storage operation
	storeSpan := s.tracer.StartSpan("db.store", pkg.WithParentSpan(span))

	// Generate a key
	key := fmt.Sprintf("data_%d_%s", time.Now().UnixNano(), data[:min(10, len(data))])
	storeSpan.AddAnnotation("db.key", key)

	// Simulate storage operation
	time.Sleep(time.Duration(10+rand.Intn(40)) * time.Millisecond)

	// Perform storage
	s.storeMutex.Lock()
	s.dataStore[key] = data
	s.storeMutex.Unlock()

	// Simulate concurrent operations
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			indexSpan := s.tracer.StartSpan(fmt.Sprintf("db.index.%d", idx), pkg.WithParentSpan(storeSpan))
			time.Sleep(time.Duration(5+rand.Intn(15)) * time.Millisecond)
			indexSpan.Finish()
		}(i)
	}

	// Wait for concurrent operations
	wg.Wait()
	storeSpan.Finish()

	// Simulate error
	if strings.Contains(data, "error") {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", "storage_error")
		return nil, fmt.Errorf("storage error for data: %s", data)
	}

	return &proto.StoreDataResponse{
		Status: "success",
	}, nil
}

// Greet implements the Greet method for ServiceC
func (s *ServiceC) Greet(ctx context.Context, req *proto.GreetRequest) (*proto.GreetResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.greet")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	name := req.Name
	span.AddAnnotation("greet.name", name)

	// Simulate some work
	time.Sleep(time.Duration(5+rand.Intn(10)) * time.Millisecond)

	// Simulate vector clock synchronization issues
	if rand.Intn(10) == 0 {
		// Create a few concurrent spans to demonstrate vector clock
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				concSpan := s.tracer.StartSpan(fmt.Sprintf("concurrent.op.%d", idx), pkg.WithParentSpan(span))
				time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
				concSpan.Finish()
			}(i)
		}
		wg.Wait()
	}

	greeting := fmt.Sprintf("Hello, %s! Greetings from Service C.", name)
	return &proto.GreetResponse{
		Greeting: greeting,
	}, nil
}

// Helper function to get the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
