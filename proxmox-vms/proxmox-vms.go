package proxmox_vms

import (
	"fmt"
	"sort"

	"github.com/muhlba91/pulumi-proxmoxve/sdk/v6/go/proxmoxve/vm"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// VmData defines the structure for a virtual machine's data.
// Each field is tagged with `yaml` annotations for easy deserialization from configuration files.
type VmData struct {
	Name        string `yaml:"name"`        // Name of the virtual machine.
	HostName    string `yaml:"hostName"`     // Hostname of the virtual machine.
	Ipv4Address string `yaml:"ipv4address"`  // IPv4 address of the virtual machine.
	NumCpus     int    `yaml:"numCpus"`      // Number of CPUs for the virtual machine.
	Memory      int    `yaml:"memory"`       // Memory size in MB for the virtual machine.
}

// ProxmoxCfg defines the Proxmox-specific configuration required for creating virtual machines.
type ProxmoxCfg struct {
	NodeName     string   `yaml:"nodeName"`     // Proxmox node name (e.g., "pve1").
	DatastoreId  string   `yaml:"datastoreId"`  // Storage for VM disks (e.g., "local-lvm").
	TemplateVmId int      `yaml:"templateVmId"` // VM ID of the template to clone.
	DiskSize     int      `yaml:"diskSize"`     // Disk size in GB.
	Tags         []string `yaml:"tags"`         // Tags for VM grouping (replaces vSphere folders).
	OnBoot       bool     `yaml:"onBoot"`       // Start VM on boot.
	Agent        bool     `yaml:"agent"`        // Enable QEMU guest agent.
}

// NetworkCfg defines the network configuration for the virtual machines.
type NetworkCfg struct {
	Gateway    string   `yaml:"gateway"`    // Network gateway IP address.
	DnsServers []string `yaml:"dnsServers"` // List of DNS server IP addresses.
	Domain     string   `yaml:"domain"`     // Network domain name.
	Mask       int      `yaml:"mask"`       // CIDR mask (e.g., 24).
	Bridge     string   `yaml:"bridge"`     // Proxmox bridge (e.g., "vmbr0").
	Model      string   `yaml:"model"`      // NIC model (default: "virtio").
	VlanTag    int      `yaml:"vlanTag"`    // Optional VLAN tag.
}

// ProxmoxVms is a Pulumi component resource for managing a group of Proxmox virtual machines.
type ProxmoxVms struct {
	pulumi.ResourceState
	VirtualMachines []*vm.VirtualMachine // List of created virtual machines.
}

// ProxmoxVmsArgs defines the arguments for creating a ProxmoxVms component.
type ProxmoxVmsArgs struct {
	Vms        []VmData
	ProxmoxCfg ProxmoxCfg
	NetworkCfg NetworkCfg
	MakeAlias  *bool // Optional parameter to enable aliases (defaults to false).
}

// NewProxmoxVmsFromConfig creates a new ProxmoxVms component by reading configuration from Pulumi config.
// It automatically reads the "vms", "proxmoxCfg", and "networkCfg" configuration objects.
func NewProxmoxVmsFromConfig(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (*ProxmoxVms, error) {
	var args ProxmoxVmsArgs
	cfg := config.New(ctx, "")
	cfg.RequireObject("vms", &args.Vms)
	cfg.RequireObject("proxmoxCfg", &args.ProxmoxCfg)
	cfg.RequireObject("networkCfg", &args.NetworkCfg)

	if args.NetworkCfg.Model == "" {
		args.NetworkCfg.Model = "virtio"
	}
	if args.NetworkCfg.Bridge == "" {
		args.NetworkCfg.Bridge = "vmbr0"
	}

	makeAlias := cfg.GetBool("makeAlias")
	args.MakeAlias = &makeAlias

	return NewProxmoxVms(ctx, name, &args, opts...)
}

// NewProxmoxVms creates a new ProxmoxVms component.
// It registers the component with Pulumi and creates the virtual machines based on the provided arguments.
func NewProxmoxVms(ctx *pulumi.Context, name string, args *ProxmoxVmsArgs, opts ...pulumi.ResourceOption) (*ProxmoxVms, error) {
	proxmoxVms := &ProxmoxVms{}
	err := ctx.RegisterComponentResource("pkg:index:ProxmoxVms", name, proxmoxVms, opts...)
	if err != nil {
		return nil, err
	}

	var virtualMachines []*vm.VirtualMachine
	for _, vmData := range args.Vms {
		var resOpts []pulumi.ResourceOption
		resOpts = append(resOpts, pulumi.Parent(proxmoxVms))
		resOpts = append(resOpts, pulumi.IgnoreChanges([]string{"nodeName"}))

		oldURN := fmt.Sprintf("urn:pulumi:%s::%s::proxmoxve:vm/virtualMachine:VirtualMachine::%s",
			ctx.Stack(), ctx.Project(), vmData.Name)
		if args.MakeAlias != nil && *args.MakeAlias {
			_ = ctx.Log.Debug(fmt.Sprintf("Adding alias for VM: %s - URN: %s", vmData.Name, oldURN), nil)
			aliases := pulumi.Aliases([]pulumi.Alias{
				{
					URN: pulumi.URN(oldURN),
				},
			})
			resOpts = append(resOpts, aliases)
		}

		newVm, err := createVm(ctx, args.ProxmoxCfg, args.NetworkCfg, vmData, resOpts...)
		if err != nil {
			return nil, err
		}
		virtualMachines = append(virtualMachines, newVm)
	}

	proxmoxVms.VirtualMachines = virtualMachines

	if err := ctx.RegisterResourceOutputs(proxmoxVms, pulumi.Map{}); err != nil {
		return nil, err
	}

	return proxmoxVms, nil
}

// createVm creates a single virtual machine in Proxmox by cloning a template.
func createVm(ctx *pulumi.Context, proxmoxCfg ProxmoxCfg, networkCfg NetworkCfg, vmData VmData, opts ...pulumi.ResourceOption) (*vm.VirtualMachine, error) {
	ipAddress := fmt.Sprintf("%s/%d", vmData.Ipv4Address, networkCfg.Mask)

	// Sort tags for consistent ordering
	tags := make([]string, len(proxmoxCfg.Tags))
	copy(tags, proxmoxCfg.Tags)
	sort.Strings(tags)

	vmArgs := &vm.VirtualMachineArgs{
		Name:     pulumi.String(vmData.Name),
		NodeName: pulumi.String(proxmoxCfg.NodeName),
		OnBoot:   pulumi.Bool(proxmoxCfg.OnBoot),
		Tags:     toStringArray(tags),
		Clone: &vm.VirtualMachineCloneArgs{
			VmId: pulumi.Int(proxmoxCfg.TemplateVmId),
			Full: pulumi.Bool(true),
		},
		Cpu: &vm.VirtualMachineCpuArgs{
			Cores:   pulumi.Int(vmData.NumCpus),
			Sockets: pulumi.Int(1),
		},
		Memory: &vm.VirtualMachineMemoryArgs{
			Dedicated: pulumi.Int(vmData.Memory),
		},
		Disks: vm.VirtualMachineDiskArray{
			&vm.VirtualMachineDiskArgs{
				DatastoreId: pulumi.String(proxmoxCfg.DatastoreId),
				Size:        pulumi.Int(proxmoxCfg.DiskSize),
				Interface:   pulumi.String("scsi0"),
			},
		},
		NetworkDevices: vm.VirtualMachineNetworkDeviceArray{
			getNetworkDevice(networkCfg),
		},
		Initialization: &vm.VirtualMachineInitializationArgs{
			DatastoreId: pulumi.String(proxmoxCfg.DatastoreId),
			IpConfigs: vm.VirtualMachineInitializationIpConfigArray{
				&vm.VirtualMachineInitializationIpConfigArgs{
					Ipv4: &vm.VirtualMachineInitializationIpConfigIpv4Args{
						Address: pulumi.String(ipAddress),
						Gateway: pulumi.String(networkCfg.Gateway),
					},
				},
			},
			Dns: &vm.VirtualMachineInitializationDnsArgs{
				Domain:  pulumi.String(networkCfg.Domain),
				Servers: toStringArray(networkCfg.DnsServers),
			},
		},
	}

	if proxmoxCfg.Agent {
		vmArgs.Agent = &vm.VirtualMachineAgentArgs{
			Enabled: pulumi.Bool(true),
		}
	}

	newVm, err := vm.NewVirtualMachine(ctx, vmData.Name, vmArgs, opts...)
	if err != nil {
		return nil, err
	}
	return newVm, nil
}

// getNetworkDevice creates a network device configuration.
func getNetworkDevice(networkCfg NetworkCfg) *vm.VirtualMachineNetworkDeviceArgs {
	model := networkCfg.Model
	if model == "" {
		model = "virtio"
	}
	bridge := networkCfg.Bridge
	if bridge == "" {
		bridge = "vmbr0"
	}

	device := &vm.VirtualMachineNetworkDeviceArgs{
		Bridge: pulumi.String(bridge),
		Model:  pulumi.String(model),
	}

	if networkCfg.VlanTag > 0 {
		device.VlanId = pulumi.Int(networkCfg.VlanTag)
	}

	return device
}

// toStringArray converts a string slice to a pulumi.StringArray.
func toStringArray(list []string) pulumi.StringArray {
	result := pulumi.StringArray{}
	for _, item := range list {
		result = append(result, pulumi.String(item))
	}
	return result
}

