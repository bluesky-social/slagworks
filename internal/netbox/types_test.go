package netbox

import (
	"encoding/json"
	"testing"
)

func TestDeviceUnmarshalOOBIP(t *testing.T) {
	// NetBox's brief IP shape includes family as a choice object. The local
	// type intentionally models only fields Slagworks reads; unknown fields
	// must not prevent production device-list responses from decoding.
	const raw = `{
		"id": 42,
		"name": "cpu1.pop1",
		"oob_ip": {
			"id": 99,
			"address": "10.254.241.7/22",
			"family": {"value": 4, "label": "IPv4"}
		}
	}`

	var device Device
	if err := json.Unmarshal([]byte(raw), &device); err != nil {
		t.Fatal(err)
	}
	if device.OOBIP == nil {
		t.Fatal("OOBIP is nil")
	}
	if device.OOBIP.ID != 99 || device.OOBIP.Address != "10.254.241.7/22" {
		t.Fatalf("OOBIP = %+v", device.OOBIP)
	}
}
