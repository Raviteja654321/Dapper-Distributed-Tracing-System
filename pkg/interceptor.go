package pkg

import (
	"context"
	"fmt"
	"io"
	"time"

	proto "github.com/sriharib128/distributed-tracer/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// UnaryClientInterceptor creates a client interceptor for unary RPCs
func UnaryClientInterceptor(tracer *Tracer) grpc.UnaryClientInterceptor {
	fmt.Println("line 18 in interceptor")
	return func(
		ctx context.Context,
		method string,
		req, resp interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		// Try to get span from context
		var span *Span
		var ok bool

		if span, ok = SpanFromContext(ctx); !ok {
			// No span in context, create one
			span = tracer.StartSpan(fmt.Sprintf("grpc.client:%s", method))
			ctx = ContextWithSpan(ctx, span)
		}

		// Inject span context to metadata
		ctx = InjectSpanToMetadata(ctx, span)

		// Record the start time
		startTime := time.Now()

		// Add metadata about the call
		span.AddAnnotation("grpc.method", method)
		span.AddAnnotation("grpc.start_time", startTime.Format(time.RFC3339Nano))
		fmt.Println("---- line 46 in interceptor.go --- ")
		fmt.Printf("%v",span)
		fmt.Println("---- line 46 in interceptor.go --- ")
		// Make the call
		err := invoker(ctx, method, req, resp, cc, opts...)

		// Record the result
		span.AddAnnotation("grpc.duration_ms", time.Since(startTime).Milliseconds())

		if err != nil {
			span.SetStatus(proto.Status_STATUS_ERROR)
			span.AddAnnotation("grpc.error", err.Error())
		} else {
			span.SetStatus(proto.Status_STATUS_OK)
		}

		// Finish the span
		span.Finish()

		return err
	}
}

// UnaryServerInterceptor creates a server interceptor for unary RPCs
func UnaryServerInterceptor(tracer *Tracer) grpc.UnaryServerInterceptor {
	fmt.Println("line 69 in interceptor")
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Extract span context from metadata
		spanCtx := ExtractSpanFromMetadata(ctx)
		fmt.Println("line 78 in interceptor.go")

		// Create a new span
		var span *Span
		var newCtx context.Context

		if spanCtx != nil {
			// Create child span from extracted context
			span = tracer.StartSpan(fmt.Sprintf("grpc.server:%s", info.FullMethod),
				WithParentSpan(spanCtx))
		} else {
			fmt.Printf("line 90")
			// Create a new root span
			span = tracer.StartSpan(fmt.Sprintf("grpc.server:%s", info.FullMethod))
		}
		// Add span to context
		newCtx = ContextWithSpan(ctx, span)

		// Add metadata about the call
		span.AddAnnotation("grpc.method", info.FullMethod)
		span.AddAnnotation("grpc.start_time", time.Now().Format(time.RFC3339Nano))

		// Handle the request
		resp, err := handler(newCtx, req)

		// Record the result
		if err != nil {
			span.SetStatus(proto.Status_STATUS_ERROR)
			span.AddAnnotation("grpc.error", err.Error())
		} else {
			span.SetStatus(proto.Status_STATUS_OK)
		}

		// Finish the span
		span.Finish()

		return resp, err
	}
}

// StreamClientInterceptor creates a client interceptor for streaming RPCs
func StreamClientInterceptor(tracer *Tracer) grpc.StreamClientInterceptor {
	fmt.Println("line 119 in interceptor")

	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		// Try to get span from context
		var span *Span
		var ok bool

		if span, ok = SpanFromContext(ctx); !ok {
			// No span in context, create one
			span = tracer.StartSpan(fmt.Sprintf("grpc.client.stream:%s", method))
			ctx = ContextWithSpan(ctx, span)
		}

		// Inject span context to metadata
		ctx = InjectSpanToMetadata(ctx, span)

		// Add metadata about the call
		span.AddAnnotation("grpc.method", method)
		span.AddAnnotation("grpc.streaming", "true")
		span.AddAnnotation("grpc.start_time", time.Now().Format(time.RFC3339Nano))

		// Create the client stream
		stream, err := streamer(ctx, desc, cc, method, opts...)

		if err != nil {
			span.SetStatus(proto.Status_STATUS_ERROR)
			span.AddAnnotation("grpc.error", err.Error())
			span.Finish()
			return nil, err
		}

		// Wrap the stream to capture the finish event
		return &wrappedClientStream{
			ClientStream: stream,
			span:         span,
		}, nil
	}
}

// StreamServerInterceptor creates a server interceptor for streaming RPCs
func StreamServerInterceptor(tracer *Tracer) grpc.StreamServerInterceptor {
	fmt.Println("line 168 in interceptor")

	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		// Extract the context from the stream
		ctx := ss.Context()

		// Extract span context from metadata
		spanCtx := ExtractSpanFromMetadata(ctx)
		fmt.Println("line 180")
		// Create a new span
		var span *Span
		fmt.Println("line 183")

		if spanCtx != nil {
			// Create child span from extracted context
			span = tracer.StartSpan(fmt.Sprintf("grpc.server.stream:%s", info.FullMethod),
				WithParentSpan(spanCtx))
		} else {
			// Create a new root span
			span = tracer.StartSpan(fmt.Sprintf("grpc.server.stream:%s", info.FullMethod))
		}
		fmt.Println("line 192")

		// Add metadata about the call
		span.AddAnnotation("grpc.method", info.FullMethod)
		span.AddAnnotation("grpc.streaming", "true")
		span.AddAnnotation("grpc.start_time", time.Now().Format(time.RFC3339Nano))
		fmt.Println("line 197")
		// Wrap the server stream
		wrappedStream := &wrappedServerStream{
			ServerStream: ss,
			ctx:          ContextWithSpan(ctx, span),
			span:         span,
		}

		// Handle the request
		err := handler(srv, wrappedStream)

		// Record the result
		if err != nil {
			span.SetStatus(proto.Status_STATUS_ERROR)
			span.AddAnnotation("grpc.error", err.Error())
		} else {
			span.SetStatus(proto.Status_STATUS_OK)
		}

		// Finish the span
		span.Finish()

		return err
	}
}

// InjectSpanToMetadata adds span context to gRPC metadata
func InjectSpanToMetadata(ctx context.Context, span *Span) context.Context {
	fmt.Println("line 230 in interceptor")
	if span == nil {
		return ctx
	}

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}

	md.Set(SpanIDHeader, span.SpanID)
	md.Set(TraceIDHeader, span.TraceID)

	if span.ParentSpanID != "" {
		md.Set(ParentSpanIDHeader, span.ParentSpanID)
	}

	// Add Vector Clock Timestamps to metadata
	if span.VectorClock != nil {
		vectorClockJSON, err := span.VectorClock.ToJSON()
		if err == nil {
			md.Set(VectorClockHeader, vectorClockJSON)
		} else {
			fmt.Println("Failed to serialize vector clock:", err)
		}
	}

	return metadata.NewOutgoingContext(ctx, md)
}


// ExtractSpanFromMetadata extracts span context from gRPC metadata
func ExtractSpanFromMetadata(ctx context.Context) *Span {
	fmt.Println("line 265 in interceptor")

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	spanID := getFirstString(md, SpanIDHeader)
	traceID := getFirstString(md, TraceIDHeader)
	parentSpanID := getFirstString(md, ParentSpanIDHeader)
	vectorClockJSON := getFirstString(md, VectorClockHeader)

	if spanID == "" || traceID == "" {
		return nil
	}

	// Create a span with the extracted data
	span := &Span{
		SpanID:       spanID,
		TraceID:      traceID,
		ParentSpanID: parentSpanID,
	}

	// Deserialize and set vector clock if available
	if vectorClockJSON != "" {
		vectorClock, err := VectorClockFromJSON(vectorClockJSON)
		if err == nil {
			span.VectorClock = vectorClock
		} else {
			fmt.Println("Failed to deserialize vector clock:", err)
		}
	}

	// Print span details to terminal
	fmt.Println("Printing extracted span details:")
	fmt.Println("SpanID:", span.SpanID)
	fmt.Println("TraceID:", span.TraceID)
	fmt.Println("ParentSpanID:", span.ParentSpanID)
	if span.VectorClock != nil {
		fmt.Println("VectorClock:", span.VectorClock.Timestamps)
	} else {
		fmt.Println("VectorClock: nil")
	}

	return span
}

// getFirstString gets the first value for a key in metadata
func getFirstString(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

// RecoveryInterceptor handles panics in RPC handlers
func RecoveryInterceptor() grpc.UnaryServerInterceptor {
	fmt.Println("line 283 in interceptor")

	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				// Get the span from context
				if span, ok := SpanFromContext(ctx); ok {
					span.SetStatus(proto.Status_STATUS_ERROR)
					span.AddAnnotation("panic", fmt.Sprintf("%v", r))
					span.Finish()
				}

				err = status.Errorf(codes.Internal, "panic: %v", r)
			}
		}()

		return handler(ctx, req)
	}
}

// Wrapper for client streams
type wrappedClientStream struct {
	grpc.ClientStream
	span *Span
}

// RecvMsg wraps the RecvMsg method
func (w *wrappedClientStream) RecvMsg(m interface{}) error {
	err := w.ClientStream.RecvMsg(m)

	if err != nil && err != io.EOF {
		w.span.SetStatus(proto.Status_STATUS_ERROR)
		w.span.AddAnnotation("grpc.error", err.Error())
	}

	if err == io.EOF {
		// Stream ended, finish span
		w.span.Finish()
	}

	return err
}

// SendMsg wraps the SendMsg method
func (w *wrappedClientStream) SendMsg(m interface{}) error {
	err := w.ClientStream.SendMsg(m)

	if err != nil {
		w.span.SetStatus(proto.Status_STATUS_ERROR)
		w.span.AddAnnotation("grpc.error", err.Error())
		w.span.Finish()
	}

	return err
}

// Wrapper for server streams
type wrappedServerStream struct {
	grpc.ServerStream
	ctx  context.Context
	span *Span
}

// Context returns the wrapped context
func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}

// RecvMsg wraps the RecvMsg method
func (w *wrappedServerStream) RecvMsg(m interface{}) error {
	err := w.ServerStream.RecvMsg(m)

	if err != nil && err != io.EOF {
		w.span.SetStatus(proto.Status_STATUS_ERROR)
		w.span.AddAnnotation("grpc.error", err.Error())
	}

	return err
}

// SendMsg wraps the SendMsg method
func (w *wrappedServerStream) SendMsg(m interface{}) error {
	err := w.ServerStream.SendMsg(m)

	if err != nil {
		w.span.SetStatus(proto.Status_STATUS_ERROR)
		w.span.AddAnnotation("grpc.error", err.Error())
	}

	return err
}
