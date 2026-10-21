package proxmox_vms

import (
	"fmt"
	"sort"

	"github.com/muhlba91/pulumi-proxmoxve/sdk/v7/go/proxmoxve/vm"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// VmData defines the structure for a virtual machine's data.
// Each field is tagged with `yaml` annotations for easy deserialization from configuration files.
type VmData struct {
	Name        string `yaml:"name"`        // Name of the virtual machine.
	HostName    string `yaml:"hostName"`    // Hostname of the virtual machine.
	Ipv4Address string `yaml:"ipv4address"` // IPv4 address of the virtual machine.
	NumCpus     int    `yaml:"numCpus"`     // CPU cores per socket.
	Sockets     int    `yaml:"sockets"`     // Optional number of CPU sockets; 0 means 1. vCPUs = numCpus * sockets.
	Memory      int    `yaml:"memory"`      // Memory size in MB for the virtual machine.
	Role        string `yaml:"role"`        // Optional role of the VM (e.g., "master", "worker"); added as a tag.
	CpuType     string `yaml:"cpuType"`     // Optional CPU type for this VM; overrides ProxmoxCfg.CpuType.
	// TemplateVmId is the optional template this VM is cloned from; 0 means ProxmoxCfg.TemplateVmId.
	// Like the rest of the clone block it is ignored once the VM exists (see clonePath), so it only
	// decides the template of a VM that is being created.
	TemplateVmId int `yaml:"templateVmId"`
}

// ProxmoxCfg defines the Proxmox-specific configuration required for creating virtual machines.
type ProxmoxCfg struct {
	NodeName     string   `yaml:"nodeName"`     // Proxmox node where NEW VMs are created; existing VMs are found by name on any node.
	DatastoreId  string   `yaml:"datastoreId"`  // Storage for VM disks (e.g., "local-lvm").
	TemplateVmId int      `yaml:"templateVmId"` // VM ID of the template to clone.
	LinkedClone  bool     `yaml:"linkedClone"`  // Use linked clone instead of full clone.
	DiskSize     int      `yaml:"diskSize"`     // Disk size in GB.
	Tags         []string `yaml:"tags"`         // Tags for VM grouping (replaces vSphere folders).
	OnBoot       bool     `yaml:"onBoot"`       // Start VM on boot.
	Agent        bool     `yaml:"agent"`        // Enable QEMU guest agent.
	CpuType      string   `yaml:"cpuType"`      // Optional default CPU type (e.g., "x86-64-v3"); empty keeps the provider default.
	// RebootAfterUpdate lets the provider reboot a VM right after an update that needs one
	// (CPU type, memory). Defaults to false so a `pulumi up` never restarts running VMs on
	// its own; the change is applied on the next restart done by the operator.
	RebootAfterUpdate bool `yaml:"rebootAfterUpdate"`
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

	nodes, err := resolveVmNodes(ctx, args.Vms)
	if err != nil {
		return nil, err
	}

	var virtualMachines []*vm.VirtualMachine
	for _, vmData := range args.Vms {
		var resOpts []pulumi.ResourceOption
		resOpts = append(resOpts, pulumi.Parent(proxmoxVms))

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

		nodeName := nodeFor(nodes, vmData, args.ProxmoxCfg)
		if nodeName != args.ProxmoxCfg.NodeName {
			_ = ctx.Log.Info(fmt.Sprintf("VM %s lives on node %s (proxmoxCfg.nodeName is %s); using the real node",
				vmData.Name, nodeName, args.ProxmoxCfg.NodeName), nil)
		}

		newVm, err := createVm(ctx, args.ProxmoxCfg, args.NetworkCfg, vmData, nodeName, resOpts...)
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
// nodeName is the node the VM lives on (or, for a new VM, the node to create it on).
func createVm(ctx *pulumi.Context, proxmoxCfg ProxmoxCfg, networkCfg NetworkCfg, vmData VmData, nodeName string, opts ...pulumi.ResourceOption) (*vm.VirtualMachine, error) {
	ipAddress := fmt.Sprintf("%s/%d", vmData.Ipv4Address, networkCfg.Mask)

	disks := vm.VirtualMachineDiskArray{
		&vm.VirtualMachineDiskArgs{
			DatastoreId: pulumi.String(proxmoxCfg.DatastoreId),
			Size:        pulumi.Int(proxmoxCfg.DiskSize),
			Interface:   pulumi.String("scsi0"),
			Iothread:    pulumi.Bool(true),
			Replicate:   pulumi.Bool(false),
		},
	}

	vmArgs := &vm.VirtualMachineArgs{
		Name:     pulumi.String(vmData.Name),
		NodeName: pulumi.String(nodeName),
		// A node change must never be a replace. With migrate=true the provider treats it as an
		// in-place update; when the state still points at the old node the update fails loudly
		// instead of destroying and re-creating the VM.
		Migrate: pulumi.Bool(true),
		OnBoot:  pulumi.Bool(proxmoxCfg.OnBoot),
		Tags:    toStringArray(buildTags(proxmoxCfg, vmData)),
		Clone: &vm.VirtualMachineCloneArgs{
			VmId: pulumi.Int(templateVmIdFor(proxmoxCfg, vmData)),
			Full: pulumi.Bool(!proxmoxCfg.LinkedClone),
		},
		Cpu: cpuArgs(proxmoxCfg, vmData),
		// Always sent: the provider default is true, which reboots every VM in parallel.
		RebootAfterUpdate: pulumi.Bool(proxmoxCfg.RebootAfterUpdate),
		Memory: &vm.VirtualMachineMemoryArgs{
			Dedicated: pulumi.Int(vmData.Memory),
		},
		Disks: disks,
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

	// Explicitly set CDROM to "none" to prevent the provider from adding a default
	// ide3 device with host_cdrom driver (which fails with empty filename).
	vmArgs.Cdrom = &vm.VirtualMachineCdromArgs{
		FileId: pulumi.String("none"),
	}

	if proxmoxCfg.Agent {
		vmArgs.Agent = &vm.VirtualMachineAgentArgs{
			Enabled: pulumi.Bool(true),
		}
	}

	opts = append(opts, pulumi.IgnoreChanges(append(diskSpeedPaths(len(disks)), clonePath)))

	newVm, err := vm.NewVirtualMachine(ctx, vmData.Name, vmArgs, opts...)
	if err != nil {
		return nil, err
	}
	return newVm, nil
}

// clonePath is ignored so that changing templateVmId (shared or per VM) does not replace the existing VMs.
//
// The provider marks clone.vm_id ForceNew, but its Read never fills clone back from the API: the
// clone block exists only in the configuration and the state. A new template id would therefore
// replace every VM of the stack, although a VM never depends on its source template (full clone,
// linkedClone: false). With the path ignored, the engine keeps the stored clone for VMs that
// already exist, and only VMs created from now on use the new template. The template may even
// be deleted from Proxmox afterwards.
const clonePath = "clone"

// diskSpeedPaths returns the ignoreChanges paths for the speed block of each disk.
//
// The provider gives disks[n].speed a default of one block with every limit at 0, while its
// Read stores an empty list when Proxmox reports no limits. Config and state never match, so
// every preview proposes adding the zeroed block, and the update does not stick. Declaring the
// zeros does not help either. The component never sets speed limits, so the block is ignored.
// The index must be concrete: the bridge matches these paths literally against the provider
// diff, and "disks[*].speed" would hide nothing.
func diskSpeedPaths(diskCount int) []string {
	paths := make([]string, 0, diskCount)
	for i := 0; i < diskCount; i++ {
		paths = append(paths, fmt.Sprintf("disks[%d].speed", i))
	}
	return paths
}

// resolveVmNodes asks Proxmox where each configured VM currently lives, so a VM migrated to
// another node keeps being managed instead of failing with "the requested resource does not
// exist". VMs not found (not created yet) are absent from the map.
func resolveVmNodes(ctx *pulumi.Context, vms []VmData) (map[string]string, error) {
	res, err := vm.GetVirtualMachines(ctx, &vm.GetVirtualMachinesArgs{})
	if err != nil {
		return nil, fmt.Errorf("listing the VMs of the Proxmox cluster: %w", err)
	}
	return nodesByName(res.Vms, vms)
}

// nodesByName maps each wanted VM name to the node where Proxmox reports it. Templates are
// skipped. A name present more than once is an error, because the node could not be chosen.
func nodesByName(found []vm.GetVirtualMachinesVm, wanted []VmData) (map[string]string, error) {
	byName := map[string][]vm.GetVirtualMachinesVm{}
	for _, f := range found {
		if f.Template != nil && *f.Template {
			continue
		}
		byName[f.Name] = append(byName[f.Name], f)
	}

	nodes := make(map[string]string, len(wanted))
	for _, w := range wanted {
		matches := byName[w.Name]
		switch len(matches) {
		case 0:
			// Not created yet: the caller falls back to ProxmoxCfg.NodeName.
		case 1:
			nodes[w.Name] = matches[0].NodeName
		default:
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, fmt.Sprintf("%d@%s", m.VmId, m.NodeName))
			}
			sort.Strings(ids)
			return nil, fmt.Errorf("VM name %q is not unique in the Proxmox cluster (%v); rename the extra VMs", w.Name, ids)
		}
	}
	return nodes, nil
}

// nodeFor returns the node to use for a VM: where it lives if it exists, else the configured node.
func nodeFor(nodes map[string]string, vmData VmData, proxmoxCfg ProxmoxCfg) string {
	if n := nodes[vmData.Name]; n != "" {
		return n
	}
	return proxmoxCfg.NodeName
}

// buildTags returns the VM tags: the shared ProxmoxCfg.Tags plus the VM role, when set,
// sorted so the order never produces a spurious diff.
func buildTags(proxmoxCfg ProxmoxCfg, vmData VmData) []string {
	tags := make([]string, 0, len(proxmoxCfg.Tags)+1)
	tags = append(tags, proxmoxCfg.Tags...)
	if vmData.Role != "" {
		tags = append(tags, vmData.Role)
	}
	sort.Strings(tags)
	return tags
}

// resolveCpuType picks the CPU type for a VM: the VM value wins over the shared default.
// An empty result means the type is not sent and the provider default applies.
func resolveCpuType(proxmoxCfg ProxmoxCfg, vmData VmData) string {
	if vmData.CpuType != "" {
		return vmData.CpuType
	}
	return proxmoxCfg.CpuType
}

// templateVmIdFor returns the template to clone: the VM value wins over the shared default.
func templateVmIdFor(proxmoxCfg ProxmoxCfg, vmData VmData) int {
	if vmData.TemplateVmId > 0 {
		return vmData.TemplateVmId
	}
	return proxmoxCfg.TemplateVmId
}

// socketsFor returns the socket count of a VM, defaulting to 1 when not configured.
func socketsFor(vmData VmData) int {
	if vmData.Sockets > 0 {
		return vmData.Sockets
	}
	return 1
}

// cpuArgs builds the CPU block of the VM from the resolved core count and type.
func cpuArgs(proxmoxCfg ProxmoxCfg, vmData VmData) *vm.VirtualMachineCpuArgs {
	args := &vm.VirtualMachineCpuArgs{
		Cores:   pulumi.Int(vmData.NumCpus),
		Sockets: pulumi.Int(socketsFor(vmData)),
	}
	if cpuType := resolveCpuType(proxmoxCfg, vmData); cpuType != "" {
		args.Type = pulumi.String(cpuType)
	}
	return args
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
