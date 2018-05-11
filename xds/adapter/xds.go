/*
Copyright 2018 Turbine Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adapter

import (
	"net"
	"time"

	envoyapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	accesslog "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v2"
	"github.com/envoyproxy/go-control-plane/pkg/server"
	"google.golang.org/grpc"

	"github.com/turbinelabs/nonstdlib/log/console"
	"github.com/turbinelabs/rotor/xds/poller"
	"github.com/turbinelabs/stats"
)

//go:generate mockgen -source $GOFILE -destination mock_$GOFILE -package $GOPACKAGE --write_package_comment=false

// XDS manages the lifecyle of an envoy xDS server. It also implements
// poller.Consumer, to allow it to consume objects from the Turbine Labs API.
type XDS interface {
	poller.Consumer

	// Run starts the XDS server. This method does not return unless
	// the server fails to start or is stopped from another goroutine
	// via Stop().
	Run() error

	// Stop terminates a running XDS server. It does not wait for
	// existing connections or requests to complete.
	Stop()

	// Addr retrieves the XDS server's listener address. Suitable for
	// retrieving the listener port when the initial address specified
	// port 0. If the server has not yet started, this method blocks.
	Addr() string
}

// XDSOption represents a configuration option for XDS.
type XDSOption struct {
	mkALSReporterConfig func() alsReporterConfig
}

func (x *XDSOption) merge(options []XDSOption) {
	for _, option := range options {
		if option.mkALSReporterConfig != nil {
			x.mkALSReporterConfig = option.mkALSReporterConfig
		}
	}
}

// WithTopResponseLog logs the top N non-success downstream responses to the console
// every interval. Requires GRPC logging.
func WithTopResponseLog(n int, interval time.Duration) XDSOption {
	return XDSOption{
		mkALSReporterConfig: func() alsReporterConfig {
			return alsReporterConfig{
				monitorAccessLog:  true,
				countRedirects:    true,
				countClientErrors: true,
				countServerErrors: true,
				cacheSize:         n,
				reportInterval:    interval,
			}
		},
	}
}

// NewXDS creates new XDS instances with the given listener address, object source,
// certificate authority file, and default timeout.
func NewXDS(
	addr string,
	registrar poller.Registrar,
	caFile string,
	defaultTimeout time.Duration,
	stats stats.Stats,
	options ...XDSOption,
) (XDS, error) {
	xdsOption := XDSOption{}
	xdsOption.merge(options)

	ldsConfig := listenerAdapterConfig{loggingCluster: xdsClusterName}
	snapshotCache := newSnapshotCache()
	consumer := newCachingConsumer(
		snapshotCache,
		registrar,
		ldsConfig,
		caFile,
		defaultTimeout,
	)

	alsReporterConfig := alsReporterConfig{}
	if xdsOption.mkALSReporterConfig != nil {
		alsReporterConfig = xdsOption.mkALSReporterConfig()
	}

	als, err := newALS(stats, alsReporterConfig)
	if err != nil {
		return nil, err
	}

	return &xds{
		Consumer:         consumer,
		addr:             addr,
		server:           server.NewServer(snapshotCache, consumer),
		logServer:        als,
		resolvedAddrChan: make(chan string, 1),
	}, nil
}

type xds struct {
	poller.Consumer
	addr string

	server           server.Server
	logServer        accesslog.AccessLogServiceServer
	resolvedAddr     string
	resolvedAddrChan chan string
	gRPCServer       *grpc.Server
}

func (x *xds) Run() error {
	lis, err := net.Listen("tcp", x.addr)
	if err != nil {
		return err
	}

	// The original address may specify a dynamic port, so retrieve
	// the actual listener address.
	resolvedAddr := lis.Addr().String()
	x.resolvedAddrChan <- resolvedAddr

	console.Info().Printf("serving xDS on %s", resolvedAddr)

	x.gRPCServer = grpc.NewServer()
	envoyapi.RegisterClusterDiscoveryServiceServer(x.gRPCServer, x.server)
	envoyapi.RegisterEndpointDiscoveryServiceServer(x.gRPCServer, x.server)
	envoyapi.RegisterRouteDiscoveryServiceServer(x.gRPCServer, x.server)
	envoyapi.RegisterListenerDiscoveryServiceServer(x.gRPCServer, x.server)

	if x.logServer != nil {
		console.Info().Println("log streaming enabled")
		accesslog.RegisterAccessLogServiceServer(x.gRPCServer, x.logServer)
	}

	if err := x.gRPCServer.Serve(lis); err != nil {
		x.gRPCServer = nil
		return err
	}

	return nil
}

func (x *xds) Addr() string {
	// First caller waits on the channel, records the address, and
	// then closes the channel. All subsequent callers see a closed
	// channel (perhaps after blocking) and return the resolved
	// address.
	addr, ok := <-x.resolvedAddrChan
	if ok {
		x.resolvedAddr = addr
		close(x.resolvedAddrChan)
	}

	return x.resolvedAddr
}

func (x *xds) Stop() {
	if x.gRPCServer != nil {
		x.gRPCServer.Stop()
	}
}