package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		grpcAddr  = flag.String("grpc-addr", ":50051", "gRPC listen address")
		redisAddr = flag.String("redis-addr", "localhost:6379", "Redis address")
		redisPass = flag.String("redis-pass", "", "Redis password (optional)")
		redisDB   = flag.Int("redis-db", 0, "Redis database number")
		keyTTL    = flag.Duration("key-ttl", 10*time.Minute, "TTL to refresh on key hit")
		logLevel  = flag.String("log-level", "info", "Log level: debug, info, warn, error")
	)
	flag.Parse()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	proc, err := New(Config{
		RedisAddr:     *redisAddr,
		RedisPassword: *redisPass,
		RedisDB:       *redisDB,
		KeyTTL:        *keyTTL,
	})
	if err != nil {
		slog.Error("failed to create processor", "err", err)
		os.Exit(1)
	}
	defer proc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proc.Ping(ctx); err != nil {
		slog.Error("redis ping failed", "addr", *redisAddr, "err", err)
		os.Exit(1)
	}
	slog.Info("redis connection healthy", "addr", *redisAddr)

	srv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	pb.RegisterExternalProcessorServer(srv, proc)

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	reflection.Register(srv)

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("failed to listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}

	slog.Info("ext_proc server listening", "addr", *grpcAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", fmt.Sprintf("%s", sig))
		healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		slog.Error("gRPC server error", "err", err)
		os.Exit(1)
	}
}
