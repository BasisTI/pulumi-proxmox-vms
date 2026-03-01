package main

import (
	"github.com/BasisTI/pulumi-proxmox-vms/v2/proxmox-vms"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		// Create the VMs
		proxmoxVms, err := proxmox_vms.NewProxmoxVmsFromConfig(ctx, "proxmox-vms")
		if err != nil {
			return err
		}

		// Export VM IDs
		var vmIds []pulumi.StringOutput
		for _, vm := range proxmoxVms.VirtualMachines {
			vmIds = append(vmIds, vm.ID().ToStringOutput())
		}
		ctx.Export("vmIds", pulumi.ToStringArrayOutput(vmIds))
		return nil
	})
}
