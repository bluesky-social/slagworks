package inventory

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
)

func collectDisks(r agent.Runner) []DiskInfo {
	if !r.Available("lsblk") {
		return nil
	}
	out, err := r.Output("lsblk", "-Jb", "-o", "NAME,MODEL,SERIAL,SIZE,TRAN,TYPE,WWN,ROTA")
	if err != nil {
		slog.Warn("lsblk failed", "err", err)
		return nil
	}

	var parsed struct {
		Blockdevices []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Serial string `json:"serial"`
			Size   any    `json:"size"`
			Tran   string `json:"tran"`
			Type   string `json:"type"`
			WWN    string `json:"wwn"`
			Rota   *bool  `json:"rota"`
		} `json:"blockdevices"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return nil
	}

	var disks []DiskInfo
	for _, bd := range parsed.Blockdevices {
		if bd.Type != "disk" {
			continue
		}
		if strings.HasPrefix(bd.Name, "loop") || strings.HasPrefix(bd.Name, "ram") {
			continue
		}
		d := DiskInfo{
			Name:       bd.Name,
			Model:      bd.Model,
			Serial:     bd.Serial,
			Transport:  bd.Tran,
			WWN:        bd.WWN,
			Rotational: bd.Rota,
		}
		// lsblk -b outputs size as a number, but JSON encoding varies
		switch v := bd.Size.(type) {
		case float64:
			d.SizeBytes = int64(v)
		case string:
			fmt.Sscanf(v, "%d", &d.SizeBytes)
		}

		// SMART info + health (best effort, skipped when smartctl unavailable)
		smartOut, err := func() (string, error) {
			if !r.Available("smartctl") {
				return "", fmt.Errorf("smartctl not available")
			}
			return r.Output("smartctl", "-i", "-H", "-j", "/dev/"+bd.Name)
		}()
		if err == nil {
			var smart struct {
				SmartStatus struct {
					Passed bool `json:"passed"`
				} `json:"smart_status"`
				ModelName    string `json:"model_name"`
				SerialNumber string `json:"serial_number"`
				FWVersion    string `json:"firmware_version"`
				Vendor       string `json:"vendor"`
			}
			if json.Unmarshal([]byte(smartOut), &smart) == nil {
				passed := smart.SmartStatus.Passed
				d.SMARTHealthy = &passed
				if smart.FWVersion != "" {
					d.FWVersion = smart.FWVersion
				}
				if smart.Vendor != "" {
					d.Manufacturer = smart.Vendor
				}
			}
		}

		disks = append(disks, d)
	}
	return disks
}
