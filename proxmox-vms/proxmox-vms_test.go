package proxmox_vms

import (
	"reflect"
	"testing"
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
