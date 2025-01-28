/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package xdsclient

import (
	"fmt"
	"sync"
	"time"

	v3statuspb "github.com/envoyproxy/go-control-plane/envoy/service/status/v3"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/xds/bootstrap"
)

var (
	// DefaultPool is the default pool for xDS clients. It is created at init
	// time by reading bootstrap configuration from env vars.
	DefaultPool *Pool
)

// Pool represents a pool of xDS clients that share the same bootstrap
// configuration.
type Pool struct {
	// Note that mu should ideally only have to guard clients. But here, we need
	// it to guard config as well since SetFallbackBootstrapConfig writes to
	// config.
	mu      sync.Mutex
	clients map[string]*clientRefCounted
	config  *bootstrap.Config
}

// OptionsForTesting contains options to configure xDS client creation for
// testing purposes only.
type OptionsForTesting struct {
	// Name is a unique name for this xDS client.
	Name string

	// WatchExpiryTimeout is the timeout for xDS resource watch expiry. If
	// unspecified, uses the default value used in non-test code.
	WatchExpiryTimeout time.Duration

	// StreamBackoffAfterFailure is the backoff function used to determine the
	// backoff duration after stream failures.
	// If unspecified, uses the default value used in non-test code.
	StreamBackoffAfterFailure func(int) time.Duration
}

// NewPool creates a new xDS client pool with the given bootstrap config.
//
// If a nil bootstrap config is passed and SetFallbackBootstrapConfig is not
// called before a call to NewClient, the latter will fail. i.e. if there is an
// attempt to create an xDS client from the pool without specifying bootstrap
// configuration (either at pool creation time or by setting the fallback
// bootstrap configuration), xDS client creation will fail.
func NewPool(config *bootstrap.Config) *Pool {
	return &Pool{
		clients: make(map[string]*clientRefCounted),
		config:  config,
	}
}

// NewClient returns an xDS client with the given name from the pool. If the
// client doesn't already exist, it creates a new xDS client and adds it to the
// pool.
//
// The second return value represents a close function which the caller is
// expected to invoke once they are done using the client.  It is safe for the
// caller to invoke this close function multiple times.
func (p *Pool) NewClient(name string) (XDSClient, func(), error) {
	return p.newRefCounted(name, defaultWatchExpiryTimeout, backoff.DefaultExponential.Backoff)
}

// NewClientForTesting returns an xDS client configured with the provided
// options from the pool. If the client doesn't already exist, it creates a new
// xDS client and adds it to the pool.
//
// The second return value represents a close function which the caller is
// expected to invoke once they are done using the client.  It is safe for the
// caller to invoke this close function multiple times.
//
// # Testing Only
//
// This function should ONLY be used for testing purposes.
func (p *Pool) NewClientForTesting(opts OptionsForTesting) (XDSClient, func(), error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("xds: opts.Name field must be non-empty")
	}
	if opts.WatchExpiryTimeout == 0 {
		opts.WatchExpiryTimeout = defaultWatchExpiryTimeout
	}
	if opts.StreamBackoffAfterFailure == nil {
		opts.StreamBackoffAfterFailure = defaultStreamBackoffFunc
	}
	return p.newRefCounted(opts.Name, opts.WatchExpiryTimeout, opts.StreamBackoffAfterFailure)
}

// GetClientForTesting returns an xDS client created earlier using the given
// name from the pool. If the client with the given name doesn't already exist,
// it returns an error.
//
// The second return value represents a close function which the caller is
// expected to invoke once they are done using the client.  It is safe for the
// caller to invoke this close function multiple times.
//
// # Testing Only
//
// This function should ONLY be used for testing purposes.
func (p *Pool) GetClientForTesting(name string) (XDSClient, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	c, ok := p.clients[name]
	if !ok {
		return nil, nil, fmt.Errorf("xds:: xDS client with name %q not found", name)
	}
	c.incrRef()
	return c, grpcsync.OnceFunc(func() { p.clientRefCountedClose(name) }), nil
}

// SetFallbackBootstrapConfig is used to specify a bootstrap configuration
// that will be used as a fallback when the bootstrap environment variables
// are not defined.
func (p *Pool) SetFallbackBootstrapConfig(config *bootstrap.Config) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.config = config
}

// DumpResources returns the status and contents of all xDS resources.
func (p *Pool) DumpResources() *v3statuspb.ClientStatusResponse {
	p.mu.Lock()
	defer p.mu.Unlock()

	resp := &v3statuspb.ClientStatusResponse{}
	for key, client := range p.clients {
		cfg := client.dumpResources()
		cfg.ClientScope = key
		resp.Config = append(resp.Config, cfg)
	}
	return resp
}

func (p *Pool) clientRefCountedClose(name string) {
	p.mu.Lock()
	client, ok := p.clients[name]
	if !ok {
		logger.Errorf("Attempt to close a non-existent xDS client with name %s", name)
		p.mu.Unlock()
		return
	}
	if client.decrRef() != 0 {
		p.mu.Unlock()
		return
	}
	delete(p.clients, name)
	p.mu.Unlock()

	// This attempts to close the transport to the management server and could
	// theoretically call back into the xdsclient package again and deadlock.
	// Hence, this needs to be called without holding the lock.
	client.clientImpl.close()
	xdsClientImplCloseHook(name)
}

// newRefCounted creates a new reference counted xDS client implementation for
// name, if one does not exist already. If an xDS client for the given name
// exists, it gets a reference to it and returns it.
func (p *Pool) newRefCounted(name string, watchExpiryTimeout time.Duration, streamBackoff func(int) time.Duration) (XDSClient, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.config == nil {
		return nil, nil, fmt.Errorf("xds: bootstrap configuration not set in the pool")
	}

	if c := p.clients[name]; c != nil {
		c.incrRef()
		return c, grpcsync.OnceFunc(func() { p.clientRefCountedClose(name) }), nil
	}

	c, err := newClientImpl(p.config, watchExpiryTimeout, streamBackoff)
	if err != nil {
		return nil, nil, err
	}
	if logger.V(2) {
		c.logger.Infof("Created client with name %q and bootstrap configuration:\n %s", name, p.config)
	}
	client := &clientRefCounted{clientImpl: c, refCount: 1}
	p.clients[name] = client
	xdsClientImplCreateHook(name)

	logger.Infof("xDS node ID: %s", p.config.Node().GetId())
	return client, grpcsync.OnceFunc(func() { p.clientRefCountedClose(name) }), nil
}