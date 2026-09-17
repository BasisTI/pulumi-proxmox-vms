package proxmox_vms

import (
	"reflect"
	"strings"
	"testing"

	"github.com/muhlba91/pulumi-proxmoxve/sdk/v7/go/proxmoxve/vm"
)

func TestBuildTagsAddsRoleAndSorts(t *testing.T) {
	cfg := ProxmoxCfg{Tags: []string{"staging", "kubernetes"}}

	got := buildTags(cfg, VmData{Role: "worker"})

	want := []string{"kubernetes", "staging", "worker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildTags() = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(cfg.Tags, []string{"staging", "kubernetes"}) {
		t.Fatalf("buildTags() must not reorder the shared config tags, got %v", cfg.Tags)
	}
}

func TestBuildTagsWithoutRoleKeepsSharedTags(t *testing.T) {
	cfg := ProxmoxCfg{Tags: []string{"b", "a"}}

	got := buildTags(cfg, VmData{})

	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("buildTags() = %v, want %v", got, want)
	}
}

func TestResolveCpuType(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		vm   string
		want string
	}{
		{"nothing set keeps provider default", "", "", ""},
		{"shared default applies", "x86-64-v3", "", "x86-64-v3"},
		{"vm value wins over shared default", "x86-64-v3", "host", "host"},
		{"vm value alone", "", "x86-64-v2-AES", "x86-64-v2-AES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCpuType(ProxmoxCfg{CpuType: tc.cfg}, VmData{CpuType: tc.vm})
			if got != tc.want {
				t.Fatalf("resolveCpuType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCpuArgsOmitsTypeWhenUnset(t *testing.T) {
	args := cpuArgs(ProxmoxCfg{}, VmData{NumCpus: 4})

	if args.Type != nil {
		t.Fatalf("cpuArgs() must leave Type nil when no type is configured, got %v", args.Type)
	}
}

func TestRebootAfterUpdateDefaultsToFalse(t *testing.T) {
	var cfg ProxmoxCfg

	if cfg.RebootAfterUpdate {
		t.Fatal("RebootAfterUpdate must default to false so pulumi up never reboots running VMs")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestNodesByNameUsesTheNodeReportedByProxmox(t *testing.T) {
	found := []vm.GetVirtualMachinesVm{
		{Name: "chur", NodeName: "mountainview", VmId: 117},
		{Name: "thun", NodeName: "siliconvalley", VmId: 116},
		{Name: "chur", NodeName: "siliconvalley", VmId: 102, Template: boolPtr(true)},
	}
	wanted := []VmData{{Name: "chur"}, {Name: "thun"}, {Name: "nova"}}

	got, err := nodesByName(found, wanted)
	if err != nil {
		t.Fatalf("nodesByName() error = %v", err)
	}
	want := map[string]string{"chur": "mountainview", "thun": "siliconvalley"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodesByName() = %v, want %v", got, want)
	}
}

func TestNodesByNameRejectsDuplicateNames(t *testing.T) {
	found := []vm.GetVirtualMachinesVm{
		{Name: "chur", NodeName: "mountainview", VmId: 117},
		{Name: "chur", NodeName: "redwood", VmId: 140},
	}

	_, err := nodesByName(found, []VmData{{Name: "chur"}})
	if err == nil || !strings.Contains(err.Error(), "117@mountainview") || !strings.Contains(err.Error(), "140@redwood") {
		t.Fatalf("nodesByName() must name both duplicates, got %v", err)
	}
}

func TestNodeForFallsBackToConfiguredNode(t *testing.T) {
	cfg := ProxmoxCfg{NodeName: "siliconvalley"}
	nodes := map[string]string{"chur": "mountainview"}

	if got := nodeFor(nodes, VmData{Name: "chur"}, cfg); got != "mountainview" {
		t.Fatalf("nodeFor(existing) = %q, want mountainview", got)
	}
	if got := nodeFor(nodes, VmData{Name: "nova"}, cfg); got != "siliconvalley" {
		t.Fatalf("nodeFor(new) = %q, want siliconvalley", got)
	}
}
