package model

import "testing"

func TestInMemoryProvider_UnknownMAC_MustNotDeploy(t *testing.T) {
	// An unknown MAC must NEVER receive BootDeploy. Same safety invariant
	// as the Cache test — an unregistered machine must not be PXE booted.
	p := NewInMemoryProvider()
	got := p.BootActionForMAC("52:54:00:aa:bb:01")
	if got == BootDeploy {
		t.Fatal("SAFETY: unknown MAC returned BootDeploy — this would PXE boot and potentially wipe an unregistered machine")
	}
	if got == BootCommission {
		t.Fatal("SAFETY: unknown MAC returned BootCommission — this would PXE boot an unregistered machine")
	}
	if got != BootLocal {
		t.Fatalf("expected BootLocal for unknown MAC, got %d", got)
	}
}
