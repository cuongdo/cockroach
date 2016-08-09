// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package kv

import (
	"sort"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/internal/client"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/util/envutil"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/rubyist/circuitbreaker"
	"google.golang.org/grpc"
)

// Allow local calls to be dispatched directly to the local server without
// sending an RPC.
var enableLocalCalls = envutil.EnvOrDefaultBool("enable_local_calls", true)

// A SendOptions structure describes the algorithm for sending RPCs to one or
// more replicas, depending on error conditions and how many successful
// responses are required.
type SendOptions struct {
	context.Context // must not be nil
	// SendNextTimeout is the duration after which RPCs are sent to
	// other replicas in a set.
	SendNextTimeout time.Duration
	// Timeout is the maximum duration of an RPC before failure.
	// 0 for no timeout.
	Timeout time.Duration

	transportFactory TransportFactory
}

func (so SendOptions) contextWithTimeout() (context.Context, func()) {
	if so.Timeout != 0 {
		return context.WithTimeout(so.Context, so.Timeout)
	}
	return so.Context, func() {}
}

type batchClient struct {
	remoteAddr string
	conn       *grpc.ClientConn
	client     roachpb.InternalClient
	args       roachpb.BatchRequest
	healthy    bool
}

// BatchCall contains a response and an RPC error (note that the
// response contains its own roachpb.Error, which is separate from
// BatchCall.Err), and is analogous to the net/rpc.Call struct.
type BatchCall struct {
	Reply *roachpb.BatchResponse
	Err   error
}

// TransportFactory encapsulates all interaction with the RPC
// subsystem, allowing it to be mocked out for testing. The factory
// function returns a Transport object which is used to send the given
// arguments to one or more replicas in the slice.
//
// In addition to actually sending RPCs, the transport is responsible
// for ordering replicas in accordance with SendOptions.Ordering and
// transport-specific knowledge such as connection health or latency.
//
// TODO(bdarnell): clean up this crufty interface; it was extracted
// verbatim from the non-abstracted code.
type TransportFactory func(
	SendOptions, *rpc.Context, ReplicaSlice, roachpb.BatchRequest,
) (Transport, error)

// Transport objects can send RPCs to one or more replicas of a range.
// All calls to Transport methods are made from a single thread, so
// Transports are not required to be thread-safe.
type Transport interface {
	// IsExhausted returns true if there are no more replicas to try.
	IsExhausted() bool

	// SendNext sends the rpc (captured at creation time) to the next
	// replica. May panic if the transport is exhausted. Should not
	// block; the transport is responsible for starting other goroutines
	// as needed. Returns the address the RPC was sent to.
	SendNext(chan BatchCall) string

	// Close is called when the transport is no longer needed. It may
	// cancel any pending RPCs without writing any response to the channel.
	Close()
}

type tryNextTransport interface {
	// Prefer TODO: fill this in
	TryNext(desc roachpb.ReplicaDescriptor) error
}

// grpcTransportFactory is the default TransportFactory, using GRPC.
func grpcTransportFactory(
	opts SendOptions,
	rpcContext *rpc.Context,
	replicas ReplicaSlice,
	args roachpb.BatchRequest,
) (Transport, error) {
	clients := make([]batchClient, 0, len(replicas))
	for _, replica := range replicas {
		conn, err := rpcContext.GRPCDial(replica.NodeDesc.Address.String())
		if err != nil {
			if errors.Cause(err) == circuit.ErrBreakerOpen {
				continue
			}
			return nil, err
		}
		argsCopy := args
		argsCopy.Replica = replica.ReplicaDescriptor
		remoteAddr := replica.NodeDesc.Address.String()
		clients = append(clients, batchClient{
			remoteAddr: remoteAddr,
			conn:       conn,
			client:     roachpb.NewInternalClient(conn),
			args:       argsCopy,
			healthy:    rpcContext.IsConnHealthy(remoteAddr),
		})
	}

	// Put known-unhealthy clients last.
	splitHealthy(clients)

	return &grpcTransport{
		opts:              opts,
		rpcContext:        rpcContext,
		orderedClients:    clients,
		allOrderedClients: clients,
	}, nil
}

type grpcTransport struct {
	opts              SendOptions
	rpcContext        *rpc.Context
	orderedClients    []batchClient
	allOrderedClients []batchClient
}

func (gt *grpcTransport) IsExhausted() bool {
	return len(gt.orderedClients) == 0
}

// SendNext invokes the specified RPC on the supplied client when the
// client is ready. On success, the reply is sent on the channel;
// otherwise an error is sent. Returns the address the RPC was sent to.
func (gt *grpcTransport) SendNext(done chan BatchCall) string {
	client := gt.orderedClients[0]
	gt.orderedClients = gt.orderedClients[1:]

	addr := client.remoteAddr
	if log.V(2) {
		log.Infof(gt.opts.Context, "sending request to %s: %+v", addr, client.args)
	}

	if localServer := gt.rpcContext.GetLocalInternalServerForAddr(addr); enableLocalCalls && localServer != nil {
		ctx, cancel := gt.opts.contextWithTimeout()
		log.Trace(ctx, "executing local RPC")
		defer cancel()

		reply, err := localServer.Batch(ctx, &client.args)
		done <- BatchCall{Reply: reply, Err: err}
		return addr
	}

	go func() {
		ctx, cancel := gt.opts.contextWithTimeout()
		log.Tracef(ctx, "sending RPC to %s", addr)
		defer cancel()
		reply, err := client.client.Batch(ctx, &client.args)
		if reply != nil {
			for i := range reply.Responses {
				if err := reply.Responses[i].GetInner().Verify(client.args.Requests[i].GetInner()); err != nil {
					log.Error(ctx, err)
				}
			}
		}
		done <- BatchCall{Reply: reply, Err: err}
	}()

	return addr
}

func (gt *grpcTransport) TryNext(replica roachpb.ReplicaDescriptor) error {
	if gt.IsExhausted() {
		return errors.New("transport is exhausted")
	}
	if replica.ReplicaID == 0 {
		return errors.New("new leader is <nil>")
	}

	// The client was going to be tried later, so we move it up to the head of
	// the slice.
	for i, c := range gt.orderedClients {
		if c.args.Replica == replica {
			// Move the client to the beginning of the slice.
			oc := make([]batchClient, len(gt.orderedClients))
			oc = append(oc, gt.orderedClients[i])
			oc = append(oc, gt.orderedClients[:i]...)
			oc = append(oc, gt.orderedClients[i+1:]...)
			log.Info(context.TODO(), "TryNext: found replica")
			return nil
		}
	}

	// A client we've already tried has been passed in. So, we try it again. To
	// prevent excessive retries, we eliminate the least preferred client.
	for _, c := range gt.allOrderedClients {
		if c.args.Replica == replica {
			gt.orderedClients = append([]batchClient{c}, gt.orderedClients[:len(gt.orderedClients)-1]...)
			log.Info(context.TODO(), "TryNext: found replica 2")
			return nil
		}
	}

	// There is no existing client for the given replica, and we don't have
	// access to gossip for doing a replica -> Node ID -> address lookup needed to
	// issue an RPC. So, we can't do anything useful here. Hopefully, a successive
	// retry at an upper layer will allow this RPC to succeed.
	return errors.Errorf("couldn't find client for replica %s", replica)
}

func (*grpcTransport) Close() {
	// TODO(bdarnell): Save the cancel functions of all pending RPCs and
	// call them here. (it's fine to ignore them for now since they'll
	// time out anyway)
}

// splitHealthy splits the provided client slice into healthy clients and
// unhealthy clients, based on their connection state. Healthy clients will
// be rearranged first in the slice, and unhealthy clients will be rearranged
// last. Within these two groups, the rearrangement will be stable. The function
// will then return the number of healthy clients.
func splitHealthy(clients []batchClient) int {
	var nHealthy int
	sort.Stable(byHealth(clients))
	for _, client := range clients {
		if client.healthy {
			nHealthy++
		}
	}
	return nHealthy
}

// byHealth sorts a slice of batchClients by their health with healthy first.
type byHealth []batchClient

func (h byHealth) Len() int           { return len(h) }
func (h byHealth) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h byHealth) Less(i, j int) bool { return h[i].healthy && !h[j].healthy }

// SenderTransportFactory wraps a client.Sender for use as a KV
// Transport. This is useful for tests that want to use DistSender
// without a full RPC stack.
func SenderTransportFactory(tracer opentracing.Tracer, sender client.Sender) TransportFactory {
	return func(
		_ SendOptions, _ *rpc.Context, _ ReplicaSlice, args roachpb.BatchRequest,
	) (Transport, error) {
		return &senderTransport{tracer, sender, args, false}, nil
	}
}

type senderTransport struct {
	tracer opentracing.Tracer
	sender client.Sender
	args   roachpb.BatchRequest

	called bool
}

func (s *senderTransport) IsExhausted() bool {
	return s.called
}

func (s *senderTransport) SendNext(done chan BatchCall) string {
	if s.called {
		panic("called an exhausted transport")
	}
	s.called = true
	sp := s.tracer.StartSpan("node")
	defer sp.Finish()
	ctx := opentracing.ContextWithSpan(context.Background(), sp)
	log.Trace(ctx, s.args.String())
	br, pErr := s.sender.Send(ctx, s.args)
	if br == nil {
		br = &roachpb.BatchResponse{}
	}
	if br.Error != nil {
		panic(roachpb.ErrorUnexpectedlySet(s.sender, br))
	}
	br.Error = pErr
	if pErr != nil {
		log.Trace(ctx, "error: "+pErr.String())
	}
	done <- BatchCall{Reply: br}
	return ""
}

func (s *senderTransport) Close() {
}
