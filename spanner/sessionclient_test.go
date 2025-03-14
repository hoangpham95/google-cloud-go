/*
Copyright 2019 Google LLC

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

package spanner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	vkit "cloud.google.com/go/spanner/apiv1"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	. "cloud.google.com/go/spanner/internal/testutil"
	"github.com/GoogleCloudPlatform/grpc-gcp-go/grpcgcp"
	"github.com/GoogleCloudPlatform/grpc-gcp-go/grpcgcp/grpc_gcp"
	"github.com/GoogleCloudPlatform/grpc-gcp-go/grpcgcp/multiendpoint"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type testSessionCreateError struct {
	err error
	num int32
}

type testConsumer struct {
	numExpected int32

	mu       sync.Mutex
	sessions []*session
	errors   []*testSessionCreateError
	numErr   int32

	receivedAll chan struct{}
}

func (tc *testConsumer) sessionReady(_ context.Context, s *session) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.sessions = append(tc.sessions, s)
	tc.checkReceivedAll()
}

func (tc *testConsumer) sessionCreationFailed(_ context.Context, err error, num int32, _ bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.errors = append(tc.errors, &testSessionCreateError{
		err: err,
		num: num,
	})
	tc.numErr += num
	tc.checkReceivedAll()
}

func (tc *testConsumer) checkReceivedAll() {
	if int32(len(tc.sessions))+tc.numErr == tc.numExpected {
		close(tc.receivedAll)
	}
}

func newTestConsumer(numExpected int32) *testConsumer {
	return &testConsumer{
		numExpected: numExpected,
		receivedAll: make(chan struct{}),
	}
}

func TestNextClient(t *testing.T) {
	t.Parallel()
	if useGRPCgcp {
		// For GCPMultiEndpoint the nextClient is indirectly tested via TestBatchCreateAndCloseSession.
		t.Skip("GCPMultiEndpoint does not provide a connection via Connection().")
	}

	n := 4
	_, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{
		DisableNativeMetrics: true,
		NumChannels:          n,
		SessionPoolConfig: SessionPoolConfig{
			MinOpened: 0,
			MaxOpened: 100,
		},
	})
	defer teardown()
	sc := client.idleSessions.sc
	connections := make(map[*grpc.ClientConn]int)
	for i := 0; i < n; i++ {
		client, err := sc.nextClient()
		if err != nil {
			t.Fatalf("Error getting a gapic client from the session client\nGot: %v", err)
		}
		conn1 := client.Connection()
		conn2 := client.Connection()
		if conn1 != conn2 {
			t.Fatalf("Client connection mismatch. Expected to get two equal connections.\nGot: %v and %v", conn1, conn2)
		}
		if index, ok := connections[conn1]; ok {
			t.Fatalf("Same connection used multiple times for different clients.\nClient 1: %v\nClient 2: %v", index, i)
		}
		connections[conn1] = i
	}
	// Pass through all the clients once more. This time the exact same
	// connections should be found.
	for i := 0; i < n; i++ {
		client, err := sc.nextClient()
		if err != nil {
			t.Fatalf("Error getting a gapic client from the session client\nGot: %v", err)
		}
		conn := client.Connection()
		if _, ok := connections[conn]; !ok {
			t.Fatalf("Connection not found for index %v", i)
		}
	}
}

func TestCreateAndCloseSession(t *testing.T) {
	t.Parallel()

	server, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{
		DisableNativeMetrics: true,
		SessionPoolConfig: SessionPoolConfig{
			MinOpened: 0,
			MaxOpened: 100,
		},
	})
	defer teardown()

	s, err := client.sc.createSession(context.Background())
	if err != nil {
		t.Fatalf("batch.next() return error mismatch\ngot: %v\nwant: nil", err)
	}
	if s == nil {
		t.Fatalf("batch.next() return value mismatch\ngot: %v\nwant: any session", s)
	}
	expectedCount := uint(1)
	if isMultiplexEnabled {
		expectedCount = 2
	}
	if server.TestSpanner.TotalSessionsCreated() != expectedCount {
		t.Fatalf("number of sessions created mismatch\ngot: %v\nwant: %v", server.TestSpanner.TotalSessionsCreated(), 1)
	}
	s.delete(context.Background())
	if server.TestSpanner.TotalSessionsDeleted() != 1 {
		t.Fatalf("number of sessions deleted mismatch\ngot: %v\nwant: %v", server.TestSpanner.TotalSessionsDeleted(), 1)
	}
}

func TestCreateSessionWithDatabaseRole(t *testing.T) {
	// Make sure that there is always only one session in the pool.
	sc := SessionPoolConfig{
		MinOpened: 0,
		MaxOpened: 1,
	}
	server, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{DisableNativeMetrics: true, SessionPoolConfig: sc, DatabaseRole: "test"})
	defer teardown()
	ctx := context.Background()

	s, err := client.sc.createSession(ctx)
	if err != nil {
		t.Fatalf("batch.next() return error mismatch\ngot: %v\nwant: nil", err)
	}
	if s == nil {
		t.Fatalf("batch.next() return value mismatch\ngot: %v\nwant: any session", s)
	}
	expectedCount := uint(1)
	if isMultiplexEnabled {
		expectedCount = 2
	}
	if g, w := server.TestSpanner.TotalSessionsCreated(), expectedCount; g != w {
		t.Fatalf("number of sessions created mismatch\ngot: %v\nwant: %v", g, w)
	}

	resp, err := server.TestSpanner.GetSession(ctx, &sppb.GetSessionRequest{Name: s.id})
	if err != nil {
		t.Fatalf("Failed to get session unexpectedly: %v", err)
	}
	if g, w := resp.CreatorRole, "test"; g != w {
		t.Fatalf("database role mismatch.\nGot: %v\nWant: %v", g, w)
	}

	s.delete(ctx)
	if g, w := server.TestSpanner.TotalSessionsDeleted(), uint(1); g != w {
		t.Fatalf("number of sessions deleted mismatch\ngot: %v\nwant: %v", g, w)
	}
}

func TestBatchCreateAndCloseSession(t *testing.T) {
	t.Parallel()

	numSessions := int32(100)

	// Remembering which connections were used for the calls.
	reqConnAddr := make(chan string, 200)
	defer close(reqConnAddr)
	connTagger := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		p, ok := peer.FromContext(ctx)
		if ok {
			reqConnAddr <- p.Addr.String()
		} else {
			reqConnAddr <- ""
		}
		return handler(ctx, req)
	}
	sopt := []grpc.ServerOption{grpc.ChainUnaryInterceptor(connTagger)}
	server, opts, serverTeardown := NewMockedSpannerInMemTestServer(t, sopt...)
	defer serverTeardown()
	for numChannels := 1; numChannels <= 32; numChannels *= 2 {
		prevCreated := server.TestSpanner.TotalSessionsCreated()
		prevDeleted := server.TestSpanner.TotalSessionsDeleted()
		config := ClientConfig{
			DisableNativeMetrics: true,
			NumChannels:          numChannels,
			SessionPoolConfig: SessionPoolConfig{
				MinOpened: 0,
				MaxOpened: 400,
			}}
		var client *Client
		var err error
		if useGRPCgcp {
			gmeCfg := &grpcgcp.GCPMultiEndpointOptions{
				GRPCgcpConfig: &grpc_gcp.ApiConfig{
					ChannelPool: &grpc_gcp.ChannelPoolConfig{
						MaxSize:          uint32(numChannels),
						MinSize:          uint32(numChannels),
						BindPickStrategy: grpc_gcp.ChannelPoolConfig_ROUND_ROBIN,
					},
				},
				MultiEndpoints: map[string]*multiendpoint.MultiEndpointOptions{
					"default": {
						Endpoints: []string{server.ServerAddress},
					},
				},
				Default: "default",
			}
			client, _, err = NewMultiEndpointClientWithConfig(context.Background(), "projects/p/instances/i/databases/d", config, gmeCfg, opts...)
		} else {
			client, err = NewClientWithConfig(context.Background(), "projects/p/instances/i/databases/d", config, opts...)
		}
		if err != nil {
			t.Fatal(err)
		}

		if isMultiplexEnabled {
			waitFor(t, func() error {
				client.idleSessions.mu.Lock()
				defer client.idleSessions.mu.Unlock()
				if client.idleSessions.multiplexedSession == nil {
					return errors.New("multiplexed session not created yet")
				}
				return nil
			})
		}

		consumer := newTestConsumer(numSessions)
		client.sc.batchCreateSessions(numSessions, true, consumer)
		<-consumer.receivedAll
		if len(consumer.sessions) != int(numSessions) {
			t.Fatalf("returned number of sessions mismatch\ngot: %v\nwant: %v", len(consumer.sessions), numSessions)
		}
		created := server.TestSpanner.TotalSessionsCreated() - prevCreated
		expectedNumSessions := numSessions
		if isMultiplexEnabled {
			expectedNumSessions++
		}
		if created != uint(expectedNumSessions) {
			t.Fatalf("number of sessions created mismatch\ngot: %v\nwant: %v", created, expectedNumSessions)
		}
		// Check that all channels are used evenly.
		channelCounts := make(map[spannerClient]int32)
		for _, s := range consumer.sessions {
			channelCounts[s.client]++
		}
		if len(channelCounts) != numChannels {
			t.Fatalf("number of channels used mismatch\ngot: %v\nwant: %v", len(channelCounts), numChannels)
		}
		for _, c := range channelCounts {
			if c < numSessions/int32(numChannels) || c > numSessions/int32(numChannels)+(numSessions%int32(numChannels)) {
				t.Fatalf("channel used an unexpected number of times\ngot: %v\nwant between %v and %v", c, numSessions/int32(numChannels), numSessions/int32(numChannels)+1)
			}
		}
		// Check that all connections are used evenly. Required for GCPMultiEndpoint.
		reqConnAddr <- "ALLRECEIVED"
		connCounts := make(map[string]int32)
		addr := <-reqConnAddr
		for addr != "ALLRECEIVED" {
			connCounts[addr]++
			addr = <-reqConnAddr
		}
		if len(connCounts) != numChannels {
			t.Fatalf("number of connections used mismatch\ngot: %v\nwant: %v", len(connCounts), numChannels)
		}
		for a, c := range connCounts {
			if c != 1 {
				if isMultiplexEnabled {
					// The multiplexed session creation will use one of the connections
					if c != 2 {
						t.Fatalf("connection %q used an unexpected number of times\ngot: %v\nwant %v", a, c, 2)
					}
				} else {
					t.Fatalf("connection %q used an unexpected number of times\ngot: %v\nwant %v", a, c, 1)
				}

			}
		}
		// Delete the sessions.
		for _, s := range consumer.sessions {
			s.delete(context.Background())
		}
		deleted := server.TestSpanner.TotalSessionsDeleted() - prevDeleted
		if deleted != uint(numSessions) {
			t.Fatalf("number of sessions deleted mismatch\ngot: %v\nwant %v", deleted, numSessions)
		}
		client.Close()
		// Flush addresses used while deleting sessions.
		reqConnAddr <- "ALLDELETED"
		addr = <-reqConnAddr
		for addr != "ALLDELETED" {
			addr = <-reqConnAddr
		}
	}
}

func TestBatchCreateSessionsWithDatabaseRole(t *testing.T) {
	// Make sure that there is always only one session in the pool.
	sc := SessionPoolConfig{
		MinOpened: 0,
		MaxOpened: 1,
	}
	server, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{DisableNativeMetrics: true, SessionPoolConfig: sc, DatabaseRole: "test"})
	defer teardown()

	ctx := context.Background()

	consumer := newTestConsumer(1)
	client.sc.batchCreateSessions(1, true, consumer)
	<-consumer.receivedAll
	if g, w := len(consumer.sessions), 1; g != w {
		t.Fatalf("returned number of sessions mismatch\ngot: %v\nwant: %v", g, w)
	}
	expectedCount := uint(1)
	if isMultiplexEnabled {
		expectedCount = 2
	}
	if g, w := server.TestSpanner.TotalSessionsCreated(), expectedCount; g != w {
		t.Fatalf("number of sessions created mismatch\ngot: %v\nwant: %v", g, w)
	}
	s := consumer.sessions[0]

	resp, err := server.TestSpanner.GetSession(ctx, &sppb.GetSessionRequest{Name: s.id})
	if err != nil {
		t.Fatalf("Failed to get session unexpectedly: %v", err)
	}
	if g, w := resp.CreatorRole, "test"; g != w {
		t.Fatalf("database role mismatch.\nGot: %v\nWant: %v", g, w)
	}

	s.delete(ctx)
	if g, w := server.TestSpanner.TotalSessionsDeleted(), uint(1); g != w {
		t.Fatalf("number of sessions deleted mismatch\ngot: %v\nwant: %v", g, w)
	}
}

func TestBatchCreateSessionsWithExceptions(t *testing.T) {
	t.Parallel()

	numSessions := int32(100)
	server, opts, serverTeardown := NewMockedSpannerInMemTestServer(t)
	defer serverTeardown()

	// Run the test with everything between 1 and numChannels errors.
	for numErrors := int32(1); numErrors <= numChannels; numErrors++ {
		// Make sure that the error is not always the first call.
		for firstErrorAt := numErrors - 1; firstErrorAt < numChannels-numErrors+1; firstErrorAt++ {
			config := ClientConfig{
				DisableNativeMetrics: true,
				NumChannels:          numChannels,
				SessionPoolConfig: SessionPoolConfig{
					MinOpened: 0,
					MaxOpened: 400,
				}}
			client, err := makeClientWithConfig(context.Background(), "projects/p/instances/i/databases/d", config, server.ServerAddress, opts...)
			if err != nil {
				t.Fatal(err)
			}
			// Register the errors on the server.
			errors := make([]error, numErrors+firstErrorAt)
			for i := firstErrorAt; i < numErrors+firstErrorAt; i++ {
				errors[i] = status.Errorf(codes.FailedPrecondition, "session creation failed")
			}
			server.TestSpanner.PutExecutionTime(MethodBatchCreateSession, SimulatedExecutionTime{
				Errors: errors,
			})
			consumer := newTestConsumer(numSessions)
			client.sc.batchCreateSessions(numSessions, true, consumer)
			<-consumer.receivedAll

			sessionsReturned := int32(len(consumer.sessions))
			if int32(len(consumer.errors)) != numErrors {
				t.Fatalf("Error count mismatch\nGot: %d\nWant: %d", len(consumer.errors), numErrors)
			}
			for _, e := range consumer.errors {
				if g, w := status.Code(e.err), codes.FailedPrecondition; g != w {
					t.Fatalf("error code mismatch\ngot: %v\nwant: %v", g, w)
				}
			}
			maxExpectedSessions := numSessions - numErrors*(numSessions/numChannels)
			minExpectedSessions := numSessions - numErrors*(numSessions/numChannels+1)
			if sessionsReturned < minExpectedSessions || sessionsReturned > maxExpectedSessions {
				t.Fatalf("session count mismatch\ngot: %v\nwant between %v and %v", sessionsReturned, minExpectedSessions, maxExpectedSessions)
			}
			client.Close()
		}
	}
}

func TestBatchCreateSessions_ServerReturnsLessThanRequestedSessions(t *testing.T) {
	t.Parallel()

	numChannels := 4
	server, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{
		DisableNativeMetrics: true,
		NumChannels:          numChannels,
		SessionPoolConfig: SessionPoolConfig{
			MinOpened: 0,
			MaxOpened: 100,
		},
	})
	defer teardown()
	// Ensure that the server will never return more than 10 sessions per batch
	// create request.
	server.TestSpanner.SetMaxSessionsReturnedByServerPerBatchRequest(10)
	numSessions := int32(100)
	// Request a batch of sessions that is larger than will be returned by the
	// server in one request. The server will return at most 10 sessions per
	// request. The sessionCreator will spread these requests over the 4
	// channels that are available, i.e. do requests for 25 sessions in each
	// request. The batch should still return 100 sessions.
	consumer := newTestConsumer(numSessions)
	client.sc.batchCreateSessions(numSessions, true, consumer)
	<-consumer.receivedAll
	if len(consumer.errors) > 0 {
		t.Fatalf("Error count mismatch\nGot: %d\nWant: %d", len(consumer.errors), 0)
	}
	returnedSessionCount := int32(len(consumer.sessions))
	if returnedSessionCount != numSessions {
		t.Fatalf("Returned sessions mismatch\nGot: %v\nWant: %v", returnedSessionCount, numSessions)
	}
}

func TestBatchCreateSessions_ServerExhausted(t *testing.T) {
	t.Parallel()

	numChannels := 4
	server, client, teardown := setupMockedTestServerWithConfig(t, ClientConfig{
		DisableNativeMetrics: true,
		NumChannels:          numChannels,
		SessionPoolConfig: SessionPoolConfig{
			MinOpened: 0,
			MaxOpened: 100,
		},
	})
	defer teardown()
	if isMultiplexEnabled {
		waitFor(t, func() error {
			if client.idleSessions.multiplexedSession == nil {
				return errors.New("multiplexed session not created yet")
			}
			return nil
		})
	}
	numSessions := int32(100)
	maxSessions := int32(50)
	// Ensure that the server will never return more than 50 sessions in total.
	if isMultiplexEnabled {
		server.TestSpanner.SetMaxSessionsReturnedByServerInTotal(maxSessions + 1)
	} else {
		server.TestSpanner.SetMaxSessionsReturnedByServerInTotal(maxSessions)
	}
	consumer := newTestConsumer(numSessions)
	client.sc.batchCreateSessions(numSessions, true, consumer)
	<-consumer.receivedAll
	// Session creation should end with at least one non-retryable error.
	if len(consumer.errors) == 0 {
		t.Fatalf("Error count mismatch\nGot: %d\nWant: > %d", len(consumer.errors), 0)
	}
	for _, e := range consumer.errors {
		if g, w := status.Code(e.err), codes.OutOfRange; g != w {
			t.Fatalf("Error code mismath\nGot: %v\nWant: %v", g, w)
		}
	}
	// The number of returned sessions should be equal to the max of the
	// server.
	returnedSessionCount := int32(len(consumer.sessions))
	if returnedSessionCount != maxSessions {
		t.Fatalf("Returned sessions mismatch\nGot: %v\nWant: %v", returnedSessionCount, maxSessions)
	}
	if consumer.numErr != (numSessions - maxSessions) {
		t.Fatalf("Num errored sessions mismatch\nGot: %v\nWant: %v", consumer.numErr, numSessions-maxSessions)
	}
}

func TestBatchCreateSessions_WithTimeout(t *testing.T) {
	t.Parallel()

	numSessions := int32(100)
	server, opts, serverTeardown := NewMockedSpannerInMemTestServer(t)
	defer serverTeardown()
	server.TestSpanner.PutExecutionTime(MethodBatchCreateSession, SimulatedExecutionTime{
		MinimumExecutionTime: time.Second,
	})
	config := ClientConfig{
		DisableNativeMetrics: true,
		SessionPoolConfig: SessionPoolConfig{
			MinOpened: 0,
			MaxOpened: 400,
		}}
	client, err := makeClientWithConfig(context.Background(), "projects/p/instances/i/databases/d", config, server.ServerAddress, opts...)
	if err != nil {
		t.Fatal(err)
	}

	client.sc.batchTimeout = 10 * time.Millisecond
	consumer := newTestConsumer(numSessions)
	client.sc.batchCreateSessions(numSessions, true, consumer)
	<-consumer.receivedAll
	if len(consumer.sessions) > 0 {
		t.Fatalf("Returned number of sessions mismatch\ngot: %v\nwant: %v", len(consumer.sessions), 0)
	}
	if len(consumer.errors) != numChannels {
		t.Fatalf("Returned number of errors mismatch\ngot: %v\nwant: %v", len(consumer.errors), numChannels)
	}
	for _, e := range consumer.errors {
		if g, w := status.Code(e.err), codes.DeadlineExceeded; g != w {
			t.Fatalf("Error code mismatch\ngot: %v (%s)\nwant: %v", g, e.err, w)
		}
	}
	client.Close()
}

func TestClientIDGenerator(t *testing.T) {
	cidGen = newClientIDGenerator()
	for _, tt := range []struct {
		database string
		clientID string
	}{
		{"db", "client-1"},
		{"db-new", "client-1"},
		{"db", "client-2"},
	} {
		if got, want := cidGen.nextID(tt.database), tt.clientID; got != want {
			t.Fatalf("Generate wrong client ID: got %v, want %v", got, want)
		}
	}
}

func TestMergeCallOptions(t *testing.T) {
	a := &vkit.CallOptions{
		CreateSession: []gax.CallOption{
			gax.WithRetry(func() gax.Retryer {
				return gax.OnCodes([]codes.Code{
					codes.Unavailable, codes.DeadlineExceeded,
				}, gax.Backoff{
					Initial:    100 * time.Millisecond,
					Max:        16000 * time.Millisecond,
					Multiplier: 1.0,
				})
			}),
		},
		GetSession: []gax.CallOption{
			gax.WithRetry(func() gax.Retryer {
				return gax.OnCodes([]codes.Code{
					codes.Unavailable, codes.DeadlineExceeded,
				}, gax.Backoff{
					Initial:    250 * time.Millisecond,
					Max:        32000 * time.Millisecond,
					Multiplier: 1.30,
				})
			}),
		},
	}
	b := &vkit.CallOptions{
		CreateSession: []gax.CallOption{
			gax.WithRetry(func() gax.Retryer {
				return gax.OnCodes([]codes.Code{
					codes.Unavailable,
				}, gax.Backoff{
					Initial:    250 * time.Millisecond,
					Max:        32000 * time.Millisecond,
					Multiplier: 1.30,
				})
			}),
		},
		BatchCreateSessions: []gax.CallOption{
			gax.WithRetry(func() gax.Retryer {
				return gax.OnCodes([]codes.Code{
					codes.Unavailable,
				}, gax.Backoff{
					Initial:    250 * time.Millisecond,
					Max:        32000 * time.Millisecond,
					Multiplier: 1.30,
				})
			}),
		}}

	merged := mergeCallOptions(b, a)
	cs := &gax.CallSettings{}
	// We can't access the fields of Retryer so we have test the result by
	// comparing strings.
	merged.CreateSession[0].Resolve(cs)
	if got, want := fmt.Sprintf("%v", cs.Retry()), "&{{250000000 32000000000 1.3 0} [14]}"; got != want {
		t.Fatalf("merged CallOptions is incorrect: got %v, want %v", got, want)
	}

	merged.CreateSession[1].Resolve(cs)
	if got, want := fmt.Sprintf("%v", cs.Retry()), "&{{100000000 16000000000 1 0} [14 4]}"; got != want {
		t.Fatalf("merged CallOptions is incorrect: got %v, want %v", got, want)
	}

	merged.GetSession[0].Resolve(cs)
	if got, want := fmt.Sprintf("%v", cs.Retry()), "&{{250000000 32000000000 1.3 0} [14 4]}"; got != want {
		t.Fatalf("merged CallOptions is incorrect: got %v, want %v", got, want)
	}

	merged.BatchCreateSessions[0].Resolve(cs)
	if got, want := fmt.Sprintf("%v", cs.Retry()), "&{{250000000 32000000000 1.3 0} [14]}"; got != want {
		t.Fatalf("merged CallOptions is incorrect: got %v, want %v", got, want)
	}
}
