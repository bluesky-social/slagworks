//go:build integration

package netboxtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/netbox"
)

// SeedSite creates a NetBox site with namespaced slug + name.
// Registers cleanup to delete on test exit.
func (e *Env) SeedSite(name string) *netbox.Site {
	e.t.Helper()
	ctx := context.Background()

	slug := e.Namespace(name)
	body := map[string]any{
		"name":   slug,
		"slug":   slug,
		"status": "active",
	}
	var site netbox.Site
	if err := e.NetBox.Post(ctx, "/api/dcim/sites/", body, &site); err != nil {
		e.t.Fatalf("seed site: %v", err)
	}
	e.t.Cleanup(func() {
		_ = e.NetBox.Delete(context.Background(), fmt.Sprintf("/api/dcim/sites/%d/", site.ID))
	})
	return &site
}

// SeedDeviceOpts governs SeedDevice behavior. Fields grow as tests
// require them — keep it minimal until there's a real use case.
type SeedDeviceOpts struct {
	Serial string
	// RoleSlug, when non-empty, attaches the device to the NetBox
	// device-role with that slug instead of the default namespaced test
	// role. Used by tests that must match the production bootstrap's
	// `server` event-rule condition (role.slug=server); the namespaced
	// test role deliberately does not.
	RoleSlug string
}

// SeedDevice creates a NetBox device bound to a site. Ensures a
// fallback Manufacturer + DeviceType + DeviceRole exist (created
// once per Env, cached). Namespaced name. Registers cleanup.
func (e *Env) SeedDevice(site *netbox.Site, opts SeedDeviceOpts) *netbox.Device {
	e.t.Helper()
	ctx := context.Background()

	mfgID := e.ensureManufacturer()
	typeID := e.ensureDeviceType(mfgID)
	roleID := e.ensureDeviceRole()
	if opts.RoleSlug != "" {
		rid, err := e.NetBox.FindDeviceRoleBySlug(ctx, opts.RoleSlug)
		if err != nil {
			e.t.Fatalf("seed device role %q: %v", opts.RoleSlug, err)
		}
		roleID = rid
	}

	name := e.Namespace("host")
	body := map[string]any{
		"name":        name,
		"site":        site.ID,
		"device_type": typeID,
		"role":        roleID,
		"status":      "active",
	}
	if opts.Serial != "" {
		body["serial"] = opts.Serial
	}
	var dev netbox.Device
	if err := e.NetBox.Post(ctx, "/api/dcim/devices/", body, &dev); err != nil {
		e.t.Fatalf("seed device: %v", err)
	}
	e.t.Cleanup(func() {
		_ = e.NetBox.Delete(context.Background(), fmt.Sprintf("/api/dcim/devices/%d/", dev.ID))
	})
	return &dev
}

// SeedPrefix creates a NetBox prefix tagged `slagworks-production` so
// it shows up in the snapshot builder's prefix query. Namespaced.
// Registers cleanup.
func (e *Env) SeedPrefix(site *netbox.Site, cidr string) *netbox.Prefix {
	e.t.Helper()
	ctx := context.Background()

	body := map[string]any{
		"prefix":      cidr,
		"site":        site.ID,
		"description": e.Namespace("test"),
		"tags":        []map[string]string{{"slug": "slagworks-production"}},
	}
	var pfx netbox.Prefix
	if err := e.NetBox.Post(ctx, "/api/ipam/prefixes/", body, &pfx); err != nil {
		e.t.Fatalf("seed prefix: %v", err)
	}
	e.t.Cleanup(func() {
		_ = e.NetBox.Delete(context.Background(), fmt.Sprintf("/api/ipam/prefixes/%d/", pfx.ID))
	})
	return &pfx
}

// SeedSiteWorkerToken mints a bearer token via store.RegisterSiteWorker
// for the given (slug, worker_id), returning the plaintext token.
// Exposed so auth-boundary tests can mint additional tokens.
func (e *Env) SeedSiteWorkerToken(t *testing.T, slug, workerID string) string {
	t.Helper()
	_, token, err := e.Store.RegisterSiteWorker(slug, workerID, workerID+"."+slug)
	if err != nil {
		t.Fatalf("seed site worker token: %v", err)
	}
	return token
}

// Per-Env caches for the "default" NetBox objects devices need
// (manufacturer, device-type, device-role). Created on first seed
// and reused. Stored on the Env (not a package-level map) so there
// is no cross-test leak.
//
// sync.Once guards creation so concurrent SeedDevice calls within
// one test share a single POST per object instead of racing.
type envDefaults struct {
	mfgOnce sync.Once
	mfgID   int
	mfgErr  error

	typeOnce sync.Once
	typeID   int
	typeErr  error

	roleOnce sync.Once
	roleID   int
	roleErr  error
}

func (e *Env) ensureManufacturer() int {
	d := e.defaults
	d.mfgOnce.Do(func() {
		d.mfgID, d.mfgErr = e.postUniqueNested("/api/dcim/manufacturers/", "testmfg")
	})
	if d.mfgErr != nil {
		e.t.Fatalf("ensure manufacturer: %v", d.mfgErr)
	}
	return d.mfgID
}

func (e *Env) ensureDeviceType(mfgID int) int {
	d := e.defaults
	d.typeOnce.Do(func() {
		ctx := context.Background()
		name := e.Namespace("testtype")
		slug := strings.ToLower(name)
		body := map[string]any{
			"model":        name,
			"slug":         slug,
			"manufacturer": mfgID,
		}
		var out struct {
			ID int `json:"id"`
		}
		if err := e.NetBox.Post(ctx, "/api/dcim/device-types/", body, &out); err != nil {
			d.typeErr = err
			return
		}
		d.typeID = out.ID
		e.t.Cleanup(func() {
			_ = e.NetBox.Delete(context.Background(), fmt.Sprintf("/api/dcim/device-types/%d/", out.ID))
		})
	})
	if d.typeErr != nil {
		e.t.Fatalf("ensure device-type: %v", d.typeErr)
	}
	return d.typeID
}

func (e *Env) ensureDeviceRole() int {
	d := e.defaults
	d.roleOnce.Do(func() {
		d.roleID, d.roleErr = e.postUniqueNested("/api/dcim/device-roles/", "testrole")
	})
	if d.roleErr != nil {
		e.t.Fatalf("ensure device-role: %v", d.roleErr)
	}
	return d.roleID
}

// postUniqueNested creates a NetBox object at path with a namespaced
// name+slug (for endpoints that accept the standard shape: name,
// slug). Returns the created ID or an error. Registers cleanup on
// success.
func (e *Env) postUniqueNested(path, baseName string) (int, error) {
	ctx := context.Background()
	name := e.Namespace(baseName)
	slug := strings.ToLower(name)
	body := map[string]any{"name": name, "slug": slug}
	var out struct {
		ID int `json:"id"`
	}
	if err := e.NetBox.Post(ctx, path, body, &out); err != nil {
		return 0, fmt.Errorf("post %s: %w", path, err)
	}
	e.t.Cleanup(func() {
		_ = e.NetBox.Delete(context.Background(), fmt.Sprintf("%s%d/", path, out.ID))
	})
	return out.ID, nil
}
