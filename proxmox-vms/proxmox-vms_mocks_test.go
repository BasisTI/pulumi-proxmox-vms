package proxmox_vms

import (
	"reflect"
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

// vmMocks records every VM registration and answers the node lookup with an empty cluster.
type vmMocks struct {
	mu  sync.Mutex
	vms map[string]registeredVm
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
	return resource.PropertyMap{"vms": resource.NewArrayProperty(nil)}, nil
}

// registerVms runs the component under mocks and returns what was registered for each VM.
func registerVms(t *testing.T, vms []VmData) map[string]registeredVm {
	t.Helper()
	mocks := &vmMocks{vms: map[string]registeredVm{}}
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
	if err != nil {
		t.Fatalf("NewProxmoxVms() error = %v", err)
	}
	return mocks.vms
}

func TestVmIgnoresOnlyTheDiskSpeedBlock(t *testing.T) {
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
		if want := []string{"disks[0].speed"}; !reflect.DeepEqual(got.ignoreChanges, want) {
			t.Fatalf("VM %s ignoreChanges = %v, want %v", name, got.ignoreChanges, want)
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
