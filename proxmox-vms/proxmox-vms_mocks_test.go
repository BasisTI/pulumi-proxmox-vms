package proxmox_vms

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const vmType = "proxmoxve:VM/virtualMachine:VirtualMachine"

// registeredVm is what the Pulumi engine received for one VM resource.
type registeredVm struct {
	inputs        resource.PropertyMap
	ignoreChanges []string
	guarded       bool // the capture option of newCreateGuard (Protect marker) reached the resource
}

// vmMocks records every VM registration and answers the VM listing with the cluster field.
type vmMocks struct {
	mu      sync.Mutex
	vms     map[string]registeredVm
	cluster []clusterVm
}

// clusterVm is one entry of the fake cluster listing.
type clusterVm struct {
	id       int
	name     string
	template bool
}

func (m *vmMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if args.TypeToken == vmType {
		m.mu.Lock()
		m.vms[args.Name] = registeredVm{
			inputs:        args.Inputs,
			ignoreChanges: args.RegisterRPC.GetIgnoreChanges(),
			guarded:       args.RegisterRPC.GetProtect(),
		}
		m.mu.Unlock()
	}
	return args.Name + "_id", args.Inputs, nil
}

func (m *vmMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	var list []resource.PropertyValue
	for _, c := range m.cluster {
		list = append(list, resource.NewObjectProperty(resource.PropertyMap{
			"name":     resource.NewStringProperty(c.name),
			"nodeName": resource.NewStringProperty("siliconvalley"),
			"vmId":     resource.NewNumberProperty(float64(c.id)),
			"template": resource.NewBoolProperty(c.template),
		}))
	}
	return resource.PropertyMap{"vms": resource.NewArrayProperty(list)}, nil
}

// guards holds the BeforeCreate hooks captured by the last runVms, by VM name. The mock monitor
// has no hook support, so newCreateGuard is replaced; the test plays the engine and calls the
// hook of the VMs it declares as being created (a VM already in the state has no create step).
var guards map[string]func() error

func init() {
	newCreateGuard = func(_ *pulumi.Context, name string, fn func() error) (pulumi.ResourceOption, error) {
		guards[strings.TrimPrefix(name, "template-guard-")] = fn
		return pulumi.Protect(true), nil
	}
}

// defaultCluster has the shared template 9000 only.
var defaultCluster = []clusterVm{{id: 9000, name: "ubuntu-template", template: true}}

// registerVms runs the component under mocks and returns what was registered for each VM.
func registerVms(t *testing.T, vms []VmData) map[string]registeredVm {
	t.Helper()
	got, err := runVms(vms, defaultCluster)
	if err != nil {
		t.Fatalf("NewProxmoxVms() error = %v", err)
	}
	return got
}

// runVms runs the component against a fake cluster and returns the registrations and the error.
func runVms(vms []VmData, cluster []clusterVm) (map[string]registeredVm, error) {
	mocks := &vmMocks{vms: map[string]registeredVm{}, cluster: cluster}
	guards = map[string]func() error{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewProxmoxVms(ctx, "test", &ProxmoxVmsArgs{
			Vms: vms,
			ProxmoxCfg: ProxmoxCfg{
				NodeName:     "siliconvalley",
				DatastoreId:  "local-lvm",
				TemplateVmId: 9000,
				DiskSize:     50,
			},
			NetworkCfg: NetworkCfg{Mask: 24, Gateway: "192.168.24.1"},
		})
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
	return mocks.vms, err
}

func TestVmIgnoresOnlyTheDiskSpeedBlockAndClone(t *testing.T) {
	vms := registerVms(t, []VmData{
		{Name: "chur", Ipv4Address: "192.168.24.103", NumCpus: 4, Memory: 16384},
		{Name: "thun", Ipv4Address: "192.168.24.104", NumCpus: 4, Memory: 16384},
	})

	for _, name := range []string{"chur", "thun"} {
		got, ok := vms[name]
		if !ok {
			t.Fatalf("VM %s was not registered", name)
		}
		// The concrete index is required: the bridge matches ignoreChanges paths literally
		// when filtering the provider diff, so "disks[*].speed" would hide nothing.
		// "clone" keeps a new templateVmId from replacing existing VMs; new VMs still send it.
		if want := []string{"disks[0].speed", "clone"}; !reflect.DeepEqual(got.ignoreChanges, want) {
			t.Fatalf("VM %s ignoreChanges = %v, want %v", name, got.ignoreChanges, want)
		}
		clone := got.inputs["clone"].ObjectValue()
		if id := clone["vmId"].NumberValue(); id != 9000 {
			t.Fatalf("VM %s clone.vmId = %v, want 9000: a new VM must still clone the template", name, id)
		}
	}
}

func TestVmDiskKeepsSizeManagedAndSendsNoSpeed(t *testing.T) {
	vms := registerVms(t, []VmData{{Name: "chur", Ipv4Address: "192.168.24.103", NumCpus: 4, Memory: 16384}})

	disks := vms["chur"].inputs["disks"].ArrayValue()
	if len(disks) != 1 {
		t.Fatalf("disks = %v, want exactly one disk (ignoreChanges targets disks[0])", disks)
	}
	disk := disks[0].ObjectValue()
	if got := disk["size"].NumberValue(); got != 50 {
		t.Fatalf("disks[0].size = %v, want 50", got)
	}
	if got := disk["interface"].StringValue(); got != "scsi0" {
		t.Fatalf("disks[0].interface = %q, want scsi0", got)
	}
	if _, ok := disk["speed"]; ok {
		t.Fatalf("disks[0].speed must not be sent: zero limits are what Proxmox reports as absent, got %v", disk["speed"])
	}
}

// create plays the engine for a VM with no state: it runs the BeforeCreate hook.
func create(t *testing.T, name string) error {
	t.Helper()
	fn, ok := guards[name]
	if !ok {
		t.Fatalf("VM %s has no BeforeCreate guard", name)
	}
	return fn()
}

func TestNewVmFromARegularVmIdIsRefused(t *testing.T) {
	cluster := append([]clusterVm{{id: 138, name: "cochem"}}, defaultCluster...)
	if _, err := runVms([]VmData{{Name: "nova", TemplateVmId: 138, NumCpus: 2, Memory: 1024}}, cluster); err != nil {
		t.Fatalf("the program itself must not fail, the engine does at create: %v", err)
	}

	err := create(t, "nova")

	if err == nil {
		t.Fatal("a new VM cloned from a regular VM must be refused")
	}
	for _, want := range []string{`"nova"`, "138", `"cochem"`, "not a template", "templateVmId"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %s", err, want)
		}
	}
}

func TestNewVmFromAMissingIdIsRefused(t *testing.T) {
	if _, err := runVms([]VmData{{Name: "nova", TemplateVmId: 4242, NumCpus: 2, Memory: 1024}}, defaultCluster); err != nil {
		t.Fatal(err)
	}

	err := create(t, "nova")

	if err == nil {
		t.Fatal("an unknown id must be refused")
	}
	for _, want := range []string{`"nova"`, "4242", "VM.Audit", "/vms/4242"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %s", err, want)
		}
	}
}

func TestNewVmFromATemplateIsAccepted(t *testing.T) {
	cluster := append([]clusterVm{{id: 9001, name: "ubuntu-new", template: true}}, defaultCluster...)
	if _, err := runVms([]VmData{{Name: "nova", TemplateVmId: 9001, NumCpus: 2, Memory: 1024}}, cluster); err != nil {
		t.Fatal(err)
	}

	if err := create(t, "nova"); err != nil {
		t.Fatalf("a template id must pass, got %v", err)
	}
}

func TestSharedTemplateIsGuardedToo(t *testing.T) {
	if _, err := runVms([]VmData{{Name: "nova", NumCpus: 2, Memory: 1024}}, []clusterVm{{id: 9000, name: "web"}}); err != nil {
		t.Fatal(err)
	}
	if err := create(t, "nova"); err == nil || !strings.Contains(err.Error(), "9000") {
		t.Fatalf("the shared proxmoxCfg.templateVmId must be checked too, got %v", err)
	}
}

// A namesake in the cluster (another stack, a manual VM) does not make the resource existing:
// the guard still refuses the creation. Reviewer case of TG-98 round 1.
func TestNamesakeDoesNotLiftTheGuard(t *testing.T) {
	cluster := []clusterVm{{id: 117, name: "nova"}, {id: 138, name: "cochem"}}
	if _, err := runVms([]VmData{{Name: "nova", TemplateVmId: 138, NumCpus: 2, Memory: 1024}}, cluster); err != nil {
		t.Fatal(err)
	}

	if err := create(t, "nova"); err == nil {
		t.Fatal("guard bypassed by a namesake: a new resource must not clone a regular VM")
	}
}

// Existing VMs are not checked at program time, whether the orphan id is now a regular VM or is
// gone altogether; the hook only fires on a create step, which the engine does not plan for them.
func TestExistingVmWithAnOrphanIdIsNotChecked(t *testing.T) {
	cluster := append([]clusterVm{{id: 138, name: "cochem"}, {id: 120, name: "chur"}, {id: 121, name: "thun"}}, defaultCluster...)

	got, err := runVms([]VmData{
		{Name: "chur", TemplateVmId: 138, NumCpus: 2, Memory: 1024},  // id now a regular VM
		{Name: "thun", TemplateVmId: 4242, NumCpus: 2, Memory: 1024}, // id gone
	}, cluster)

	if err != nil {
		t.Fatalf("existing VMs must not be checked, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("both VMs must be registered, got %d", len(got))
	}
}

// Renaming a VM in the stack is a new URN, so a real create: the guard applies to it. This is
// documented behavior, not an error of the check.
func TestRenamedVmIsGuardedAsACreate(t *testing.T) {
	cluster := []clusterVm{{id: 120, name: "old-name"}, {id: 138, name: "cochem"}}
	if _, err := runVms([]VmData{{Name: "new-name", TemplateVmId: 138, NumCpus: 2, Memory: 1024}}, cluster); err != nil {
		t.Fatal(err)
	}
	if err := create(t, "new-name"); err == nil {
		t.Fatal("a rename creates a new VM and must be guarded")
	}
}

func TestEveryVmGetsAGuardAttachedToItsResource(t *testing.T) {
	got, err := runVms([]VmData{{Name: "a"}, {Name: "b"}}, defaultCluster)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if !got[name].guarded {
			t.Fatalf("VM %s was registered without the BeforeCreate guard", name)
		}
	}
}
