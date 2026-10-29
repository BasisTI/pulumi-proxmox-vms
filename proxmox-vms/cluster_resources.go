package proxmox_vms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/muhlba91/pulumi-proxmoxve/sdk/v7/go/proxmoxve/vm"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pconfig "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// clusterResourcesTimeout bounds the lookup. /cluster/resources is served from the cluster
// cache and answers in milliseconds, so a long wait means something is wrong.
const clusterResourcesTimeout = 20 * time.Second

// clusterResourcesEndpoint holds what is needed to call /cluster/resources, taken from the
// configuration of the proxmoxve provider (proxmoxve:endpoint, proxmoxve:apiToken,
// proxmoxve:insecure), so no new secret is introduced.
type clusterResourcesEndpoint struct {
	URL      string
	ApiToken string
	Insecure bool
}

// clusterResourcesFromConfig reads the provider configuration. ok is false when the provider
// is not authenticated by API token (username/password, ticket): the ticket flow is not
// reimplemented here, and the caller falls back to the provider data source.
func clusterResourcesFromConfig(ctx *pulumi.Context) (ep clusterResourcesEndpoint, ok bool) {
	url := pconfig.Get(ctx, "proxmoxve:endpoint")
	token := pconfig.Get(ctx, "proxmoxve:apiToken")
	if url == "" || token == "" {
		return clusterResourcesEndpoint{}, false
	}
	return clusterResourcesEndpoint{
		URL:      url,
		ApiToken: token,
		Insecure: pconfig.GetBool(ctx, "proxmoxve:insecure"),
	}, true
}

// clusterResource is one entry of GET /cluster/resources?type=vm.
type clusterResource struct {
	Type     string `json:"type"`
	VmId     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Template int    `json:"template"`
}

// listClusterVms returns the QEMU VMs of the cluster from GET /cluster/resources?type=vm, in
// the shape of the provider data source, so nodesByName works unchanged.
//
// The provider data source reads /nodes/<node>/qemu, which makes Proxmox read the status of
// every VM of the node (smaps_rollup per VM) and took over 30 s on a loaded node, beyond the
// proxy limit (HTTP 596). /cluster/resources comes from the cluster cache instead.
func listClusterVms(ctx context.Context, client *http.Client, ep clusterResourcesEndpoint) ([]vm.GetVirtualMachinesVm, error) {
	ctx, cancel := context.WithTimeout(ctx, clusterResourcesTimeout)
	defer cancel()

	url := strings.TrimRight(ep.URL, "/") + "/api2/json/cluster/resources?type=vm"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+ep.ApiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// The error of net/http carries the URL, never the header with the token.
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /cluster/resources answered HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data []clusterResource `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decoding /cluster/resources: %w", err)
	}

	vms := make([]vm.GetVirtualMachinesVm, 0, len(parsed.Data))
	for _, r := range parsed.Data {
		// type=vm also returns LXC containers; the data source it replaces lists QEMU only.
		if r.Type != "qemu" {
			continue
		}
		template := r.Template == 1
		vms = append(vms, vm.GetVirtualMachinesVm{
			Name:     r.Name,
			NodeName: r.Node,
			VmId:     r.VmId,
			Template: &template,
		})
	}
	return vms, nil
}

// newClusterResourcesClient builds the HTTP client honoring proxmoxve:insecure.
func newClusterResourcesClient(ep clusterResourcesEndpoint) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ep.Insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // same switch the provider offers (proxmoxve:insecure)
	}
	return &http.Client{Transport: transport}
}
