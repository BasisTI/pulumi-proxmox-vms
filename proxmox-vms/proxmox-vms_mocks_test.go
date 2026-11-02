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

func TestNewVmFromARegularVmIdIsRefused(t *testing.T) {
	cluster := append([]clusterVm{{id: 138, name: "cochem", template: false}}, defaultCluster...)

	got, err := runVms([]VmData{{Name: "nova", TemplateVmId: 138, NumCpus: 2, Memory: 1024}}, cluster)

	if err == nil {
		t.Fatal("a new VM cloned from a regular VM must be refused")
	}
	for _, want := range []string{`"nova"`, "138", `"cochem"`, "not a template", "templateVmId"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %s", err, want)
		}
	}
	if _, ok := got["nova"]; ok {
		t.Fatal("the VM must not be registered when the guard refuses it")
	}
}

func TestNewVmFromAMissingIdIsRefused(t *testing.T) {
	_, err := runVms([]VmData{{Name: "nova", TemplateVmId: 4242, NumCpus: 2, Memory: 1024}}, defaultCluster)

	if err == nil || !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), `"nova"`) {
		t.Fatalf("an unknown id must be refused naming the VM and the id, got %v", err)
	}
}

func TestNewVmFromATemplateIsAccepted(t *testing.T) {
	got, err := runVms([]VmData{{Name: "nova", TemplateVmId: 9001, NumCpus: 2, Memory: 1024}},
		append([]clusterVm{{id: 9001, name: "ubuntu-new", template: true}}, defaultCluster...))

	if err != nil {
		t.Fatalf("a template id must pass, got %v", err)
	}
	if _, ok := got["nova"]; !ok {
		t.Fatal("VM nova was not registered")
	}
}

func TestExistingVmWithAnOrphanIdIsNotChecked(t *testing.T) {
	cluster := append([]clusterVm{{id: 138, name: "cochem"}, {id: 120, name: "chur"}}, defaultCluster...)

	got, err := runVms([]VmData{
		{Name: "chur", TemplateVmId: 138, NumCpus: 2, Memory: 1024},   // id of a regular VM
		{Name: "chur2", TemplateVmId: 9000, NumCpus: 2, Memory: 1024}, // new, valid
	}, append(cluster, clusterVm{id: 121, name: "chur2"}))

	if err != nil {
		t.Fatalf("an existing VM must not be checked, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("both VMs must be registered, got %d", len(got))
	}
}

func TestCheckTemplatesFallsBackToTheSharedTemplate(t *testing.T) {
	err := checkTemplates(nil, []VmData{{Name: "nova"}}, ProxmoxCfg{TemplateVmId: 138})
	if err == nil || !strings.Contains(err.Error(), "138") {
		t.Fatalf("the shared templateVmId must be checked too, got %v", err)
	}
}
