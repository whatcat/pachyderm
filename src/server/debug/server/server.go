package server

import (
	"fmt"
	"runtime/pprof"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/types"
	"github.com/pachyderm/pachyderm/src/client/debug"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/server/worker"
)

const (
	defaultDuration = time.Minute
)

// NewDebugServer creates a new server that serves the debug api over GRPC
func NewDebugServer(name string, etcdClient *etcd.Client, etcdPrefix string, workerGrpcPort uint16) debug.DebugServer {
	return &debugServer{
		name:           name,
		etcdClient:     etcdClient,
		etcdPrefix:     etcdPrefix,
		workerGrpcPort: workerGrpcPort,
	}
}

type debugServer struct {
	name           string
	etcdClient     *etcd.Client
	etcdPrefix     string
	workerGrpcPort uint16
}

func (s *debugServer) Dump(request *debug.DumpRequest, server debug.Debug_DumpServer) error {
	profile := pprof.Lookup("goroutine")
	if profile == nil {
		return fmt.Errorf("unable to find goroutine profile")
	}
	w := grpcutil.NewStreamingBytesWriter(server)
	if s.name != "" {
		if _, err := fmt.Fprintf(w, "== %s ==\n\n", s.name); err != nil {
			return err
		}
	}
	if err := profile.WriteTo(w, 2); err != nil {
		return err
	}
	if !request.Recursed {
		request.Recursed = true
		cs, err := worker.Clients(server.Context(), "", s.etcdClient, s.etcdPrefix, s.workerGrpcPort)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if _, err := fmt.Fprintf(w, "\n"); err != nil {
				return err
			}
			dumpC, err := c.Dump(
				server.Context(),
				request,
			)
			if err != nil {
				return err
			}
			if err := grpcutil.WriteFromStreamingBytesClient(dumpC, w); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *debugServer) Profile(request *debug.ProfileRequest, server debug.Debug_ProfileServer) error {
	w := grpcutil.NewStreamingBytesWriter(server)
	if request.Profile == "cpu" {
		if err := pprof.StartCPUProfile(w); err != nil {
			return err
		}
		duration := defaultDuration
		if request.Duration != nil {
			var err error
			duration, err = types.DurationFromProto(request.Duration)
			if err != nil {
				return err
			}
		}
		time.Sleep(duration)
		pprof.StopCPUProfile()
		return nil
	}
	profile := pprof.Lookup(request.Profile)
	if profile == nil {
		return fmt.Errorf("unable to find profile %q", request.Profile)
	}
	if err := profile.WriteTo(w, 2); err != nil {
		return err
	}
	return nil
}
