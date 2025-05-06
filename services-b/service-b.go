// services/service-b.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sriharib128/distributed-tracer/pkg"
	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// ServiceB represents the service B implementation
type ServiceB struct {
	proto.UnimplementedServiceBServer
	tracer           *pkg.Tracer
	serviceCClient   proto.ServiceCClient
	simulatedFailure bool
}

// Service B: Middleware/Business Logic Service
func main() {
	// Parse command-line flags
	port := flag.Int("port", 8001, "gRPC port")
	grpcServiceCAddr := flag.String("service-c", "localhost:8002", "Service C gRPC address")
	collectorAddr := flag.String("collector", "localhost:7777", "Collector address")
	flag.Parse()

	// Setup tracer
	tracer, err := setupTracer("service-b", *collectorAddr)
	if err != nil {
		log.Fatalf("Failed to setup tracer: %v", err)
	}
	defer tracer.Close()

	// Connect to service C
	conn, err := grpc.Dial(
		*grpcServiceCAddr,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(pkg.UnaryClientInterceptor(tracer)),
		grpc.WithStreamInterceptor(pkg.StreamClientInterceptor(tracer)),
	)
	if err != nil {
		log.Fatalf("Failed to connect to service C: %v", err)
	}
	defer conn.Close()

	serviceCClient := proto.NewServiceCClient(conn)

	// Create service
	service := &ServiceB{
		tracer:         tracer,
		serviceCClient: serviceCClient,
	}

	// Setup gRPC server
	server := grpc.NewServer(
		grpc.UnaryInterceptor(pkg.UnaryServerInterceptor(tracer)),
		grpc.StreamInterceptor(pkg.StreamServerInterceptor(tracer)),
	)

	// Register service
	proto.RegisterServiceBServer(server, service)

	// Enable reflection for debugging
	reflection.Register(server)

	// Start listening
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}

	go func() {
		log.Printf("Service B gRPC server listening on port %d", *port)
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
	log.Println("Service B shutdown complete")
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

// Search implements the Search method for ServiceB
func (s *ServiceB) Search(ctx context.Context, req *proto.SearchRequest) (*proto.SearchResponse, error) {

	// Get span from context
	fmt.Println("line 122")
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.search")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}
	log.Println("line 127")
	query := req.Query
	span.AddAnnotation("search.query", query)
	log.Println("line 130")
	// Simulate some work
	time.Sleep(50 * time.Millisecond)
	log.Println(span)
	// Call service C for database operations
	resp, err := s.serviceCClient.QueryDatabase(ctx, &proto.QueryDatabaseRequest{
		Query: query,
		Type:  "search",
	})

	if err != nil {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return nil, err
	}

	// Simulate error for certain queries
	if strings.Contains(query, "error") {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", "search_error")
		return nil, fmt.Errorf("search error for query: %s", query)
	}

	// Simulate timeout for certain queries
	if strings.Contains(query, "slow") {
		time.Sleep(200 * time.Millisecond)
	}

	// Process results
	results := int(resp.Results)
	span.AddAnnotation("search.results", results)

	return &proto.SearchResponse{
		Results: int32(results),
	}, nil
}

// Process implements the Process method for ServiceB
func (s *ServiceB) Process(ctx context.Context, req *proto.ProcessRequest) (*proto.ProcessResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.process")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	data := req.Data
	span.AddAnnotation("process.data_size", len(data))

	// Create some child spans to represent processing steps
	validateSpan := s.tracer.StartSpan("process.validate", pkg.WithParentSpan(span))
	// Validate data
	time.Sleep(10 * time.Millisecond)
	validateSpan.Finish()

	transformSpan := s.tracer.StartSpan("process.transform", pkg.WithParentSpan(span))
	// Transform data
	time.Sleep(30 * time.Millisecond)
	transformSpan.Finish()

	// Call service C to store processed data
	storeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	storeResp, err := s.serviceCClient.StoreData(storeCtx, &proto.StoreDataRequest{
		Data: fmt.Sprintf("processed:%s", data),
	})

	if err != nil {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return nil, err
	}

	return &proto.ProcessResponse{
		Status: storeResp.Status,
	}, nil
}

// ForwardHello implements the ForwardHello method for ServiceB
func (s *ServiceB) ForwardHello(ctx context.Context, req *proto.ForwardHelloRequest) (*proto.ForwardHelloResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.forward_hello")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	span.AddAnnotation("hello.name", req.Name)

	// Call service C
	resp, err := s.serviceCClient.Greet(ctx, &proto.GreetRequest{
		Name: req.Name,
	})

	if err != nil {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return nil, err
	}

	// Toggle simulated failure every other request
	s.simulatedFailure = !s.simulatedFailure
	if s.simulatedFailure && strings.Contains(req.Name, "fail") {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", "simulated_failure")
		return nil, fmt.Errorf("simulated failure in Service B")
	}

	return &proto.ForwardHelloResponse{
		Message: resp.Greeting,
	}, nil
}
