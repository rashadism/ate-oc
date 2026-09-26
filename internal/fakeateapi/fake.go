// Package fakeateapi is an in-memory ateapi.Control server that mirrors the
// observable semantics of Substrate's server (status codes, state transitions,
// uid/version preconditions) for the RPCs the operator uses.
package fakeateapi

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/uuid"

	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

type key struct{ atespace, name string }

type Server struct {
	pb.UnimplementedControlServer

	mu         sync.Mutex
	atespaces  map[string]*pb.Atespace
	templates  map[key]*pb.ActorTemplate
	actors     map[key]*pb.Actor
	egress     map[key]*pb.EgressPolicy
	sandboxes  map[string]pb.SandboxClass
	failNext   map[string]codes.Code
	autoGolden bool
}

type Option func(*Server)

// WithAutoGolden marks every new template's golden snapshot ready on creation.
func WithAutoGolden() Option { return func(s *Server) { s.autoGolden = true } }

// WithSandboxConfig registers a SandboxConfig; templates must reference one.
func WithSandboxConfig(name string, class pb.SandboxClass) Option {
	return func(s *Server) { s.sandboxes[name] = class }
}

func New(opts ...Option) *Server {
	s := &Server{
		atespaces: map[string]*pb.Atespace{},
		templates: map[key]*pb.ActorTemplate{},
		actors:    map[key]*pb.Actor{},
		egress:    map[key]*pb.EgressPolicy{},
		sandboxes: map[string]pb.SandboxClass{},
		failNext:  map[string]codes.Code{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start serves s over bufconn and returns a connected client.
func Start(t testing.TB, s *Server, opts ...grpc.DialOption) pb.ControlClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterControlServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); gs.Stop() })
	return pb.NewControlClient(conn)
}

// FailNext makes the next call to method (e.g. "SuspendActor") fail with code.
func (s *Server) FailNext(method string, code codes.Code) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = code
}

func (s *Server) injected(method string) error {
	if c, ok := s.failNext[method]; ok {
		delete(s.failNext, method)
		return status.Errorf(c, "injected %s", c)
	}
	return nil
}

// SetGolden completes a template's golden snapshot, successfully or with errMsg.
func (s *Server) SetGolden(atespace, name, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.templates[key{atespace, name}]
	if t == nil {
		return
	}
	setGolden(t, errMsg)
}

func setGolden(t *pb.ActorTemplate, errMsg string) {
	gs := &pb.GoldenSnapshotStatus{}
	if errMsg != "" {
		gs.ErrorMessage = errMsg
	} else {
		gs.GoldenTag = &pb.ObjectRef{Atespace: "ate-golden", Name: t.GetMetadata().GetUid()}
	}
	t.Status = &pb.ActorTemplateStatus{GoldenSnapshotStatus: gs}
}

// SetActorState forces an actor into state, e.g. RUNNING or CRASHED.
func (s *Server) SetActorState(atespace, name string, state pb.ActorState, crashMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.actors[key{atespace, name}]
	if a == nil {
		return
	}
	a.Status.State = state
	a.Status.Crash = nil
	if state == pb.ActorState_ACTOR_STATE_CRASHED {
		a.Status.Crash = &pb.ActorCrash{Message: crashMsg, CrashTime: timestamppb.Now()}
	}
	bump(a.Metadata)
}

func newMeta(in *pb.ResourceMetadata) *pb.ResourceMetadata {
	now := timestamppb.Now()
	return &pb.ResourceMetadata{
		Atespace:   in.GetAtespace(),
		Name:       in.GetName(),
		Uid:        string(uuid.NewUUID()),
		Version:    1,
		CreateTime: now,
		UpdateTime: now,
	}
}

func bump(m *pb.ResourceMetadata) {
	m.Version++
	m.UpdateTime = timestamppb.Now()
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

func checkUpdate(stored, in *pb.ResourceMetadata) error {
	if in.GetUid() == "" || in.GetVersion() == 0 {
		return status.Error(codes.InvalidArgument, "metadata.uid and metadata.version are required")
	}
	if in.GetUid() != stored.GetUid() || in.GetVersion() != stored.GetVersion() {
		return status.Error(codes.Aborted, "concurrent update conflict, please retry")
	}
	return nil
}

func checkDelete(stored *pb.ResourceMetadata, opts *pb.DeleteOptions) error {
	if u := opts.GetUid(); u != "" && u != stored.GetUid() {
		return status.Error(codes.Aborted, "uid precondition failed")
	}
	if v := opts.GetVersion(); v != 0 && v != stored.GetVersion() {
		return status.Error(codes.Aborted, "version precondition failed")
	}
	return nil
}

func (s *Server) CreateAtespace(_ context.Context, req *pb.CreateAtespaceRequest) (*pb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("CreateAtespace"); err != nil {
		return nil, err
	}
	name := req.GetAtespace().GetMetadata().GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	if _, ok := s.atespaces[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "atespace %s already exists", name)
	}
	as := &pb.Atespace{Metadata: newMeta(&pb.ResourceMetadata{Name: name})}
	s.atespaces[name] = as
	return clone(as), nil
}

func (s *Server) GetAtespace(_ context.Context, req *pb.GetAtespaceRequest) (*pb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("GetAtespace"); err != nil {
		return nil, err
	}
	name := req.GetAtespace().GetName()
	as, ok := s.atespaces[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "atespace %s not found", name)
	}
	return clone(as), nil
}

func (s *Server) CreateActorTemplate(_ context.Context, req *pb.CreateActorTemplateRequest) (*pb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("CreateActorTemplate"); err != nil {
		return nil, err
	}
	in := req.GetActorTemplate()
	k := key{in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()}
	if k.atespace == "" || k.name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.atespace and metadata.name are required")
	}
	sc := in.GetSandboxConfig()
	if class, ok := s.sandboxes[sc.GetConfigName()]; !ok || class != sc.GetSandboxClass() {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox config %q (class %s) not found", sc.GetConfigName(), sc.GetSandboxClass())
	}
	if _, ok := s.templates[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor template %s/%s already exists", k.atespace, k.name)
	}
	if _, ok := s.atespaces[k.atespace]; !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "atespace %s not found", k.atespace)
	}
	t := clone(in)
	t.Metadata = newMeta(in.GetMetadata())
	t.Status = &pb.ActorTemplateStatus{}
	if s.autoGolden {
		setGolden(t, "")
	}
	s.templates[k] = t
	return clone(t), nil
}

func (s *Server) GetActorTemplate(_ context.Context, req *pb.GetActorTemplateRequest) (*pb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("GetActorTemplate"); err != nil {
		return nil, err
	}
	ref := req.GetActorTemplate()
	t, ok := s.templates[key{ref.GetAtespace(), ref.GetName()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor template %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	return clone(t), nil
}

func (s *Server) ListActorTemplates(_ context.Context, req *pb.ListActorTemplatesRequest) (*pb.ListActorTemplatesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("ListActorTemplates"); err != nil {
		return nil, err
	}
	resp := &pb.ListActorTemplatesResponse{}
	for k, t := range s.templates {
		if req.GetAtespace() == "" || req.GetAtespace() == k.atespace {
			resp.ActorTemplates = append(resp.ActorTemplates, clone(t))
		}
	}
	return resp, nil
}

// DeleteActorTemplate does not check for referencing actors, like the real server.
func (s *Server) DeleteActorTemplate(_ context.Context, req *pb.DeleteActorTemplateRequest) (*pb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("DeleteActorTemplate"); err != nil {
		return nil, err
	}
	k := key{req.GetActorTemplate().GetAtespace(), req.GetActorTemplate().GetName()}
	t, ok := s.templates[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor template %s/%s not found", k.atespace, k.name)
	}
	if err := checkDelete(t.GetMetadata(), req.GetOptions()); err != nil {
		return nil, err
	}
	delete(s.templates, k)
	return t, nil
}

func (s *Server) CreateActor(_ context.Context, req *pb.CreateActorRequest) (*pb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("CreateActor"); err != nil {
		return nil, err
	}
	in := req.GetActor()
	k := key{in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()}
	if k.atespace == "" || k.name == "" || in.GetActorTemplate().GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.atespace, metadata.name and actor_template are required")
	}
	t, ok := s.templates[key{in.GetActorTemplate().GetAtespace(), in.GetActorTemplate().GetName()}]
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "actor template not found")
	}
	if _, ok := s.actors[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %s/%s already exists", k.atespace, k.name)
	}
	if _, ok := s.atespaces[k.atespace]; !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "Atespace %s not found", k.atespace)
	}
	a := clone(in)
	a.Metadata = newMeta(in.GetMetadata())
	a.SourceTag = t.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag()
	a.Status = &pb.ActorStatus{State: pb.ActorState_ACTOR_STATE_SUSPENDED}
	s.actors[k] = a
	return clone(a), nil
}

func (s *Server) GetActor(_ context.Context, req *pb.GetActorRequest) (*pb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("GetActor"); err != nil {
		return nil, err
	}
	a, err := s.actor(req.GetActor())
	if err != nil {
		return nil, err
	}
	return clone(a), nil
}

func (s *Server) ListActors(_ context.Context, req *pb.ListActorsRequest) (*pb.ListActorsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("ListActors"); err != nil {
		return nil, err
	}
	resp := &pb.ListActorsResponse{}
	for k, a := range s.actors {
		if req.GetAtespace() == "" || req.GetAtespace() == k.atespace {
			resp.Actors = append(resp.Actors, clone(a))
		}
	}
	return resp, nil
}

func (s *Server) UpdateActor(_ context.Context, req *pb.UpdateActorRequest) (*pb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("UpdateActor"); err != nil {
		return nil, err
	}
	in := req.GetActor()
	stored, err := s.actor(&pb.ObjectRef{Atespace: in.GetMetadata().GetAtespace(), Name: in.GetMetadata().GetName()})
	if err != nil {
		return nil, err
	}
	if err := checkUpdate(stored.GetMetadata(), in.GetMetadata()); err != nil {
		return nil, err
	}
	if !proto.Equal(in.GetSourceTag(), stored.GetSourceTag()) {
		return nil, status.Error(codes.InvalidArgument, "source_tag is immutable")
	}
	oldRef, newRef := stored.GetActorTemplate(), in.GetActorTemplate()
	if !proto.Equal(oldRef, newRef) {
		if stored.GetStatus().GetState() != pb.ActorState_ACTOR_STATE_SUSPENDED {
			return nil, status.Error(codes.FailedPrecondition, "actor_template can only change while SUSPENDED")
		}
		nt, ok := s.templates[key{newRef.GetAtespace(), newRef.GetName()}]
		if !ok {
			return nil, status.Error(codes.FailedPrecondition, "actor template not found")
		}
		if ot, ok := s.templates[key{oldRef.GetAtespace(), oldRef.GetName()}]; ok &&
			(!proto.Equal(ot.GetSandboxConfig(), nt.GetSandboxConfig()) || !volumesEqual(ot, nt)) {
			return nil, status.Error(codes.FailedPrecondition, "new actor template must keep sandbox config and volumes")
		}
	}
	stored.ActorTemplate = clone(newRef)
	stored.WorkerSelector = clone(in.GetWorkerSelector())
	bump(stored.Metadata)
	return clone(stored), nil
}

func volumesEqual(a, b *pb.ActorTemplate) bool {
	if len(a.GetVolumes()) != len(b.GetVolumes()) || len(a.GetContainers()) != len(b.GetContainers()) {
		return false
	}
	for i := range a.GetVolumes() {
		if !proto.Equal(a.GetVolumes()[i], b.GetVolumes()[i]) {
			return false
		}
	}
	for i := range a.GetContainers() {
		am, bm := a.GetContainers()[i].GetVolumeMounts(), b.GetContainers()[i].GetVolumeMounts()
		if len(am) != len(bm) {
			return false
		}
		for j := range am {
			if !proto.Equal(am[j], bm[j]) {
				return false
			}
		}
	}
	return true
}

func (s *Server) SuspendActor(_ context.Context, req *pb.SuspendActorRequest) (*pb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("SuspendActor"); err != nil {
		return nil, err
	}
	a, err := s.actorWithTemplate(req.GetActor())
	if err != nil {
		return nil, err
	}
	switch a.Status.State {
	case pb.ActorState_ACTOR_STATE_SUSPENDED:
	case pb.ActorState_ACTOR_STATE_RUNNING, pb.ActorState_ACTOR_STATE_PAUSED, pb.ActorState_ACTOR_STATE_SUSPENDING:
		a.Status.State = pb.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.WorkerAssignment = nil
		bump(a.Metadata)
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "cannot suspend actor in state %s", a.Status.State)
	}
	return &pb.SuspendActorResponse{Actor: clone(a)}, nil
}

func (s *Server) ResumeActor(_ context.Context, req *pb.ResumeActorRequest) (*pb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("ResumeActor"); err != nil {
		return nil, err
	}
	a, err := s.actor(req.GetActor())
	if err != nil {
		return nil, err
	}
	if a.Status.State == pb.ActorState_ACTOR_STATE_RUNNING {
		return &pb.ResumeActorResponse{Actor: clone(a), Resumed: false}, nil
	}
	if _, err := s.actorWithTemplate(req.GetActor()); err != nil {
		return nil, err
	}
	switch a.Status.State {
	case pb.ActorState_ACTOR_STATE_SUSPENDED, pb.ActorState_ACTOR_STATE_PAUSED, pb.ActorState_ACTOR_STATE_RESUMING:
		a.Status.State = pb.ActorState_ACTOR_STATE_RUNNING
		bump(a.Metadata)
		return &pb.ResumeActorResponse{Actor: clone(a), Resumed: true}, nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "cannot resume actor in state %s", a.Status.State)
	}
}

func (s *Server) RevertActor(_ context.Context, req *pb.RevertActorRequest) (*pb.RevertActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("RevertActor"); err != nil {
		return nil, err
	}
	a, err := s.actor(req.GetActor())
	if err != nil {
		return nil, err
	}
	switch a.Status.State {
	case pb.ActorState_ACTOR_STATE_RUNNING, pb.ActorState_ACTOR_STATE_PAUSED,
		pb.ActorState_ACTOR_STATE_CRASHED, pb.ActorState_ACTOR_STATE_REVERTING:
		a.Status.State = pb.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.Crash = nil
		a.Status.WorkerAssignment = nil
		bump(a.Metadata)
		return &pb.RevertActorResponse{Actor: clone(a)}, nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "cannot revert actor in state %s", a.Status.State)
	}
}

func (s *Server) DeleteActor(_ context.Context, req *pb.DeleteActorRequest) (*pb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("DeleteActor"); err != nil {
		return nil, err
	}
	a, err := s.actor(req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := checkDelete(a.GetMetadata(), req.GetOptions()); err != nil {
		return nil, err
	}
	switch a.Status.State {
	case pb.ActorState_ACTOR_STATE_SUSPENDED, pb.ActorState_ACTOR_STATE_CRASHED, pb.ActorState_ACTOR_STATE_DELETING:
	default:
		if !req.GetAnyState() {
			return nil, status.Errorf(codes.FailedPrecondition, "Actor %s is not in a deletable state", a.GetMetadata().GetName())
		}
	}
	k := key{req.GetActor().GetAtespace(), req.GetActor().GetName()}
	delete(s.actors, k)
	delete(s.egress, k)
	return a, nil
}

func (s *Server) CreateActorEgressPolicy(_ context.Context, req *pb.CreateActorEgressPolicyRequest) (*pb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("CreateActorEgressPolicy"); err != nil {
		return nil, err
	}
	in := req.GetEgressPolicy()
	if in.GetMetadata().GetName() != "default" || in.GetMetadata().GetAtespace() != req.GetActor().GetAtespace() {
		return nil, status.Error(codes.InvalidArgument, `egress policy must be named "default" in the actor's atespace`)
	}
	k := key{req.GetActor().GetAtespace(), req.GetActor().GetName()}
	if _, ok := s.egress[k]; ok {
		return nil, status.Error(codes.AlreadyExists, "egress policy already exists")
	}
	if _, ok := s.actors[k]; !ok {
		return nil, status.Error(codes.FailedPrecondition, "parent Actor does not exist")
	}
	p := clone(in)
	p.Metadata = newMeta(&pb.ResourceMetadata{Atespace: k.atespace, Name: "default"})
	s.egress[k] = p
	return clone(p), nil
}

func (s *Server) GetActorEgressPolicy(_ context.Context, req *pb.GetActorEgressPolicyRequest) (*pb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("GetActorEgressPolicy"); err != nil {
		return nil, err
	}
	p, ok := s.egress[key{req.GetActor().GetAtespace(), req.GetActor().GetName()}]
	if !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	return clone(p), nil
}

func (s *Server) UpdateActorEgressPolicy(_ context.Context, req *pb.UpdateActorEgressPolicyRequest) (*pb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("UpdateActorEgressPolicy"); err != nil {
		return nil, err
	}
	p, ok := s.egress[key{req.GetActor().GetAtespace(), req.GetActor().GetName()}]
	if !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	if err := checkUpdate(p.GetMetadata(), req.GetEgressPolicy().GetMetadata()); err != nil {
		return nil, err
	}
	p.Rules = clone(req.GetEgressPolicy()).GetRules()
	bump(p.Metadata)
	return clone(p), nil
}

func (s *Server) DeleteActorEgressPolicy(_ context.Context, req *pb.DeleteActorEgressPolicyRequest) (*pb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.injected("DeleteActorEgressPolicy"); err != nil {
		return nil, err
	}
	k := key{req.GetActor().GetAtespace(), req.GetActor().GetName()}
	p, ok := s.egress[k]
	if !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	if err := checkDelete(p.GetMetadata(), req.GetOptions()); err != nil {
		return nil, err
	}
	delete(s.egress, k)
	return p, nil
}

func (s *Server) actor(ref *pb.ObjectRef) (*pb.Actor, error) {
	a, ok := s.actors[key{ref.GetAtespace(), ref.GetName()}]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	return a, nil
}

func (s *Server) actorWithTemplate(ref *pb.ObjectRef) (*pb.Actor, error) {
	a, err := s.actor(ref)
	if err != nil {
		return nil, err
	}
	tr := a.GetActorTemplate()
	if _, ok := s.templates[key{tr.GetAtespace(), tr.GetName()}]; !ok {
		return nil, status.Error(codes.FailedPrecondition, "actor template not found")
	}
	return a, nil
}
