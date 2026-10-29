package proxmox_vms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		func(key string) (string, bool) {
			v, err := pconfig.Try(ctx, "proxmoxve:"+key)
			return v, err == nil
		},
		os.Getenv,
	)
}

// resolveClusterResources mirrors the SDK provider of bpg/terraform-provider-proxmox, the one
// that serves the VMs data source. pulumi-proxmoxve v7.13.0 depends on bpg v0.95.0
// (provider/go.mod:8); lines below are from that tag, and v0.118.1 has the same code.
// Each item starts from the environment and the stack configuration overrides it
// (proxmoxtf/provider/provider.go:53-61 environment, :63-69 and :83-85 configuration):
//   - endpoint: PROXMOX_VE_ENDPOINT, then PM_VE_ENDPOINT (utils.GetAnyStringEnv, first
//     non-empty); proxmoxve:endpoint wins when non-empty (d.GetOk).
//   - apiToken: same for PROXMOX_VE_API_TOKEN / PM_VE_API_TOKEN, but the configuration uses
//     d.GetOkExists, so a token present in the stack, even empty, wins over the environment.
//     An empty token means no token: the provider then authenticates by username/password.
//   - insecure: utils.GetAnyBoolEnv (utils/env.go:26-37) is true only for exactly "true" or
//     "1" (not "TRUE", "t"); proxmoxve:insecure uses d.GetOk, which skips the zero value, so
//     "false" in the stack does not turn off a true environment variable.
//
// A stack that carries only proxmoxve:endpoint, with the token in PROXMOX_VE_API_TOKEN, as the
// iac-runner delivers it, resolves to the endpoint of the stack and the token of the environment.
func resolveClusterResources(cfg func(key string) (string, bool), env func(name string) string) (ep clusterResourcesEndpoint, ok bool, reason string) {
	anyEnv := func(names ...string) string {
		for _, n := range names {
			if v := env(n); v != "" {
				return v
			}
		}
		return ""
	}

	ep.URL = anyEnv("PROXMOX_VE_ENDPOINT", "PM_VE_ENDPOINT")
	if v, _ := cfg("endpoint"); v != "" {
		ep.URL = v
	}

	ep.ApiToken = anyEnv("PROXMOX_VE_API_TOKEN", "PM_VE_API_TOKEN")
	if v, present := cfg("apiToken"); present {
		ep.ApiToken = v
	}

	envInsecure := anyEnv("PROXMOX_VE_INSECURE", "PM_VE_INSECURE")
	ep.Insecure = envInsecure == "true" || envInsecure == "1"
	if v, _ := cfg("insecure"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			ep.Insecure = true
		}
	}

	switch {
	case ep.URL == "":
		return clusterResourcesEndpoint{}, false, "no endpoint (proxmoxve:endpoint or PROXMOX_VE_ENDPOINT)"
	case ep.ApiToken == "":
		return clusterResourcesEndpoint{}, false, "no API token (proxmoxve:apiToken or PROXMOX_VE_API_TOKEN); the provider authenticates by username/password or ticket"
	}
	return ep, true, ""
}

// apiBaseURL normalizes the endpoint like the provider client (proxmox/api/client.go:101-105
// in v0.95.0): the path is dropped, so "https://pve:8006", ".../", ".../api2/json" and
// ".../api2/json/" all address the same API.
func apiBaseURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Proxmox endpoint: %w", err)
	}
	u.Path = ""
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
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

	base, err := apiBaseURL(ep.URL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api2/json/cluster/resources?type=vm", nil)
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
