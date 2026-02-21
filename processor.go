// Package processor implements the Envoy ext_proc gRPC service.
//
// Flow for each request:
//  1. Envoy sends ProcessingRequest_RequestHeaders.
//  2. We extract the :path pseudo-header and use it as the Redis key.
//  3. GET from Redis:
//     - MISS  → HeadersResponse{CONTINUE}, Envoy forwards to upstream normally.
//     - HIT   → ImmediateResponse{200, body=value}, Envoy short-circuits the request.
//     EXPIRE is issued asynchronously to refresh the TTL.
//
// processing_mode in envoy.yaml is set to only SEND request headers, so in
// practice we will never receive RequestBody / ResponseHeaders / etc. messages.
// The default branch handles them defensively anyway.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Config holds options for creating a Processor.
type Config struct {
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	// KeyTTL is applied (via EXPIRE) every time a cache hit occurs.
	KeyTTL time.Duration
}

// Processor implements pb.ExternalProcessorServer.
type Processor struct {
	pb.UnimplementedExternalProcessorServer
	rdb    *redis.Client
	keyTTL time.Duration
}

// New creates a Processor and initialises the Redis connection pool.
func New(cfg Config) (*Processor, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     20,
		MinIdleConns: 4,
	})

	ttl := cfg.KeyTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}

	return &Processor{rdb: rdb, keyTTL: ttl}, nil
}

// Ping checks Redis connectivity.
func (p *Processor) Ping(ctx context.Context) error {
	return p.rdb.Ping(ctx).Err()
}

// Close shuts down the Redis client pool.
func (p *Processor) Close() {
	_ = p.rdb.Close()
}

// Process is the bidirectional streaming RPC that Envoy calls once per HTTP
// request (or connection, depending on ext_proc configuration).
func (p *Processor) Process(stream pb.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// Cancelled means the downstream closed the connection cleanly.
			if status.Code(err) == codes.Canceled {
				return nil
			}
			slog.Error("stream recv error", "err", err)
			return err
		}

		resp := p.buildResponse(ctx, req)

		if err := stream.Send(resp); err != nil {
			slog.Error("stream send error", "err", err)
			return err
		}
	}
}

// buildResponse decides what ProcessingResponse to send back for a given
// ProcessingRequest. It never returns nil.
func (p *Processor) buildResponse(ctx context.Context, req *pb.ProcessingRequest) *pb.ProcessingResponse {
	switch v := req.Request.(type) {

	case *pb.ProcessingRequest_RequestHeaders:
		resp, err := p.handleRequestHeaders(ctx, v.RequestHeaders)
		if err != nil {
			slog.Error("handleRequestHeaders error, failing open", "err", err)
			return continueHeaders()
		}
		return resp

	case *pb.ProcessingRequest_RequestBody:
		// We don't ask for the request body (NONE in processing_mode), but
		// handle it defensively so Envoy doesn't hang waiting for a reply.
		return &pb.ProcessingResponse{
			Response: &pb.ProcessingResponse_RequestBody{
				RequestBody: &pb.BodyResponse{
					Response: &pb.CommonResponse{
						Status: pb.CommonResponse_CONTINUE,
					},
				},
			},
		}

	case *pb.ProcessingRequest_ResponseHeaders:
		return &pb.ProcessingResponse{
			Response: &pb.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &pb.HeadersResponse{
					Response: &pb.CommonResponse{
						Status: pb.CommonResponse_CONTINUE,
					},
				},
			},
		}

	case *pb.ProcessingRequest_ResponseBody:
		return &pb.ProcessingResponse{
			Response: &pb.ProcessingResponse_ResponseBody{
				ResponseBody: &pb.BodyResponse{
					Response: &pb.CommonResponse{
						Status: pb.CommonResponse_CONTINUE,
					},
				},
			},
		}

	default:
		// Unknown phase — return a no-op RequestHeaders response as a fallback.
		// This should never happen given our processing_mode config.
		slog.Warn("unexpected processing request type, passing through",
			"type", fmt.Sprintf("%T", req.Request))
		return continueHeaders()
	}
}

// handleRequestHeaders inspects the :path header, queries Redis, and returns
// either an immediate cached response or a continue (pass-through) response.
func (p *Processor) handleRequestHeaders(ctx context.Context, reqHeaders *pb.HttpHeaders) (*pb.ProcessingResponse, error) {
	path := extractHeader(reqHeaders, ":path")
	method := extractHeader(reqHeaders, ":method")
	allHeaders := reqHeaders.GetHeaders()
	for _, h := range allHeaders.Headers {
		slog.Debug("request header", "key", h.Key, "value", string(h.RawValue))
	}

	if path == "" {
		slog.Warn("request has no :path header, passing through")
		return continueHeaders(), nil
	}

	// The Redis key is the raw :path value (including query string).
	// To strip query params: strings.SplitN(path, "?", 2)[0]
	// To strip the leading slash: strings.TrimPrefix(path, "/")
	key := path

	slog.Debug("redis lookup", "key", key, "method", method)

	val, err := p.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		slog.Info("cache miss", "key", key)
		return continueHeaders(), nil
	}
	if err != nil {
		// Redis unavailable — fail open so traffic is not dropped.
		slog.Error("redis GET error, failing open", "key", key, "err", err)
		return continueHeaders(), nil
	}

	// Cache hit: refresh TTL asynchronously, then short-circuit with cached body.
	slog.Info("cache hit", "key", key, "value_bytes", len(val))
	go p.refreshTTL(key)

	return immediateResponse(http.StatusOK, val, key), nil
}

// refreshTTL issues an EXPIRE command in the background so it does not add
// latency to the response path.
func (p *Processor) refreshTTL(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.rdb.Expire(ctx, key, p.keyTTL).Err(); err != nil {
		slog.Warn("failed to refresh TTL", "key", key, "err", err)
	}
}

// extractHeader returns the value of the first header matching name.
// HTTP/2 pseudo-headers (":path", ":method", etc.) and regular headers are
// both lower-cased by Envoy before being forwarded to ext_proc.
func extractHeader(hdrs *pb.HttpHeaders, name string) string {
	if hdrs == nil || hdrs.Headers == nil {
		return ""
	}
	for _, h := range hdrs.Headers.Headers {
		if h.Key == name {
			return string(h.RawValue)
		}
	}
	return ""
}

// continueHeaders returns a ProcessingResponse that tells Envoy to proceed
// with the request unchanged.
func continueHeaders() *pb.ProcessingResponse {
	return &pb.ProcessingResponse{
		Response: &pb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &pb.HeadersResponse{
				Response: &pb.CommonResponse{
					Status: pb.CommonResponse_CONTINUE,
				},
			},
		},
	}
}

// immediateResponse builds a ProcessingResponse that short-circuits the
// request and returns a synthetic HTTP response directly to the client.
// Envoy does not contact the upstream.
func immediateResponse(statusCode int, body string, cacheKey string) *pb.ProcessingResponse {
	return &pb.ProcessingResponse{
		Response: &pb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &pb.ImmediateResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode(statusCode),
				},
				Headers: &pb.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{
						{
							Header: &corev3.HeaderValue{
								Key:   "content-type",
								Value: "text/plain; charset=utf-8",
							},
						},
						{
							Header: &corev3.HeaderValue{
								Key:   "x-cache",
								Value: "HIT",
							},
						},
						{
							Header: &corev3.HeaderValue{
								Key:   "x-cache-key",
								Value: cacheKey,
							},
						},
					},
				},
				Body: body,
			},
		},
	}
}
