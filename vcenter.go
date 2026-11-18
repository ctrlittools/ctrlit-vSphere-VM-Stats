package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// VCConfig is what's needed to log in to vCenter.
type VCConfig struct {
	Server   string
	User     string
	Password string
	Insecure bool
}

type vmEntry struct {
	Name string
	Ref  types.ManagedObjectReference
}

// VMInfo is sent to the popup as "vm".
type VMInfo struct {
	Found         bool     `json:"found"`
	Name          string   `json:"name"`
	ID            string   `json:"id"`
	PowerState    string   `json:"powerState"`
	IsTemplate    bool     `json:"isTemplate"`
	Host          string   `json:"host"`
	NumCPU        int32    `json:"numCpu"`
	MemoryMB      int32    `json:"memoryMB"`
	GuestOS       string   `json:"guestOS"`
	GuestFamily   string   `json:"guestFamily"`
	GuestHostName string   `json:"guestHostName"`
	IP            string   `json:"ip"`
	ToolsRunning  string   `json:"toolsRunning"`
	Duplicates    int      `json:"duplicates"`
	Targets       []string `json:"-"` // addresses to try for WinRM
}

type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// VSphereStats is sent to the popup as "vs".
type VSphereStats struct {
	IntervalSec int     `json:"intervalSec"`
	CPU         []Point `json:"cpu"`
	Mem         []Point `json:"mem"`
	Net         []Point `json:"net"`
	Ready       []Point `json:"ready"`
	CPUMHz      int32   `json:"cpuMHz"`
	ActiveMB    int32   `json:"activeMB"`
	ConsumedMB  int32   `json:"consumedMB"`
}

// VC is a long-lived vCenter connection with a cache of VM names.
type VC struct {
	cfg VCConfig

	mu      sync.Mutex
	client  *govmomi.Client
	perf    *performance.Manager
	hosts   map[string]string
	names   map[string][]vmEntry // lower-case name -> VMs
	namesAt time.Time
	loading sync.Mutex
}

func NewVC(cfg VCConfig) *VC {
	return &VC{cfg: cfg, hosts: map[string]string{}}
}

// vSphere stores a few characters in names escaped; the vSphere Client shows them plainly.
var nameUnescaper = strings.NewReplacer("%2f", "/", "%2F", "/", "%5c", `\`, "%5C", `\`, "%25", "%")

func (v *VC) connect(ctx context.Context) (*govmomi.Client, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.client != nil {
		return v.client, nil
	}
	if strings.TrimSpace(v.cfg.Server) == "" {
		return nil, errors.New("vCenter isn't set up yet - open Settings from the tray icon")
	}
	u, err := soap.ParseURL(strings.TrimSpace(v.cfg.Server))
	if err != nil {
		return nil, fmt.Errorf("bad vCenter address: %w", err)
	}
	u.User = url.UserPassword(v.cfg.User, v.cfg.Password)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := govmomi.NewClient(cctx, u, v.cfg.Insecure)
	if err != nil {
		return nil, friendlyVCError(err)
	}
	v.client = c
	v.perf = performance.NewManager(c.Client)
	return c, nil
}

func friendlyVCError(err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate"):
		return fmt.Errorf("vCenter's certificate isn't trusted by this PC. Tick \"Accept untrusted certificate\" in Settings. (%v)", err)
	case strings.Contains(strings.ToLower(s), "incorrect user name or password") || strings.Contains(s, "InvalidLogin"):
		return errors.New("vCenter rejected the user name or password")
	}
	return err
}

func (v *VC) reset() {
	v.mu.Lock()
	c := v.client
	v.client = nil
	v.perf = nil
	v.mu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.Logout(ctx)
		cancel()
	}
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not authenticated") || strings.Contains(s, "notauthenticated")
}

// do runs f with a connected client; it logs in again once if the session has expired
// and drops the connection on network errors so the next call reconnects.
func (v *VC) do(ctx context.Context, f func(c *govmomi.Client, pm *performance.Manager) error) error {
	for attempt := 0; attempt < 2; attempt++ {
		c, err := v.connect(ctx)
		if err != nil {
			return err
		}
		v.mu.Lock()
		pm := v.perf
		v.mu.Unlock()
		err = f(c, pm)
		if err == nil {
			return nil
		}
		if isAuthError(err) && attempt == 0 {
			v.reset()
			continue
		}
		if !soap.IsSoapFault(err) && !soap.IsVimFault(err) && ctx.Err() == nil {
			v.reset()
		}
		return err
	}
	return nil
}

// Close logs out.
func (v *VC) Close() { v.reset() }

// RefreshNames reloads the list of VM names.
func (v *VC) RefreshNames(ctx context.Context) (int, error) {
	v.loading.Lock()
	defer v.loading.Unlock()
	names := map[string][]vmEntry{}
	err := v.do(ctx, func(c *govmomi.Client, _ *performance.Manager) error {
		m := view.NewManager(c.Client)
		cv, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
		if err != nil {
			return err
		}
		defer func() { _ = cv.Destroy(context.Background()) }()
		var vms []mo.VirtualMachine
		if err := cv.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name"}, &vms); err != nil {
			return err
		}
		for _, vm := range vms {
			n := nameUnescaper.Replace(vm.Name)
			k := strings.ToLower(n)
			names[k] = append(names[k], vmEntry{Name: n, Ref: vm.Self})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	v.mu.Lock()
	v.names = names
	v.namesAt = time.Now()
	v.mu.Unlock()
	return len(names), nil
}

// candidateVariants yields the text itself plus trimmed forms, e.g. "WIN01 (orphaned)" -> "WIN01".
func candidateVariants(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 {
		return nil
	}
	out := []string{s}
	if i := strings.IndexAny(s, "\r\n\t"); i > 0 {
		out = append(out, strings.TrimSpace(s[:i]))
	}
	for _, sep := range []string{" (", ", ", " - ", " | "} {
		if i := strings.Index(s, sep); i > 0 {
			out = append(out, strings.TrimSpace(s[:i]))
		}
	}
	return out
}

// Match returns the first candidate text that is a VM name. err is set only when vCenter can't be reached.
func (v *VC) Match(ctx context.Context, candidates []string) (vmEntry, int, bool, error) {
	v.mu.Lock()
	empty := v.names == nil
	stale := time.Since(v.namesAt) > 2*time.Minute
	v.mu.Unlock()
	if empty {
		if _, err := v.RefreshNames(ctx); err != nil {
			return vmEntry{}, 0, false, err
		}
	} else if stale {
		go func() { _, _ = v.RefreshNames(context.Background()) }()
	}
	if e, n, ok := v.lookup(candidates); ok {
		return e, n, true, nil
	}
	// Not found: maybe a brand-new VM. Reload once if the list is older than 15 s.
	v.mu.Lock()
	age := time.Since(v.namesAt)
	v.mu.Unlock()
	if age > 15*time.Second && hasText(candidates) {
		if _, err := v.RefreshNames(ctx); err == nil {
			e, n, ok := v.lookup(candidates)
			return e, n, ok, nil
		}
	}
	return vmEntry{}, 0, false, nil
}

func hasText(c []string) bool {
	for _, s := range c {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

func (v *VC) lookup(candidates []string) (vmEntry, int, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range candidates {
		for _, s := range candidateVariants(c) {
			if list := v.names[strings.ToLower(s)]; len(list) > 0 {
				return list[0], len(list), true
			}
		}
	}
	return vmEntry{}, 0, false
}

var vmProps = []string{
	"name", "runtime.powerState", "runtime.host", "config.hardware.numCPU", "config.hardware.memoryMB",
	"config.guestFullName", "config.template", "guest", "summary.quickStats",
}

// Info reads the VM's details and current quick stats.
func (v *VC) Info(ctx context.Context, e vmEntry, matches int) (VMInfo, VSphereStats, error) {
	var vm mo.VirtualMachine
	info := VMInfo{}
	qs := VSphereStats{}
	err := v.do(ctx, func(c *govmomi.Client, _ *performance.Manager) error {
		pc := property.DefaultCollector(c.Client)
		if err := pc.RetrieveOne(ctx, e.Ref, vmProps, &vm); err != nil {
			return err
		}
		info.Host = v.hostName(ctx, pc, vm.Runtime.Host)
		return nil
	})
	if err != nil {
		return info, qs, err
	}
	info.Found = true
	info.Name = nameUnescaper.Replace(vm.Name)
	info.ID = e.Ref.Value
	info.PowerState = string(vm.Runtime.PowerState)
	info.Duplicates = matches - 1
	if vm.Config != nil {
		info.IsTemplate = vm.Config.Template
		info.NumCPU = vm.Config.Hardware.NumCPU
		info.MemoryMB = vm.Config.Hardware.MemoryMB
		info.GuestOS = vm.Config.GuestFullName
	}
	if g := vm.Guest; g != nil {
		if g.GuestFullName != "" {
			info.GuestOS = g.GuestFullName
		}
		info.GuestFamily = g.GuestFamily
		info.GuestHostName = g.HostName
		info.IP = g.IpAddress
		info.ToolsRunning = g.ToolsRunningStatus
		info.Targets = guestTargets(g)
	}
	q := vm.Summary.QuickStats
	qs.CPUMHz, qs.ActiveMB, qs.ConsumedMB = q.OverallCpuUsage, q.GuestMemoryUsage, q.HostMemoryUsage
	return info, qs, nil
}

func guestTargets(g *types.GuestInfo) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, x := range out {
			if strings.EqualFold(x, s) {
				return
			}
		}
		out = append(out, s)
	}
	add(g.HostName)
	for _, n := range g.Net {
		for _, ip := range n.IpAddress {
			if isIPv4(ip) && !strings.HasPrefix(ip, "169.254.") {
				add(ip)
			}
		}
	}
	if isIPv4(g.IpAddress) {
		add(g.IpAddress)
	}
	return out
}

func isIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

func (v *VC) hostName(ctx context.Context, pc *property.Collector, ref *types.ManagedObjectReference) string {
	if ref == nil {
		return ""
	}
	v.mu.Lock()
	n, ok := v.hosts[ref.Value]
	v.mu.Unlock()
	if ok {
		return n
	}
	var h mo.HostSystem
	if err := pc.RetrieveOne(ctx, *ref, []string{"name"}, &h); err != nil {
		return ""
	}
	v.mu.Lock()
	v.hosts[ref.Value] = h.Name
	v.mu.Unlock()
	return h.Name
}

var perfMetrics = []string{"cpu.usage.average", "mem.usage.average", "net.usage.average", "cpu.ready.summation"}

// Stats returns the last 5 minutes of realtime (20 s) samples for the whole VM.
func (v *VC) Stats(ctx context.Context, ref types.ManagedObjectReference, numCPU int32, st *VSphereStats) error {
	return v.do(ctx, func(c *govmomi.Client, pm *performance.Manager) error {
		spec := types.PerfQuerySpec{
			MaxSample:  15,
			IntervalId: 20,
			MetricId:   []types.PerfMetricId{{Instance: ""}}, // "" = whole-VM aggregate
		}
		sample, err := pm.SampleByName(ctx, spec, perfMetrics, []types.ManagedObjectReference{ref})
		if err != nil {
			return err
		}
		series, err := pm.ToMetricSeries(ctx, sample)
		if err != nil {
			return err
		}
		st.IntervalSec = 20
		st.CPU, st.Mem, st.Net, st.Ready = []Point{}, []Point{}, []Point{}, []Point{}
		vcpus := float64(numCPU)
		if vcpus < 1 {
			vcpus = 1
		}
		for _, em := range series {
			for _, s := range em.Value {
				if s.Instance != "" {
					continue
				}
				for i, raw := range s.Value {
					if i >= len(em.SampleInfo) || raw < 0 {
						continue
					}
					t := em.SampleInfo[i].Timestamp.UnixMilli()
					interval := float64(em.SampleInfo[i].Interval)
					if interval <= 0 {
						interval = 20
					}
					switch s.Name {
					case "cpu.usage.average": // hundredths of a percent
						st.CPU = append(st.CPU, Point{t, round1(float64(raw) / 100)})
					case "mem.usage.average":
						st.Mem = append(st.Mem, Point{t, round1(float64(raw) / 100)})
					case "net.usage.average": // KBps
						st.Net = append(st.Net, Point{t, float64(raw)})
					case "cpu.ready.summation": // ms waiting per interval, summed over vCPUs
						st.Ready = append(st.Ready, Point{t, round2(float64(raw) / (interval * 1000) * 100 / vcpus)})
					}
				}
			}
		}
		for _, p := range [][]Point{st.CPU, st.Mem, st.Net, st.Ready} {
			sort.Slice(p, func(i, j int) bool { return p[i].T < p[j].T })
		}
		return nil
	})
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
