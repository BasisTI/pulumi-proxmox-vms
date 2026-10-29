package proxmox_vms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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

// clusterResourcesFromConfig reads the credential the way the provider does (see
// resolveClusterResources). ok is false when the provider is not authenticated by API token
// (username/password, ticket): the ticket flow is not reimplemented here, and the caller
// falls back to the provider data source. reason says what was missing.
func clusterResourcesFromConfig(ctx *pulumi.Context) (ep clusterResourcesEndpoint, ok bool, reason string) {
	return resolveClusterResources(
		func(key string) string { return pconfig.Get(ctx, "proxmoxve:"+key) },
		os.Getenv,
	)
}

// resolveClusterResources mirrors bpg/terraform-provider-proxmox v0.118.1: each item starts from
// the environment variable and the stack configuration overrides it when set
// (proxmoxtf/provider/provider.go:53-69 and :83-85 for the SDK provider, which serves the VMs
// data source; fwprovider/provider.go:386-404 for the framework provider). The PM_VE_* names
// are aliases accepted by the SDK provider, after PROXMOX_VE_* (utils/env.go GetAnyStringEnv).
//
// A stack that carries only proxmoxve:endpoint, with the token in PROXMOX_VE_API_TOKEN, as the
// iac-runner delivers it, resolves to the endpoint of the stack and the token of the environment.
func resolveClusterResources(cfg func(key string) string, env func(name string) string) (ep clusterResourcesEndpoint, ok bool, reason string) {
	anyEnv := func(names ...string) string {
		for _, n := range names {
			if v := env(n); v != "" {
				return v
			}
		}
		return ""
	}
	pick := func(key string, names ...string) string {
		if v := cfg(key); v != "" {
			return v
		}
		return anyEnv(names...)
	}

	ep.URL = pick("endpoint", "PROXMOX_VE_ENDPOINT", "PM_VE_ENDPOINT")
	ep.ApiToken = pick("apiToken", "PROXMOX_VE_API_TOKEN", "PM_VE_API_TOKEN")
	insecure := pick("insecure", "PROXMOX_VE_INSECURE", "PM_VE_INSECURE")
	ep.Insecure, _ = strconv.ParseBool(insecure)

	switch {
	case ep.URL == "":
		return clusterResourcesEndpoint{}, false, "no endpoint (proxmoxve:endpoint or PROXMOX_VE_ENDPOINT)"
	case ep.ApiToken == "":
		return clusterResourcesEndpoint{}, false, "no API token (proxmoxve:apiToken or PROXMOX_VE_API_TOKEN); the provider authenticates by username/password or ticket"
	}
	return ep, true, ""
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
