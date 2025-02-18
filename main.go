// Copyright (c) 2020-2021 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !windows

package main

import (
	"context"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/grpcfd"
	"github.com/golang/protobuf/ptypes"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/noop"
	"github.com/networkservicemesh/api/pkg/api/networkservice/payload"
	"github.com/networkservicemesh/api/pkg/api/registry"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms"
	registryclient "github.com/networkservicemesh/sdk/pkg/registry/chains/client"
	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/jaeger"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/opentracing"
	"github.com/networkservicemesh/sdk/pkg/tools/signalctx"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"

	"github.com/networkservicemesh/cmd-nse-vfio/internal/config"
	"github.com/networkservicemesh/cmd-nse-vfio/internal/networkservice/mapserver"
)

const (
	serviceDomainLabel = "serviceDomain"
)

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx := signalctx.WithSignals(context.Background())
	ctx, cancel := context.WithCancel(ctx)

	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithFields(ctx, map[string]interface{}{"cmd": os.Args[0]})
	ctx = log.WithLog(ctx, logruslogger.New(ctx))

	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}

	// ********************************************************************************
	// Configure open tracing
	// ********************************************************************************
	log.EnableTracing(true)
	jaegerCloser := jaeger.InitJaeger(ctx, "cmd-nse-vfio")
	defer func() { _ = jaegerCloser.Close() }()

	// enumerating phases
	log.FromContext(ctx).Infof("there are 5 phases which will be executed followed by a success message:")
	log.FromContext(ctx).Infof("the phases include:")
	log.FromContext(ctx).Infof("1: get config from environment")
	log.FromContext(ctx).Infof("2: retrieve spiffe svid")
	log.FromContext(ctx).Infof("3: create vfio server nse")
	log.FromContext(ctx).Infof("4: create grpc and mount nse")
	log.FromContext(ctx).Infof("5: register nse with nsm")
	log.FromContext(ctx).Infof("a final success message with start time duration")

	starttime := time.Now()

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 1: get config from environment")
	// ********************************************************************************
	cfg := new(config.Config)
	if err := cfg.Process(); err != nil {
		logrus.Fatal(err.Error())
	}

	log.FromContext(ctx).Infof("Config: %#v", cfg)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 2: retrieving svid, check spire agent logs if this is the last line you see")
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	log.FromContext(ctx).Infof("SVID: %q", svid.ID)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 3: create vfio-server network service endpoint")
	// ********************************************************************************
	responderEndpoint := endpoint.NewServer(
		ctx,
		cfg.Name,
		authorize.NewServer(),
		spiffejwt.TokenGeneratorFunc(source, cfg.MaxTokenLifetime),
		mechanisms.NewServer(map[string]networkservice.NetworkServiceServer{
			noop.MECHANISM: mapserver.NewServer(cfg),
		}))

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 4: create grpc server and register vfio-server")
	// ********************************************************************************
	options := append(
		opentracing.WithTracing(),
		grpc.Creds(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
	)
	server := grpc.NewServer(options...)
	responderEndpoint.Register(server)
	tmpDir, err := ioutil.TempDir("", cfg.Name)
	if err != nil {
		logrus.Fatalf("error creating tmpDir %+v", err)
	}
	defer func(tmpDir string) { _ = os.Remove(tmpDir) }(tmpDir)
	listenOn := &(url.URL{Scheme: "unix", Path: filepath.Join(tmpDir, "listen.on")})
	srvErrCh := grpcutils.ListenAndServe(ctx, listenOn, server)
	exitOnErr(ctx, cancel, srvErrCh)
	log.FromContext(ctx).Infof("grpc server started")

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 5: register nse with nsm")
	// ********************************************************************************
	clientOptions := append(
		opentracing.WithTracingDial(),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
	)
	cc, err := grpc.DialContext(ctx,
		grpcutils.URLToTarget(&cfg.ConnectTo),
		clientOptions...,
	)
	if err != nil {
		log.FromContext(ctx).Fatalf("error establishing grpc connection to registry server %+v", err)
	}

	nsRegistryClient := registryclient.NewNetworkServiceRegistryClient(cc)
	for i := range cfg.Services {
		nsName := cfg.Services[i].Name
		if _, err = nsRegistryClient.Register(ctx, &registry.NetworkService{
			Name:    nsName,
			Payload: payload.Ethernet,
		}); err != nil {
			log.FromContext(ctx).Fatalf("failed to register ns(%s) %s", nsName, err.Error())
		}
	}

	nseRegistryClient := registryclient.NewNetworkServiceEndpointRegistryClient(ctx, cc)
	nse, err := nseRegistryClient.Register(ctx, registryEndpoint(listenOn, cfg))
	if err != nil {
		log.FromContext(ctx).Fatalf("unable to register nse %+v", err)
	}
	logrus.Infof("nse: %+v", nse)

	// ********************************************************************************
	log.FromContext(ctx).Infof("startup completed in %v", time.Since(starttime))
	// ********************************************************************************

	// wait for server to exit
	<-ctx.Done()
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}

func registryEndpoint(listenOn *url.URL, cfg *config.Config) *registry.NetworkServiceEndpoint {
	expireTime, _ := ptypes.TimestampProto(time.Now().Add(cfg.MaxTokenLifetime))

	nse := &registry.NetworkServiceEndpoint{
		Name:                 cfg.Name,
		NetworkServiceNames:  make([]string, len(cfg.Services)),
		NetworkServiceLabels: make(map[string]*registry.NetworkServiceLabels, len(cfg.Services)),
		Url:                  grpcutils.URLToTarget(listenOn),
		ExpirationTime:       expireTime,
	}

	for i := range cfg.Services {
		service := &cfg.Services[i]

		labels := service.Labels
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[serviceDomainLabel] = service.Domain

		nse.NetworkServiceNames[i] = service.Name
		nse.NetworkServiceLabels[service.Name] = &registry.NetworkServiceLabels{
			Labels: labels,
		}
	}

	return nse
}
