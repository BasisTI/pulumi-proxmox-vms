package proxmox_vms

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeEngine is a local gRPC resource monitor + engine. Unlike the SDK mocks it implements
// RegisterResourceHook, so the real hook registration of the component runs end to end up to the
// wire: what the engine would receive, and the callback it would invoke on a create step.
type fakeEngine struct {
	pulumirpc.UnimplementedResourceMonitorServer
	pulumirpc.UnimplementedEngineServer

	supportsHooks bool
	cluster       []clusterVm

	mu         sync.Mutex
	registered []*pulumirpc.RegisterResourceRequest
	hooks      map[string]*pulumirpc.RegisterResourceHookRequest
	invokes    int
}

func (e *fakeEngine) SupportsFeature(_ context.Context, r *pulumirpc.SupportsFeatureRequest) (*pulumirpc.SupportsFeatureResponse, error) {
	has := r.Id != "outputValues"
	if r.Id == "resourceHooks" {
		has = e.supportsHooks
	}
	return &pulumirpc.SupportsFeatureResponse{HasSupport: has}, nil
}

func (e *fakeEngine) RegisterResourceHook(_ context.Context, r *pulumirpc.RegisterResourceHookRequest) (*emptypb.Empty, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks[r.Name] = r
	return &emptypb.Empty{}, nil
}

func (e *fakeEngine) RegisterResource(_ context.Context, r *pulumirpc.RegisterResourceRequest) (*pulumirpc.RegisterResourceResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.registered = append(e.registered, r)
	return &pulumirpc.RegisterResourceResponse{Urn: "urn:pulumi:stack::project::" + r.Type + "::" + r.Name, Id: r.Name + "_id", Object: r.Object}, nil
}

func (e *fakeEngine) RegisterResourceOutputs(context.Context, *pulumirpc.RegisterResourceOutputsRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (e *fakeEngine) Invoke(context.Context, *pulumirpc.ResourceInvokeRequest) (*pulumirpc.InvokeResponse, error) {
	e.mu.Lock()
	e.invokes++
	e.mu.Unlock()
	var list []any
	for _, c := range e.cluster {
		list = append(list, map[string]any{"name": c.name, "nodeName": "siliconvalley", "vmId": float64(c.id), "template": c.template})
	}
	ret, err := structpb.NewStruct(map[string]any{"vms": list})
	return &pulumirpc.InvokeResponse{Return: ret}, err
}

func (e *fakeEngine) Log(context.Context, *pulumirpc.LogRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// startEngine serves the fake and points PULUMI_MONITOR at it, as the CLI does for the program.
func startEngine(t *testing.T, supportsHooks bool, cluster []clusterVm) (*fakeEngine, string) {
	t.Helper()
	e := &fakeEngine{supportsHooks: supportsHooks, cluster: cluster, hooks: map[string]*pulumirpc.RegisterResourceHookRequest{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pulumirpc.RegisterResourceMonitorServer(srv, e)
	pulumirpc.RegisterEngineServer(srv, e)
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	t.Setenv(pulumi.EnvMonitor, l.Addr().String())
	return e, l.Addr().String()
}

// runAgainstEngine runs the real component (no seam replaced) against the fake engine, in preview.
func runAgainstEngine(t *testing.T, e *fakeEngine, addr string, vms []VmData) error {
	t.Helper()
	ctx, err := pulumi.NewContext(context.Background(), pulumi.RunInfo{
		Project: "project", Stack: "stack", DryRun: true, MonitorAddr: addr, EngineAddr: addr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	return pulumi.RunWithContext(ctx, func(ctx *pulumi.Context) error {
		_, err := NewProxmoxVms(ctx, "test", &ProxmoxVmsArgs{
			Vms:        vms,
			ProxmoxCfg: ProxmoxCfg{NodeName: "siliconvalley", DatastoreId: "local-lvm", TemplateVmId: 9000, DiskSize: 50},
			NetworkCfg: NetworkCfg{Mask: 24, Gateway: "192.168.24.1"},
		})
		return err
	})
}

func (e *fakeEngine) vmRegistrations() map[string]*pulumirpc.RegisterResourceRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]*pulumirpc.RegisterResourceRequest{}
	for _, r := range e.registered {
		if r.Type == vmType {
			out[r.Name] = r
		}
	}
	return out
}

// invokeHook calls the callback registered for hook the way the engine does on a create step and
// returns the error text it reports (empty when the hook allows the create).
func invokeHook(t *testing.T, h *pulumirpc.RegisterResourceHookRequest) string {
	t.Helper()
	conn, err := grpc.NewClient(h.Callback.Target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, _ := proto.Marshal(&pulumirpc.ResourceHookRequest{})
	resp, err := pulumirpc.NewCallbacksClient(conn).Invoke(context.Background(), &pulumirpc.CallbackInvokeRequest{Token: h.Callback.Token, Request: data})
	if err != nil {
		t.Fatal(err)
	}
	var out pulumirpc.ResourceHookResponse
	if err := proto.Unmarshal(resp.Response, &out); err != nil {
		t.Fatal(err)
	}
	return out.Error
}

// Two VMs from different origins: a from a template, b from the id of a regular VM. Each VM must
// carry its own BeforeCreate hook, registered to run on preview, whose error reaches the engine.
func TestHookIsRegisteredPerVmOnTheWire(t *testing.T) {
	cluster := []clusterVm{{id: 9000, name: "tpl", template: true}, {id: 138, name: "cochem"}}
	e, addr := startEngine(t, true, cluster)

	err := runAgainstEngine(t, e, addr, []VmData{
		{Name: "a", NumCpus: 2, Memory: 1024},                    // shared template 9000
		{Name: "b", TemplateVmId: 138, NumCpus: 2, Memory: 1024}, // regular VM
	})
	if err != nil {
		t.Fatalf("component failed: %v", err)
	}

	regs := e.vmRegistrations()
	if len(regs) != 2 {
		t.Fatalf("registered VMs = %d, want 2", len(regs))
	}
	if e.invokes != 1 {
		t.Fatalf("the cluster was listed %d times, want exactly 1", e.invokes)
	}
	hookOf := func(vmName string) *pulumirpc.RegisterResourceHookRequest {
		t.Helper()
		b := regs[vmName].GetHooks()
		if b == nil || len(b.BeforeCreate) != 1 {
			t.Fatalf("VM %s must have exactly one BeforeCreate hook, binding = %v", vmName, b)
		}
		if len(b.BeforeUpdate)+len(b.AfterCreate)+len(b.AfterUpdate)+len(b.BeforeDelete)+len(b.AfterDelete) != 0 {
			t.Fatalf("VM %s must have only a BeforeCreate hook, binding = %v", vmName, b)
		}
		h := e.hooks[b.BeforeCreate[0]]
		if h == nil {
			t.Fatalf("hook %q of VM %s was never registered with the engine", b.BeforeCreate[0], vmName)
		}
		if !h.OnDryRun {
			t.Fatalf("hook of VM %s must run on preview (OnDryRun)", vmName)
		}
		return h
	}
	ha, hb := hookOf("a"), hookOf("b")
	if ha.Name == hb.Name {
		t.Fatalf("both VMs share the hook %q", ha.Name)
	}
	if msg := invokeHook(t, ha); msg != "" {
		t.Fatalf("hook of a (template origin) must allow the create, got %q", msg)
	}
	msg := invokeHook(t, hb)
	for _, want := range []string{`"b"`, "138", `"cochem"`, "not a template"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error of the hook of b = %q, must mention %s", msg, want)
		}
	}
}

// A CLI without hooks must stop the program with a clear error and no VM may be registered:
// the SDK itself would swallow the rejection and drop the VM in silence.
func TestCliWithoutHooksFailsClosedAndRegistersNothing(t *testing.T) {
	e, addr := startEngine(t, false, []clusterVm{{id: 9000, name: "tpl", template: true}})

	err := runAgainstEngine(t, e, addr, []VmData{{Name: "a", NumCpus: 2, Memory: 1024}})

	if err == nil || !strings.Contains(err.Error(), "does not support resource hooks") || !strings.Contains(err.Error(), "NOT registered") {
		t.Fatalf("want a clear error about resource hooks, got %v", err)
	}
	if n := len(e.vmRegistrations()); n != 0 {
		t.Fatalf("%d VMs registered, want none", n)
	}
	if len(e.hooks) != 0 {
		t.Fatalf("no hook may be registered, got %v", e.hooks)
	}
}

func TestMissingMonitorAddressFailsClosed(t *testing.T) {
	t.Setenv(pulumi.EnvMonitor, "")
	if err := ensureHooksSupported(context.Background()); err == nil {
		t.Fatal("without PULUMI_MONITOR the support cannot be confirmed: must fail")
	}
}
