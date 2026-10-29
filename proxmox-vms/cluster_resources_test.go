package proxmox_vms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const clusterResourcesBody = `{"data":[
 {"id":"qemu/117","type":"qemu","vmid":117,"name":"chur","node":"mountainview","template":0},
 {"id":"qemu/116","type":"qemu","vmid":116,"name":"thun","node":"siliconvalley"},
 {"id":"qemu/102","type":"qemu","vmid":102,"name":"chur","node":"siliconvalley","template":1},
 {"id":"lxc/200","type":"lxc","vmid":200,"name":"nova","node":"redwood"}]}`

func clusterServer(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestListClusterVmsFeedsNodesByName(t *testing.T) {
	srv, req := clusterServer(t, http.StatusOK, clusterResourcesBody)
	ep := clusterResourcesEndpoint{URL: srv.URL + "/", ApiToken: "user@pve!tok=secret"}

	found, err := listClusterVms(context.Background(), srv.Client(), ep)
	if err != nil {
		t.Fatalf("listClusterVms() error = %v", err)
	}
	if req.URL.Path != "/api2/json/cluster/resources" || req.URL.Query().Get("type") != "vm" {
		t.Fatalf("requested %s, want /api2/json/cluster/resources?type=vm", req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "PVEAPIToken=user@pve!tok=secret" {
		t.Fatalf("Authorization = %q", got)
	}

	// VM in another node, template left out, LXC ignored, VM not created yet absent.
	got, err := nodesByName(found, []VmData{{Name: "chur"}, {Name: "thun"}, {Name: "nova"}, {Name: "nunca"}})
	if err != nil {
		t.Fatalf("nodesByName() error = %v", err)
	}
	want := map[string]string{"chur": "mountainview", "thun": "siliconvalley"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
}

func TestListClusterVmsKeepsDuplicateNamesAnError(t *testing.T) {
	body := `{"data":[{"type":"qemu","vmid":117,"name":"chur","node":"a"},{"type":"qemu","vmid":140,"name":"chur","node":"b"}]}`
	srv, _ := clusterServer(t, http.StatusOK, body)
	found, err := listClusterVms(context.Background(), srv.Client(), clusterResourcesEndpoint{URL: srv.URL, ApiToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodesByName(found, []VmData{{Name: "chur"}}); err == nil || !strings.Contains(err.Error(), "not unique") {
		t.Fatalf("err = %v, want duplicate-name error", err)
	}
}

func TestListClusterVmsReportsHttpErrorWithoutToken(t *testing.T) {
	srv, _ := clusterServer(t, http.StatusUnauthorized, "no")
	_, err := listClusterVms(context.Background(), srv.Client(), clusterResourcesEndpoint{URL: srv.URL, ApiToken: "user@pve!tok=secret"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func fakeLookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeConfig is a stack configuration: a key present in the map is set, even when empty.
func fakeConfig(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolveClusterResourcesTokenFromEnvEndpointFromStack(t *testing.T) {
	// The build-agent case: only proxmoxve:endpoint in the stack, token delivered by the iac-runner.
	ep, ok, reason := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://pve:8006/"}),
		fakeLookup(map[string]string{"PROXMOX_VE_API_TOKEN": "u@pve!t=s", "PROXMOX_VE_INSECURE": "true"}),
	)
	if !ok || reason != "" {
		t.Fatalf("ok = %v, reason = %q", ok, reason)
	}
	want := clusterResourcesEndpoint{URL: "https://pve:8006/", ApiToken: "u@pve!t=s", Insecure: true}
	if ep != want {
		t.Fatalf("ep = %+v, want %+v", ep, want)
	}
}

func TestResolveClusterResourcesStackWinsOverEnvironment(t *testing.T) {
	ep, ok, _ := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://stack", "apiToken": "stack-token"}),
		fakeLookup(map[string]string{"PROXMOX_VE_ENDPOINT": "https://env", "PROXMOX_VE_API_TOKEN": "env-token", "PM_VE_ENDPOINT": "https://alias"}),
	)
	if !ok || ep.URL != "https://stack" || ep.ApiToken != "stack-token" {
		t.Fatalf("ep = %+v, ok = %v", ep, ok)
	}
}

func TestResolveClusterResourcesEndpointFromEnvironmentAndAlias(t *testing.T) {
	ep, ok, _ := resolveClusterResources(fakeConfig(nil),
		fakeLookup(map[string]string{"PM_VE_ENDPOINT": "https://alias", "PROXMOX_VE_API_TOKEN": "t"}))
	if !ok || ep.URL != "https://alias" || ep.Insecure {
		t.Fatalf("ep = %+v, ok = %v", ep, ok)
	}
}

func TestResolveClusterResourcesWithoutTokenFallsBack(t *testing.T) {
	// kubernetes and docker stacks: username/password, no token anywhere.
	_, ok, reason := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://pve", "username": "root@pam"}),
		fakeLookup(map[string]string{"PROXMOX_VE_PASSWORD": "x"}))
	if ok || !strings.Contains(reason, "API token") {
		t.Fatalf("ok = %v, reason = %q", ok, reason)
	}
	_, ok, reason = resolveClusterResources(fakeConfig(nil), fakeLookup(map[string]string{"PROXMOX_VE_API_TOKEN": "t"}))
	if ok || !strings.Contains(reason, "endpoint") {
		t.Fatalf("ok = %v, reason = %q", ok, reason)
	}
}

func TestResolveClusterResourcesInsecureFalseInStackKeepsTrueEnvironment(t *testing.T) {
	// d.GetOk skips the zero value: false in the stack does not override true in the environment.
	ep, ok, _ := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://pve", "insecure": "false"}),
		fakeLookup(map[string]string{"PROXMOX_VE_API_TOKEN": "t", "PROXMOX_VE_INSECURE": "true"}))
	if !ok || !ep.Insecure {
		t.Fatalf("ep = %+v, ok = %v, want insecure kept from the environment", ep, ok)
	}
}

func TestResolveClusterResourcesInsecureEnvironmentOnlyTrueOrOne(t *testing.T) {
	for env, want := range map[string]bool{"true": true, "1": true, "TRUE": false, "True": false, "t": false, "yes": false, "false": false} {
		ep, _, _ := resolveClusterResources(
			fakeConfig(map[string]string{"endpoint": "https://pve", "apiToken": "t"}),
			fakeLookup(map[string]string{"PROXMOX_VE_INSECURE": env}))
		if ep.Insecure != want {
			t.Errorf("PROXMOX_VE_INSECURE=%q: insecure = %v, want %v", env, ep.Insecure, want)
		}
	}
}

func TestResolveClusterResourcesInsecureTrueInStack(t *testing.T) {
	ep, _, _ := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://pve", "apiToken": "t", "insecure": "true"}), fakeLookup(nil))
	if !ep.Insecure {
		t.Fatal("insecure=true in the stack must be honored")
	}
}

func TestResolveClusterResourcesEmptyTokenInStackOverridesEnvironment(t *testing.T) {
	// d.GetOkExists: a token present in the stack wins even when empty, so the provider has no
	// token and authenticates by username/password. The component must fall back too.
	_, ok, reason := resolveClusterResources(
		fakeConfig(map[string]string{"endpoint": "https://pve", "apiToken": ""}),
		fakeLookup(map[string]string{"PROXMOX_VE_API_TOKEN": "env-token"}))
	if ok || !strings.Contains(reason, "API token") {
		t.Fatalf("ok = %v, reason = %q, want fallback", ok, reason)
	}
}

func TestListClusterVmsAcceptsEveryEndpointForm(t *testing.T) {
	for _, suffix := range []string{"", "/", "/api2/json", "/api2/json/"} {
		srv, req := clusterServer(t, http.StatusOK, clusterResourcesBody)
		_, err := listClusterVms(context.Background(), srv.Client(), clusterResourcesEndpoint{URL: srv.URL + suffix, ApiToken: "t"})
		if err != nil {
			t.Fatalf("endpoint suffix %q: %v", suffix, err)
		}
		if req.URL.Path != "/api2/json/cluster/resources" {
			t.Fatalf("endpoint suffix %q: requested %s", suffix, req.URL.Path)
		}
	}
}
