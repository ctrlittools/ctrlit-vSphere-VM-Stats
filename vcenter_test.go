package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"testing"

	"github.com/vmware/govmomi/simulator"
)

func TestVC(t *testing.T) {
	m := simulator.VPX()
	defer m.Remove()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	m.Service.TLS = new(tls.Config)
	s := m.Service.NewServer()
	defer s.Close()
	pass, _ := s.URL.User.Password()
	vc := NewVC(VCConfig{Server: s.URL.Host, User: s.URL.User.Username(), Password: pass, Insecure: true})
	ctx := context.Background()
	n, err := vc.RefreshNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("VMs: %d", n)
	e, dup, ok, _ := vc.Match(ctx, []string{"", "Some text", "DC0_H0_VM0 (orphaned)"})
	if !ok {
		t.Fatal("no match")
	}
	info, qs, err := vc.Info(ctx, e, dup)
	if err != nil {
		t.Fatal(err)
	}
	if err := vc.Stats(ctx, e.Ref, info.NumCPU, &qs); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"vm": info, "vs": qs})
	t.Logf("%s", b)
	if _, _, ok, _ := vc.Match(ctx, []string{"DC0_H0"}); ok {
		t.Fatal("host matched as VM")
	}
	// session expiry -> reconnect
	vc.mu.Lock()
	c := vc.client
	vc.mu.Unlock()
	_ = c.SessionManager.Logout(ctx)
	if err := vc.Stats(ctx, e.Ref, info.NumCPU, &qs); err != nil {
		t.Fatal("after logout:", err)
	}
	bad := NewVC(VCConfig{Server: s.URL.Host, User: "x", Password: "y", Insecure: true})
	_, err = bad.RefreshNames(ctx)
	t.Log("bad login:", err)
	sec := NewVC(VCConfig{Server: s.URL.Host, User: "x", Password: "y"})
	_, err = sec.RefreshNames(ctx)
	t.Log("cert:", err)
}
