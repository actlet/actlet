package sdk_test

import (
	"context"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// An orchestrator embeds the gateway in its own gRPC server: it decides what
// each registering worker runs, and keeps the activity they publish.
func ExampleNewServer() {
	store, err := sdk.NewRecordingStore("/var/lib/orchestrator/activity")
	if err != nil {
		log.Fatal(err)
	}
	gw, err := sdk.NewServer(sdk.Options{
		Authenticate: sdk.StaticToken("change-me"),
		Assign: func(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			// req.SessionId is set when a worker resumes a session; keep it.
			return &gatewayv0.RegisterResponse{
				SessionId: req.GetSessionId(),
				Profile: &gatewayv0.Profile{
					Id: "default", Version: "1",
					DefaultPauseMode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN,
					StopGrace:        durationpb.New(30 * time.Second),
				},
				Task: &gatewayv0.Task{Schema: "v0", Id: "task-1", Kind: "demo", Definition: []byte(`{"steps":3}`)},
			}, nil
		},
		Store: store,
		OnActivity: func(sessionID string, ev *gatewayv0.ActivityEvent) {
			log.Printf("%s %s: %v", sessionID, ev.GetId(), ev.GetBody())
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	gs := grpc.NewServer(sdk.ServerOptions()...)
	gw.RegisterService(gs)
	lis, err := net.Listen("tcp", ":7400")
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(gs.Serve(lis))
}

// Control commands wait for the worker's acknowledgement. Pausing needs a
// mode; the worker then persists and exits, and the session resumes when a
// worker is started again on the same session folder.
func ExampleSession_Pause() {
	var gw *sdk.Server // from sdk.NewServer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, sess := range gw.Sessions() {
		ack, err := sess.Pause(ctx, gatewayv0.PauseMode_PAUSE_MODE_DRAIN)
		if err != nil {
			log.Printf("%s: %v", sess.ID(), err)
			continue
		}
		log.Printf("%s: %v", sess.ID(), ack.GetResult())
	}
}
