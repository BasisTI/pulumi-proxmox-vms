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
