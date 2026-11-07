package proxmox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// fakePVE is a minimal Proxmox VE API. Handlers are keyed by the path below /api2/json.
type fakePVE struct {
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]any // path -> the "data" value, or an int for an HTTP error status
	requests []string
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{handlers: map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		f.requests = append(f.requests, r.Method+" "+path)
		h, ok := f.handlers[path]
		if !ok {
			http.Error(w, "no such path "+path, http.StatusNotFound)
			return
		}
		if code, isCode := h.(int); isCode {
			http.Error(w, "error", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": h})
	}))
	t.Cleanup(f.srv.Close)
	f.handlers["/version"] = map[string]any{"version": "9.2.0", "release": "9.2", "repoid": "abc"}
	return f
}

func (f *fakePVE) url() string { return f.srv.URL + "/api2/json" }

func (f *fakePVE) set(path string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[path] = data
}

func (f *fakePVE) client() *proxmox.Client {
	return proxmox.NewClient(f.url(), proxmox.WithAPIToken("test@pve!t", "secret"))
}

// addVM registers a VM in the cluster resources and gives it status, config and agent data.
func (f *fakePVE) addVM(node string, vmid int, name, status string, config map[string]any, agent []map[string]any) {
	f.mu.Lock()
	res, _ := f.handlers["/cluster/resources"].([]any)
	res = append(res, map[string]any{"id": "qemu/" + itoa(vmid), "type": "qemu", "vmid": vmid, "node": node, "name": name, "status": status, "template": 0})
	f.handlers["/cluster/resources"] = res
	f.mu.Unlock()

	f.set("/nodes/"+node+"/status", map[string]any{"uptime": 100})
	base := "/nodes/" + node + "/qemu/" + itoa(vmid)
	f.set(base+"/status/current", map[string]any{"vmid": vmid, "name": name, "status": status, "cpus": 4, "maxmem": 8 << 30})
	if config == nil {
		config = map[string]any{}
	}
	config["name"] = name
	f.set(base+"/config", config)
	if agent != nil {
		f.set(base+"/agent/network-get-interfaces", map[string]any{"result": agent})
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
