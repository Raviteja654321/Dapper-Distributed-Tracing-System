// services/service-a.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sriharib128/distributed-tracer/pkg"
	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// ServiceA represents the service A implementation
type ServiceA struct {
	proto.UnimplementedServiceAServer
	tracer         *pkg.Tracer
	serviceBClient proto.ServiceBClient
}

// Service A: API Gateway/Frontend Service
func main() {
	// Parse command-line flags
	port := flag.Int("port", 8000, "HTTP/gRPC port")
	grpcServiceBAddr := flag.String("service-b", "localhost:8001", "Service B gRPC address")
	collectorAddr := flag.String("collector", "localhost:7777", "Collector address")
	flag.Parse()

	// Setup tracer
	tracer, err := setupTracer("service-a", *collectorAddr)
	if err != nil {
		log.Fatalf("Failed to setup tracer: %v", err)
	}
	defer tracer.Close()

	// Connect to service B
	conn, err := grpc.Dial(
		*grpcServiceBAddr,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(pkg.UnaryClientInterceptor(tracer)),
		grpc.WithStreamInterceptor(pkg.StreamClientInterceptor(tracer)),
	)
	if err != nil {
		log.Fatalf("Failed to connect to service B: %v", err)
	}
	defer conn.Close()

	serviceBClient := proto.NewServiceBClient(conn)

	// Create service
	service := &ServiceA{
		tracer:         tracer,
		serviceBClient: serviceBClient,
	}

	// Setup HTTP server
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: setupHTTPRoutes(service, tracer),
	}

	// Setup gRPC server on the same port
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(pkg.UnaryServerInterceptor(tracer)),
		grpc.StreamInterceptor(pkg.StreamServerInterceptor(tracer)),
	)

	// Register service
	proto.RegisterServiceAServer(grpcServer, service)

	// Enable reflection for debugging
	reflection.Register(grpcServer)

	// Start servers
	go func() {
		log.Printf("Service A HTTP server listening on port %d", *port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}

	grpcServer.GracefulStop()
	log.Println("Service A shutdown complete")
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

// setupHTTPRoutes configures the HTTP routes
func setupHTTPRoutes(service *ServiceA, tracer *pkg.Tracer) *mux.Router {
	router := mux.NewRouter()

	// Add middleware
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Create span for HTTP request
			span := tracer.StartSpan("http.request")
			ctx := pkg.ContextWithSpan(r.Context(), span)
			defer span.Finish()

			// Add HTTP details to span
			span.AddAnnotation("http.method", r.Method)
			span.AddAnnotation("http.url", r.URL.String())

			// Execute request with tracing context
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	// Routes
	router.HandleFunc("/", service.handleRoot)
	router.HandleFunc("/api/v1/search", service.handleSearch)
	router.HandleFunc("/api/v1/process", service.handleProcess)
	router.HandleFunc("/health", service.handleHealth)

	return router
}

// API Handlers

func (s *ServiceA) handleRoot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("handle.root")
		defer span.Finish()
	} else {
		// Create a child span for this handler
		span = s.tracer.StartSpan("handle.root", pkg.WithParentSpan(span))
		defer span.Finish()
	}

	span.AddAnnotation("client.address", r.RemoteAddr)

	// Simulate some work
	time.Sleep(10 * time.Millisecond)

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("Service A - API Gateway\n"))
}

func (s *ServiceA) handleSearch(w http.ResponseWriter, r *http.Request) {
	fmt.Println("line 179 in service a")
	ctx := r.Context()
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("handle.search")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	} else {
		// Create a child span for this handler
		span = s.tracer.StartSpan("handle.search", pkg.WithParentSpan(span))
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Missing query parameter 'q'"))
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", "missing_query")
		return
	}

	span.AddAnnotation("search.query", query)
	fmt.Println("line 202 in service-a")
	// Call service B
	resp, err := s.serviceBClient.Search(ctx, &proto.SearchRequest{
		Query: query,
	})
	fmt.Println("line 207 in service-a")

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error from Service B: %v", err)))
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return
	}
	fmt.Println("line 217 in service-a")

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"results": %d, "query": "%s"}`, resp.Results, query)))
}

func (s *ServiceA) handleProcess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("handle.process")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	} else {
		// Create a child span for this handler
		span = s.tracer.StartSpan("handle.process", pkg.WithParentSpan(span))
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	data := r.URL.Query().Get("data")
	if data == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Missing query parameter 'data'"))
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", "missing_data")
		return
	}

	// Add data size annotation
	span.AddAnnotation("process.data_size", len(data))

	// Call service B
	resp, err := s.serviceBClient.Process(ctx, &proto.ProcessRequest{
		Data: data,
	})

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error from Service B: %v", err)))
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"status": "%s", "processed": true}`, resp.Status)))
}

func (s *ServiceA) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("handle.health")
		defer span.Finish()
	} else {
		// Create a child span for this handler
		span = s.tracer.StartSpan("handle.health", pkg.WithParentSpan(span))
		defer span.Finish()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "ok"}`))
}

// GRPC Service implementation

// SayHello implements the SayHello method for ServiceA
func (s *ServiceA) SayHello(ctx context.Context, req *proto.HelloRequest) (*proto.HelloResponse, error) {
	// Get span from context
	span, ok := pkg.SpanFromContext(ctx)
	if !ok {
		span = s.tracer.StartSpan("grpc.say_hello")
		ctx = pkg.ContextWithSpan(ctx, span)
		defer span.Finish()
	}

	span.AddAnnotation("hello.name", req.Name)

	// Call service B
	resp, err := s.serviceBClient.ForwardHello(ctx, &proto.ForwardHelloRequest{
		Name: req.Name,
	})

	if err != nil {
		span.SetStatus(proto.Status_STATUS_ERROR)
		span.AddAnnotation("error", err.Error())
		return nil, err
	}

	return &proto.HelloResponse{
		Message: fmt.Sprintf("Hello %s! Service B says: %s", req.Name, resp.Message),
	}, nil
}