package endpoints

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	discovery_v2 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	secret_v3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/sirupsen/logrus"
	workload_pb "github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	healthv1 "github.com/spiffe/spire/pkg/agent/api/health/v1"
	"github.com/spiffe/spire/pkg/agent/endpoints/sdsv2"
	"github.com/spiffe/spire/pkg/agent/endpoints/sdsv3"
	"github.com/spiffe/spire/pkg/agent/endpoints/workload"
	"github.com/spiffe/spire/pkg/common/api/middleware"
	"github.com/spiffe/spire/pkg/common/peertracker"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type Server interface {
	ListenAndServe(ctx context.Context) error
}

type Endpoints struct {
	addr              net.Addr
	log               logrus.FieldLogger
	metrics           telemetry.Metrics
	workloadAPIServer workload_pb.SpiffeWorkloadAPIServer
	sdsv2Server       discovery_v2.SecretDiscoveryServiceServer
	sdsv3Server       secret_v3.SecretDiscoveryServiceServer
	healthServer      grpc_health_v1.HealthServer

	hooks struct {
		// test hook used to indicate that is listening
		listening chan struct{}
	}
}

func New(c Config) *Endpoints {
	attestor := PeerTrackerAttestor{Attestor: c.Attestor}

	if c.newWorkloadAPIServer == nil {
		c.newWorkloadAPIServer = func(c workload.Config) workload_pb.SpiffeWorkloadAPIServer {
			return workload.New(c)
		}
	}
	if c.newSDSv2Server == nil {
		c.newSDSv2Server = func(c sdsv2.Config) discovery_v2.SecretDiscoveryServiceServer {
			return sdsv2.New(c)
		}
	}
	if c.newSDSv3Server == nil {
		c.newSDSv3Server = func(c sdsv3.Config) secret_v3.SecretDiscoveryServiceServer {
			return sdsv3.New(c)
		}
	}
	if c.newHealthServer == nil {
		c.newHealthServer = func(c healthv1.Config) grpc_health_v1.HealthServer {
			return healthv1.New(c)
		}
	}

	allowedClaims := make(map[string]struct{}, len(c.AllowedForeignJWTClaims))
	for _, claim := range c.AllowedForeignJWTClaims {
		allowedClaims[claim] = struct{}{}
	}

	workloadAPIServer := c.newWorkloadAPIServer(workload.Config{
		Manager:                       c.Manager,
		Attestor:                      attestor,
		AllowUnauthenticatedVerifiers: c.AllowUnauthenticatedVerifiers,
		AllowedForeignJWTClaims:       allowedClaims,
		TrustDomain:                   c.TrustDomain,
	})

	sdsv2Server := c.newSDSv2Server(sdsv2.Config{
		Attestor:          attestor,
		Manager:           c.Manager,
		DefaultSVIDName:   c.DefaultSVIDName,
		DefaultBundleName: c.DefaultBundleName,
	})

	sdsv3Server := c.newSDSv3Server(sdsv3.Config{
		Attestor:              attestor,
		Manager:               c.Manager,
		DefaultSVIDName:       c.DefaultSVIDName,
		DefaultBundleName:     c.DefaultBundleName,
		DefaultAllBundlesName: c.DefaultAllBundlesName,
	})

	healthServer := c.newHealthServer(healthv1.Config{
		Addr: c.BindAddr,
	})

	return &Endpoints{
		addr:              c.BindAddr,
		log:               c.Log,
		metrics:           c.Metrics,
		workloadAPIServer: workloadAPIServer,
		sdsv2Server:       sdsv2Server,
		sdsv3Server:       sdsv3Server,
		healthServer:      healthServer,
	}
}

func (e *Endpoints) ListenAndServe(ctx context.Context) error {
	unaryInterceptor, streamInterceptor := middleware.Interceptors(
		Middleware(e.log, e.metrics),
	)

	server := grpc.NewServer(
		grpc.Creds(peertracker.NewCredentials()),
		grpc.UnaryInterceptor(unaryInterceptor),
		grpc.StreamInterceptor(streamInterceptor),
	)

	workload_pb.RegisterSpiffeWorkloadAPIServer(server, e.workloadAPIServer)
	discovery_v2.RegisterSecretDiscoveryServiceServer(server, e.sdsv2Server)
	secret_v3.RegisterSecretDiscoveryServiceServer(server, e.sdsv3Server)
	grpc_health_v1.RegisterHealthServer(server, e.healthServer)

	var l net.Listener
	var err error
	switch e.addr.Network() {
	case "unix":
		l, err = e.createUDSListener()
	case "tcp":
		l, err = e.createTCPListener()
	default:
		return net.UnknownNetworkError(e.addr.Network())
	}

	if err != nil {
		return err
	}
	defer l.Close()

	// Update the listening address with the actual address.
	// If a TCP address was specified with port 0, this will
	// update the address with the actual port that is used
	// to listen.
	e.addr = l.Addr()
	e.log.WithFields(logrus.Fields{
		telemetry.Network: e.addr.Network(),
		telemetry.Address: e.addr,
	}).Info("Starting Workload and SDS APIs")
	e.triggerListeningHook()
	errChan := make(chan error)
	go func() { errChan <- server.Serve(l) }()

	select {
	case err = <-errChan:
	case <-ctx.Done():
		e.log.Info("Stopping Workload and SDS APIs")
		server.Stop()
		err = <-errChan
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
	}
	return err
}

func (e *Endpoints) createUDSListener() (net.Listener, error) {
	// Remove uds if already exists
	os.Remove(e.addr.String())

	unixListener := &peertracker.ListenerFactory{
		Log: e.log,
	}

	unixAddr, ok := e.addr.(*net.UnixAddr)
	if !ok {
		return nil, fmt.Errorf("create UDS listener: address is type %T, not net.UnixAddr", e.addr)
	}
	l, err := unixListener.ListenUnix(e.addr.Network(), unixAddr)
	if err != nil {
		return nil, fmt.Errorf("create UDS listener: %w", err)
	}

	if err := os.Chmod(e.addr.String(), os.ModePerm); err != nil {
		return nil, fmt.Errorf("unable to change UDS permissions: %w", err)
	}
	return l, nil
}

func (e *Endpoints) createTCPListener() (net.Listener, error) {
	tcpListener := &peertracker.ListenerFactory{
		Log: e.log,
	}

	l, err := tcpListener.ListenTCP(e.addr.Network(), e.addr.(*net.TCPAddr))
	if err != nil {
		return nil, fmt.Errorf("create TCP listener: %w", err)
	}
	return l, nil
}

func (e *Endpoints) triggerListeningHook() {
	if e.hooks.listening != nil {
		e.hooks.listening <- struct{}{}
	}
}
