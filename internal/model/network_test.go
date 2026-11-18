package model

import "testing"

func TestIsBMCUSBNetworkDriver(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		driver string
		want   bool
	}{
		{driver: "rndis_host", want: true},
		{driver: " RNDIS_HOST ", want: true},
		{driver: "cdc_ether", want: true},
		{driver: "mlx5_core", want: false},
		{driver: "", want: false},
	} {
		if got := IsBMCUSBNetworkDriver(tc.driver); got != tc.want {
			t.Errorf("IsBMCUSBNetworkDriver(%q) = %v, want %v", tc.driver, got, tc.want)
		}
	}
}
