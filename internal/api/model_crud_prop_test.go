package api

// The CRUD and read-only actions of the model-based operator API test
// (model_prop_test.go): SSH keys, images, storage and network scripts, site
// configs, site workers, and the read routes that need NetBox or the hub.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func pmJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// ---------------------------------------------------------------- ssh keys

func (r *pmRun) addKey(i int, comment string) {
	r.t.Helper()
	key := propSSHKey(i)
	resp := r.call("ssh-key add", propReq{method: http.MethodPost, path: "/api/v1/ssh-keys",
		body: pmJSON(map[string]string{"public_key": key, "comment": comment})})
	for _, k := range r.m.keys {
		if k.key == key {
			r.expect("ssh-key add (duplicate)", resp, http.StatusConflict)
			return
		}
	}
	r.expect("ssh-key add", resp, http.StatusCreated)
	var got model.SSHKey
	if err := resp.json(&got); err != nil || got.ID == uuid.Nil || got.PublicKey != key {
		r.failf("ssh-key add: bad body %s (%v)", resp, err)
	}
	r.m.keys = append(r.m.keys, &pmKey{id: got.ID, key: key, comment: comment})
}

func (r *pmRun) sshKey() {
	switch rapid.SampledFrom([]string{"add", "add", "add-bad", "delete", "delete", "list"}).Draw(r.t, "keyOp") {
	case "add":
		r.addKey(rapid.IntRange(0, 2).Draw(r.t, "key"), rapid.SampledFrom([]string{"", "alice", "bob"}).Draw(r.t, "comment"))
	case "add-bad":
		body := rapid.SampledFrom([]string{
			`{"public_key":""}`, `{"comment":"x"}`, `{"public_key":"not a key"}`,
			`{"public_key":"ssh-ed25519 AAAA"}`, `{"public_key":42}`, `{`, ``,
		}).Draw(r.t, "badKey")
		resp := r.call("ssh-key add bad", propReq{method: http.MethodPost, path: "/api/v1/ssh-keys", body: body})
		r.expect("ssh-key add bad", resp, http.StatusBadRequest)
	case "delete":
		kind := rapid.SampledFrom([]string{"key", "key", "missing", "bad"}).Draw(r.t, "delKind")
		if kind == "key" && len(r.m.keys) == 0 {
			kind = "missing"
		}
		switch kind {
		case "key":
			i := rapid.IntRange(0, len(r.m.keys)-1).Draw(r.t, "delKey")
			resp := r.call("ssh-key delete", propReq{method: http.MethodDelete, path: "/api/v1/ssh-keys/" + r.m.keys[i].id.String()})
			r.expect("ssh-key delete", resp, http.StatusNoContent)
			r.m.keys = append(r.m.keys[:i], r.m.keys[i+1:]...)
		case "missing":
			resp := r.call("ssh-key delete", propReq{method: http.MethodDelete, path: "/api/v1/ssh-keys/" + pmMissingUUID})
			r.expect("ssh-key delete", resp, http.StatusNotFound)
		default:
			resp := r.call("ssh-key delete", propReq{method: http.MethodDelete, path: "/api/v1/ssh-keys/nope"})
			r.expect("ssh-key delete", resp, http.StatusBadRequest)
		}
	default:
		resp := r.call("ssh-key list", propReq{method: http.MethodGet, path: "/api/v1/ssh-keys"})
		r.expect("ssh-key list", resp, http.StatusOK)
		var got []model.SSHKey
		if err := resp.json(&got); err != nil {
			r.failf("ssh-key list: %v", err)
		}
		if len(got) != len(r.m.keys) {
			r.failf("ssh-key list: %d keys, model has %d", len(got), len(r.m.keys))
		}
		for i, g := range got { // created_at asc: the order deployed hosts receive them
			w := r.m.keys[i]
			if g.ID != w.id || g.PublicKey != w.key || g.Comment != w.comment {
				r.failf("ssh-key list[%d] = {%s %q}, model has {%s %q}", i, g.ID, g.Comment, w.id, w.comment)
			}
		}
	}
}

// ---------------------------------------------------------------- images

// pmUUIDName is a legal image name that also parses as a UUID.
const pmUUIDName = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

// pmImageNameOK is the documented image-name rule: lowercase letters,
// digits, '.', '_' and '-', starting with a letter or digit, and not
// "systemd-bootx64.efi". The name becomes an entry of the images dir on
// every site worker, so "..", a slash, an upper-case twin of another name or
// the name of the bootloader in that dir must not be accepted.
func pmImageNameOK(name string) bool {
	if name == "" || name == "." || name == ".." || name == "systemd-bootx64.efi" {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

func (r *pmRun) image() {
	names := []string{"noble-pxe", "noble-pxe", "img-b", "IMG-B", pmUUIDName}
	name := rapid.SampledFrom(names).Draw(r.t, "imgName")
	imgOp := rapid.SampledFrom([]string{"create", "create", "upsert", "upsert", "delete", "delete-id", "list"}).Draw(r.t, "imgOp")
	if imgOp == "create" && rapid.IntRange(0, 9).Draw(r.t, "badImageName") == 0 {
		name = ".."
	}
	if existing := pmSortedKeys(r.m.images); imgOp != "create" && len(existing) > 0 && rapid.IntRange(0, 9).Draw(r.t, "existingImage") < 6 {
		name = rapid.SampledFrom(existing).Draw(r.t, "existingImageName")
	}
	fields := func() map[string]any {
		f := map[string]any{}
		if rapid.IntRange(0, 9).Draw(r.t, "hasVersion") > 0 {
			f["version"] = rapid.SampledFrom([]string{"v1", "v2"}).Draw(r.t, "version")
		}
		if rapid.IntRange(0, 9).Draw(r.t, "hasSHA") > 0 {
			f["sha256"] = rapid.SampledFrom([]string{"aa11", "bb22"}).Draw(r.t, "sha")
		}
		if rapid.IntRange(0, 9).Draw(r.t, "hasURL") > 0 {
			f["url"] = rapid.SampledFrom([]string{"https://img.example/a.tar.gz", "https://img.example/b.tar.gz"}).Draw(r.t, "url")
		}
		if rapid.Bool().Draw(r.t, "hasArch") {
			f["arch"] = rapid.SampledFrom([]string{"amd64", "arm64"}).Draw(r.t, "arch")
		}
		return f
	}
	complete := func(f map[string]any) bool {
		return f["version"] != nil && f["sha256"] != nil && f["url"] != nil
	}
	apply := func(img *pmImage, f map[string]any) {
		img.version, img.sha, img.url = f["version"].(string), f["sha256"].(string), f["url"].(string)
		img.arch = "amd64" // docs/dev/api.md: arch defaults to amd64
		if a, ok := f["arch"].(string); ok {
			img.arch = a
		}
	}
	checkBody := func(what string, resp propResp, img *pmImage) {
		var got model.Image
		if err := resp.json(&got); err != nil {
			r.failf("%s: %v", what, err)
		}
		if got.Name != img.name || got.Version != img.version || got.SHA256 != img.sha || got.URL != img.url || got.Arch != img.arch {
			r.failf("%s: body %s, model has %+v", what, resp, *img)
		}
		if img.id == uuid.Nil {
			img.id = got.ID
		}
		if got.ID != img.id {
			r.failf("%s: image %q id %s, model has %s (the id of a row must not change)", what, img.name, got.ID, img.id)
		}
	}

	switch imgOp {
	case "create":
		f := fields()
		f["name"] = name
		resp := r.call("image create", propReq{method: http.MethodPost, path: "/api/v1/images", body: pmJSON(f)})
		switch {
		case !complete(f):
			r.expect("image create (missing field)", resp, http.StatusBadRequest)
		case !pmImageNameOK(name):
			r.expect("image create (invalid name)", resp, http.StatusBadRequest)
		case r.m.images[name] != nil:
			r.expect("image create (duplicate)", resp, http.StatusConflict)
		default:
			r.expect("image create", resp, http.StatusCreated)
			img := &pmImage{name: name}
			apply(img, f)
			checkBody("image create", resp, img)
			r.m.images[name] = img
		}
	case "upsert":
		f := fields()
		if rapid.Bool().Draw(r.t, "bodyName") {
			f["name"] = "other-name" // the path name wins over the body
		}
		resp := r.call("image upsert", propReq{method: http.MethodPut, path: "/api/v1/images/" + name, body: pmJSON(f)})
		if !complete(f) {
			r.expect("image upsert (missing field)", resp, http.StatusBadRequest)
			return
		}
		if !pmImageNameOK(name) {
			r.expect("image upsert (invalid name)", resp, http.StatusBadRequest)
			return
		}
		r.expect("image upsert", resp, http.StatusOK)
		img := r.m.images[name]
		if img == nil {
			img = &pmImage{name: name}
			r.m.images[name] = img
			r.stats.add("image upsert: created")
		} else {
			r.stats.add("image upsert: replaced")
		}
		apply(img, f)
		checkBody("image upsert", resp, img)
	case "delete":
		resp := r.call("image delete by name", propReq{method: http.MethodDelete, path: "/api/v1/images/" + name})
		img := r.m.images[name]
		switch {
		case img == nil:
			r.expect("image delete by name", resp, http.StatusNotFound)
		default:
			r.expect("image delete by name", resp, http.StatusNoContent)
			delete(r.m.images, name)
		}
	case "delete-id":
		img := r.m.images[name]
		id := pmMissingUUID
		if img != nil {
			id = img.id.String()
		}
		resp := r.call("image delete by id", propReq{method: http.MethodDelete, path: "/api/v1/images/" + id})
		if img == nil {
			r.expect("image delete by id", resp, http.StatusNotFound)
			return
		}
		r.expect("image delete by id", resp, http.StatusNoContent)
		delete(r.m.images, name)
	default:
		resp := r.call("image list", propReq{method: http.MethodGet, path: "/api/v1/images"})
		r.expect("image list", resp, http.StatusOK)
		var got []model.Image
		if err := resp.json(&got); err != nil {
			r.failf("image list: %v", err)
		}
		if len(got) != len(r.m.images) {
			r.failf("image list: %d rows, model has %d", len(got), len(r.m.images))
		}
		for _, g := range got {
			w := r.m.images[g.Name]
			if w == nil || g.ID != w.id || g.Version != w.version || g.SHA256 != w.sha || g.URL != w.url || g.Arch != w.arch {
				r.failf("image list: row %+v, model has %+v", g, w)
			}
		}
	}
}

// ---------------------------------------------------------------- scripts

// pmScriptContent is a generated script with the outcome the upload
// contract assigns it: a priority, or a parse rejection.
type pmScriptContent struct {
	text     string
	valid    bool
	priority int
}

// drawScript builds script content constructively: a valid header block,
// optionally with exactly one violation.
func (r *pmRun) drawScript(kind string) pmScriptContent {
	fieldsByKind := map[string][]string{
		"storage": {"system_manufacturer", "system_product", "hostname"},
		"network": {"site", "location", "rack", "u", "device_type", "hostname"},
	}
	var lines []string
	if rapid.Bool().Draw(r.t, "shebang") {
		lines = append(lines, "#!/bin/sh")
	}
	out := pmScriptContent{valid: true}
	nm := rapid.IntRange(0, 2).Draw(r.t, "matches")
	for i := range nm {
		f := rapid.SampledFrom(fieldsByKind[kind]).Draw(r.t, fmt.Sprintf("field%d", i))
		v := rapid.SampledFrom([]string{"alpha", "/a.*/", "a=b", "/(x|y)-[0-9]+/"}).Draw(r.t, fmt.Sprintf("value%d", i))
		lines = append(lines, "# slagworks:match "+f+"="+v)
	}
	np := rapid.IntRange(0, 2).Draw(r.t, "priorities")
	for i := range np {
		p := rapid.SampledFrom([]int{0, 10, 10, 200, -5, 2147483648}).Draw(r.t, fmt.Sprintf("priority%d", i))
		pad := rapid.SampledFrom([]string{"", " ", "\t"}).Draw(r.t, fmt.Sprintf("pad%d", i))
		lines = append(lines, fmt.Sprintf("#%sslagworks:priority %d%s", pad, p, pad))
		out.priority = p // the last header wins
	}
	switch rapid.SampledFrom([]string{"ok", "ok", "ok", "ok", "ok", "bad-field", "bad-match", "bad-regex", "bad-priority", "kernel-params", "after-body"}).Draw(r.t, "violation") {
	case "bad-field":
		lines = append(lines, "# slagworks:match nonsense=1")
		out.valid = false
	case "bad-match":
		lines = append(lines, "# slagworks:match hostname")
		out.valid = false
	case "bad-regex":
		lines = append(lines, "# slagworks:match hostname=/(/")
		out.valid = false
	case "bad-priority":
		lines = append(lines, "# slagworks:priority high")
		out.valid = false
	case "kernel-params":
		// Not a directive of either family: an unrecognized `slagworks:`
		// header is refused rather than read as a comment.
		lines = append(lines, "# slagworks:kernel_params console=ttyS1")
		out.valid = false
	case "after-body":
		// Headers are read only from the leading comment block: a header
		// after the first command is script text, valid or not.
		lines = append(lines, "set -e", "# slagworks:priority 999", "# slagworks:match nonsense=1")
	}
	lines = append(lines, "echo layout")
	out.text = strings.Join(lines, "\n") + "\n"
	return out
}

func (r *pmRun) script() {
	kind := rapid.SampledFrom([]string{"storage", "network"}).Draw(r.t, "scriptKind")
	base := "/api/v1/" + kind + "-scripts"
	scripts := r.m.scripts[kind]
	name := rapid.SampledFrom([]string{"s1", "s1", "s2", "s3"}).Draw(r.t, "scriptName")
	op := rapid.SampledFrom([]string{"create", "create", "update", "update", "update", "delete", "list", "history"}).Draw(r.t, "scriptOp")
	if existing := pmSortedKeys(scripts); op != "create" && len(existing) > 0 && rapid.IntRange(0, 9).Draw(r.t, "existingScript") < 7 {
		name = rapid.SampledFrom(existing).Draw(r.t, "existingScriptName")
	}
	sc := scripts[name]

	switch op {
	case "create":
		c := r.drawScript(kind)
		resp := r.call(kind+"-script create", propReq{method: http.MethodPost, path: base,
			body: pmJSON(map[string]string{"name": name, "content": c.text})})
		switch {
		case !c.valid:
			r.expect(kind+"-script create (bad header)", resp, http.StatusBadRequest)
		case sc != nil:
			r.expect(kind+"-script create (duplicate)", resp, http.StatusConflict)
		default:
			r.expect(kind+"-script create", resp, http.StatusCreated)
			var got model.StorageScript // both families share the wire shape
			if err := resp.json(&got); err != nil || got.ID == uuid.Nil {
				r.failf("%s-script create: bad body %s (%v)", kind, resp, err)
			}
			if got.Name != name || got.Priority != c.priority || got.Content != c.text {
				r.failf("%s-script create: body {%q priority %d}, model expects {%q priority %d}\ncontent:\n%s",
					kind, got.Name, got.Priority, name, c.priority, c.text)
			}
			scripts[name] = &pmScript{id: got.ID, name: name, versions: []pmScriptVersion{{c.priority, c.text}}}
		}
	case "update":
		c := r.drawScript(kind)
		resp := r.call(kind+"-script update", propReq{method: http.MethodPut, path: base + "/" + name,
			body: pmJSON(map[string]string{"content": c.text})})
		switch {
		case !c.valid:
			r.expect(kind+"-script update (bad header)", resp, http.StatusBadRequest)
		case sc == nil:
			r.expect(kind+"-script update (unknown)", resp, http.StatusNotFound)
		default:
			r.expect(kind+"-script update", resp, http.StatusOK)
			var got struct {
				model.StorageScript
				Version int `json:"version"`
			}
			if err := resp.json(&got); err != nil {
				r.failf("%s-script update: %v", kind, err)
			}
			sc.versions = append(sc.versions, pmScriptVersion{c.priority, c.text})
			if got.ID != sc.id || got.Version != len(sc.versions) || got.Priority != c.priority || got.Content != c.text {
				r.failf("%s-script update: body {id %s version %d priority %d}, model expects {id %s version %d priority %d}",
					kind, got.ID, got.Version, got.Priority, sc.id, len(sc.versions), c.priority)
			}
			r.stats.add(fmt.Sprintf("%s-script versions reached %d", kind, len(sc.versions)))
		}
	case "delete":
		kindDel := rapid.SampledFrom([]string{"id", "id", "missing", "bad", "name"}).Draw(r.t, "delKind")
		if kindDel == "id" && sc == nil {
			kindDel = "missing"
		}
		switch kindDel {
		case "id":
			resp := r.call(kind+"-script delete", propReq{method: http.MethodDelete, path: base + "/" + sc.id.String()})
			r.expect(kind+"-script delete", resp, http.StatusNoContent)
			delete(scripts, name)
		case "missing":
			resp := r.call(kind+"-script delete", propReq{method: http.MethodDelete, path: base + "/" + pmMissingUUID})
			r.expect(kind+"-script delete", resp, http.StatusNotFound)
		default: // delete is by UUID only; a name is a malformed id
			resp := r.call(kind+"-script delete", propReq{method: http.MethodDelete, path: base + "/" + name})
			r.expect(kind+"-script delete", resp, http.StatusBadRequest)
		}
	case "list":
		resp := r.call(kind+"-script list", propReq{method: http.MethodGet, path: base})
		r.expect(kind+"-script list", resp, http.StatusOK)
		var got []model.StorageScript
		if err := resp.json(&got); err != nil {
			r.failf("%s-script list: %v", kind, err)
		}
		if len(got) != len(scripts) {
			r.failf("%s-script list: %d rows, model has %d", kind, len(got), len(scripts))
		}
		for _, g := range got {
			w := scripts[g.Name]
			if w == nil {
				r.failf("%s-script list: unexpected script %q", kind, g.Name)
			}
			head := w.versions[len(w.versions)-1]
			if g.ID != w.id || g.Priority != head.priority || g.Content != head.content {
				r.failf("%s-script list: %q is {id %s priority %d}, model head is {id %s priority %d}; the list must serve the head version",
					kind, g.Name, g.ID, g.Priority, w.id, head.priority)
			}
		}
	default:
		resp := r.call(kind+"-script history", propReq{method: http.MethodGet, path: base + "/" + name + "/history"})
		if sc == nil {
			r.expect(kind+"-script history", resp, http.StatusNotFound)
			return
		}
		r.expect(kind+"-script history", resp, http.StatusOK)
		var got []model.ScriptVersion
		if err := resp.json(&got); err != nil {
			r.failf("%s-script history: %v", kind, err)
		}
		if len(got) != len(sc.versions) {
			r.failf("%s-script history: %d versions, model has %d", kind, len(got), len(sc.versions))
		}
		for i, g := range got {
			w := sc.versions[i]
			if g.Version != i+1 || g.Priority != w.priority || g.Content != w.content || g.Head != (i == len(got)-1) {
				r.failf("%s-script history[%d] = {version %d priority %d head %v}, model has {version %d priority %d head %v}",
					kind, i, g.Version, g.Priority, g.Head, i+1, w.priority, i == len(got)-1)
			}
		}
	}
}

// ---------------------------------------------------------------- site configs

func (r *pmRun) createSite(slug, start, end string) {
	r.t.Helper()
	resp := r.call("site-config create", propReq{method: http.MethodPost, path: "/api/v1/site-configs",
		body: pmJSON(map[string]any{"slug": slug, "ephemeral_pool_start": start, "ephemeral_pool_end": end})})
	r.expect("site-config create", resp, http.StatusCreated)
	r.m.sites[slug] = &pmSiteConfig{slug: slug, netboxSite: slug, poolStart: start, poolEnd: end}
}

// pmIPv4 parses a dotted-quad IPv4 address (four decimal octets, no leading
// zeros) without the standard library's parser, returning its numeric value.
func pmIPv4(s string) (uint32, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return 0, false
	}
	var v uint32
	for _, p := range parts {
		if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
			return 0, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return 0, false
		}
		v = v<<8 | uint32(n)
	}
	return v, true
}

// pmPoolOK is the documented pool rule: both ends IPv4, start <= end.
func pmPoolOK(start, end string) bool {
	a, okA := pmIPv4(start)
	b, okB := pmIPv4(end)
	return okA && okB && a <= b
}

func (r *pmRun) siteConfig() {
	slug := rapid.SampledFrom([]string{"pop-a", "pop-a", "pop-b", "pop-c"}).Draw(r.t, "slug")
	siteOp := rapid.SampledFrom([]string{"create", "create", "patch", "patch", "patch", "delete", "get", "list"}).Draw(r.t, "siteOp")
	if existing := pmSortedKeys(r.m.sites); siteOp != "create" && len(existing) > 0 && rapid.IntRange(0, 9).Draw(r.t, "existingSite") < 7 {
		slug = rapid.SampledFrom(existing).Draw(r.t, "existingSlug")
	}
	pools := []string{"10.0.0.10", "10.0.0.10", "10.0.0.200", "10.0.0.200", "10.0.1.5", "10.0.1.5", "nope", "fe80::1", "", "10.0.0.256", "010.0.0.1"}
	cur := r.m.sites[slug]
	compare := func(what string, resp propResp, w *pmSiteConfig) {
		var got model.SiteConfig
		if err := resp.json(&got); err != nil {
			r.failf("%s: %v", what, err)
		}
		if got.Slug != w.slug || got.NetBoxSite != w.netboxSite || got.NetBoxLocation != w.location ||
			got.EphemeralPoolStart != w.poolStart || got.EphemeralPoolEnd != w.poolEnd || got.Promiscuous != w.promiscuous {
			r.failf("%s: body %s, model has %+v", what, resp, *w)
		}
	}
	switch siteOp {
	case "create":
		start := rapid.SampledFrom(pools).Draw(r.t, "start")
		end := rapid.SampledFrom(pools).Draw(r.t, "end")
		body := map[string]any{"ephemeral_pool_start": start, "ephemeral_pool_end": end}
		w := &pmSiteConfig{slug: slug, netboxSite: slug, poolStart: start, poolEnd: end}
		if rapid.IntRange(0, 9).Draw(r.t, "hasSlug") > 0 {
			body["slug"] = slug
		} else {
			w.slug, w.netboxSite = "", ""
		}
		if rapid.Bool().Draw(r.t, "hasNetboxSite") {
			w.netboxSite = "site-x"
			body["netbox_site"] = "site-x"
		}
		if rapid.Bool().Draw(r.t, "promiscuous") {
			w.promiscuous = true
			body["promiscuous"] = true
		}
		resp := r.call("site-config create", propReq{method: http.MethodPost, path: "/api/v1/site-configs", body: pmJSON(body)})
		if w.slug == "" || !pmPoolOK(start, end) || cur != nil {
			r.expect("site-config create (invalid or duplicate)", resp, http.StatusBadRequest)
			return
		}
		r.expect("site-config create", resp, http.StatusCreated)
		compare("site-config create", resp, w)
		r.m.sites[slug] = w
	case "patch":
		body := map[string]any{}
		next := pmSiteConfig{}
		if cur != nil {
			next = *cur
		}
		if rapid.Bool().Draw(r.t, "pStart") {
			next.poolStart = rapid.SampledFrom(pools).Draw(r.t, "start")
			body["ephemeral_pool_start"] = next.poolStart
		}
		if rapid.Bool().Draw(r.t, "pEnd") {
			next.poolEnd = rapid.SampledFrom(pools).Draw(r.t, "end")
			body["ephemeral_pool_end"] = next.poolEnd
		}
		if rapid.Bool().Draw(r.t, "pSite") {
			next.netboxSite = rapid.SampledFrom([]string{"site-y", ""}).Draw(r.t, "nbSite")
			body["netbox_site"] = next.netboxSite
		}
		if rapid.Bool().Draw(r.t, "pProm") {
			next.promiscuous = rapid.Bool().Draw(r.t, "prom")
			body["promiscuous"] = next.promiscuous
		}
		if rapid.Bool().Draw(r.t, "pSlug") {
			body["slug"] = "renamed" // not an updatable field; must be ignored
		}
		resp := r.call("site-config patch", propReq{method: http.MethodPatch, path: "/api/v1/site-configs/" + slug, body: pmJSON(body)})
		switch {
		case cur == nil:
			r.expect("site-config patch (unknown)", resp, http.StatusNotFound)
		case next.netboxSite == "" || !pmPoolOK(next.poolStart, next.poolEnd):
			r.expect("site-config patch (invalid)", resp, http.StatusBadRequest)
		default:
			r.expect("site-config patch", resp, http.StatusOK)
			compare("site-config patch", resp, &next)
			*cur = next
		}
	case "delete":
		resp := r.call("site-config delete", propReq{method: http.MethodDelete, path: "/api/v1/site-configs/" + slug})
		r.expect("site-config delete", resp, http.StatusNoContent) // also when no such slug exists
		delete(r.m.sites, slug)
	case "get":
		resp := r.call("site-config get", propReq{method: http.MethodGet, path: "/api/v1/site-configs/" + slug})
		if cur == nil {
			r.expect("site-config get", resp, http.StatusNotFound)
			return
		}
		r.expect("site-config get", resp, http.StatusOK)
		compare("site-config get", resp, cur)
	default:
		resp := r.call("site-config list", propReq{method: http.MethodGet, path: "/api/v1/site-configs"})
		r.expect("site-config list", resp, http.StatusOK)
		var got []model.SiteConfig
		if err := resp.json(&got); err != nil {
			r.failf("site-config list: %v", err)
		}
		if len(got) != len(r.m.sites) {
			r.failf("site-config list: %d rows, model has %d", len(got), len(r.m.sites))
		}
		for _, g := range got {
			if r.m.sites[g.Slug] == nil {
				r.failf("site-config list: unexpected slug %q", g.Slug)
			}
		}
	}
}

// ---------------------------------------------------------------- site workers

func (r *pmRun) findWorker(slug, workerID string) (int, *pmWorker) {
	for i, w := range r.m.workers {
		if w.slug == slug && w.workerID == workerID {
			return i, w
		}
	}
	return -1, nil
}

func (r *pmRun) siteWorker() {
	slug := rapid.SampledFrom([]string{"pop-a", "pop-b"}).Draw(r.t, "wSlug")
	workerID := rapid.SampledFrom([]string{"w1", "w1", "w2"}).Draw(r.t, "wID")
	workerOp := rapid.SampledFrom([]string{"register", "register", "register", "register-bad", "delete", "delete", "list"}).Draw(r.t, "workerOp")
	if workerOp == "delete" && len(r.m.workers) > 0 && rapid.IntRange(0, 9).Draw(r.t, "existingWorker") < 8 {
		w := r.m.workers[rapid.IntRange(0, len(r.m.workers)-1).Draw(r.t, "existingWorkerIdx")]
		slug, workerID = w.slug, w.workerID
	}
	switch workerOp {
	case "register":
		// The fqdn pool overlaps the worker_id pool so an identifier can
		// match one row by worker_id and another by fqdn.
		fqdn := rapid.SampledFrom([]string{"", "gw.pop-a.example", "w1", "w2"}).Draw(r.t, "fqdn")
		// docs/dev/api.md class `creds`: the minted token receives the
		// site's device credentials on the op stream, so an operator outside
		// the credential group cannot register (or rotate) a worker.
		tok := rapid.SampledFrom([]string{propCredToken, propCredToken, propCredToken, propOperatorToken}).Draw(r.t, "registrar")
		resp := r.call("site-worker register", propReq{method: http.MethodPost, path: "/api/v1/site-workers", token: tok,
			body: pmJSON(map[string]string{"site_slug": slug, "worker_id": workerID, "fqdn": fqdn})})
		if tok != propCredToken {
			r.expect("site-worker register (no credential group)", resp, http.StatusForbidden)
			if strings.Contains(string(resp.body), `"token"`) {
				r.failf("site-worker register: a refusal carries a token: %s", resp)
			}
			return
		}
		r.expect("site-worker register", resp, http.StatusCreated)
		var got struct {
			ID       uuid.UUID `json:"id"`
			SiteSlug string    `json:"site_slug"`
			WorkerID string    `json:"worker_id"`
			FQDN     string    `json:"fqdn"`
			Token    string    `json:"token"`
		}
		if err := resp.json(&got); err != nil {
			r.failf("site-worker register: %v", err)
		}
		if got.SiteSlug != slug || got.WorkerID != workerID || got.FQDN != fqdn || len(got.Token) != 64 {
			r.failf("site-worker register: body %s", resp)
		}
		_, w := r.findWorker(slug, workerID)
		if w == nil {
			r.m.workers = append(r.m.workers, &pmWorker{id: got.ID, slug: slug, workerID: workerID, fqdn: fqdn})
			return
		}
		// Re-registration rotates the token of the SAME registration: the
		// response names the existing row, not a fresh id.
		r.stats.add("site-worker register: rotation")
		w.fqdn = fqdn
		if got.ID != w.id {
			r.failf("site-worker register (rotation): response id %s, but the registration's id is %s", got.ID, w.id)
		}
	case "register-bad":
		body := rapid.SampledFrom([]string{`{"site_slug":"pop-a"}`, `{"worker_id":"w1"}`, `{}`, `{"site_slug":1,"worker_id":"w"}`, `[`}).Draw(r.t, "badWorker")
		resp := r.call("site-worker register bad", propReq{method: http.MethodPost, path: "/api/v1/site-workers", token: propCredToken, body: body})
		r.expect("site-worker register bad", resp, http.StatusBadRequest)
	case "delete":
		kind := rapid.SampledFrom([]string{"id", "worker_id", "worker_id", "fqdn", "missing"}).Draw(r.t, "identKind")
		ident := pmMissingUUID
		i, w := r.findWorker(slug, workerID)
		if w != nil {
			switch kind {
			case "id":
				ident = w.id.String()
			case "worker_id":
				ident = w.workerID
			case "fqdn":
				if w.fqdn != "" {
					ident = w.fqdn
				}
			}
		} else if kind != "missing" {
			ident = workerID // may still match another slug's registration
		}
		resp := r.call("site-worker delete", propReq{method: http.MethodDelete, path: "/api/v1/site-workers/" + ident})
		// Independent resolution: a UUID names a row id; anything else
		// matches worker_id or fqdn, and must match exactly one row.
		var matches []int
		if id, err := uuid.Parse(ident); err == nil {
			for j, x := range r.m.workers {
				if x.id == id {
					matches = append(matches, j)
				}
			}
		} else {
			for j, x := range r.m.workers {
				if x.workerID == ident || x.fqdn == ident {
					matches = append(matches, j)
				}
			}
		}
		_ = i
		switch len(matches) {
		case 0:
			r.expect("site-worker delete (unknown)", resp, http.StatusNotFound)
		case 1:
			target := r.m.workers[matches[0]]
			for _, o := range r.m.ops {
				if o.active() && o.claimedBy == target.workerID {
					r.expect("site-worker delete (in-flight claim)", resp, http.StatusConflict)
					r.stats.add("site-worker delete: refused, in-flight claim")
					return
				}
			}
			r.expect("site-worker delete", resp, http.StatusNoContent)
			r.m.workers = append(r.m.workers[:matches[0]], r.m.workers[matches[0]+1:]...)
		default:
			r.expect("site-worker delete (ambiguous)", resp, http.StatusConflict)
			r.stats.add("site-worker delete: ambiguous identifier")
		}
	default:
		resp := r.call("workers list", propReq{method: http.MethodGet, path: "/api/v1/workers"})
		r.expect("workers list", resp, http.StatusOK)
		var got []model.SiteWorker
		if err := resp.json(&got); err != nil {
			r.failf("workers list: %v", err)
		}
		if len(got) != len(r.m.workers) {
			r.failf("workers list: %d rows, model has %d", len(got), len(r.m.workers))
		}
		for _, g := range got {
			_, w := r.findWorker(g.SiteSlug, g.WorkerID)
			if w == nil || g.ID != w.id || g.FQDN != w.fqdn {
				r.failf("workers list: row {%s %s/%s fqdn %q}, model has %+v", g.ID, g.SiteSlug, g.WorkerID, g.FQDN, w)
			}
		}
		if strings.Contains(string(resp.body), "token") {
			r.failf("workers list exposes a token field: %s", resp)
		}
	}
}

// ---------------------------------------------------------------- reads that need NetBox or the hub

func (r *pmRun) miscRead() {
	ref := r.drawRef("m")
	mpath := "/api/v1/machines/" + ref.seg
	slug := rapid.SampledFrom([]string{"pop-a", "pop-b", "pop-c"}).Draw(r.t, "slug")
	resolved := func(code int) []int { // code once the machine id resolves
		if c := ref.resolveCode(); c != 0 {
			return []int{c}
		}
		return []int{code}
	}
	type probe struct {
		name string
		req  propReq
		want []int
	}
	siteCode := func(code int) []int {
		if r.m.sites[slug] == nil {
			return []int{http.StatusNotFound}
		}
		return []int{code}
	}
	probes := []probe{
		// No hub: the synchronous BMC reads cannot be routed.
		{"power status", propReq{method: http.MethodGet, path: mpath + "/power"}, resolved(http.StatusServiceUnavailable)},
		{"boot status", propReq{method: http.MethodGet, path: mpath + "/boot"}, resolved(http.StatusServiceUnavailable)},
		{"preflight", propReq{method: http.MethodGet, path: mpath + "/preflight"}, resolved(http.StatusOK)},
		{"preflight commission", propReq{method: http.MethodGet, path: mpath + "/preflight?op=commission"}, resolved(http.StatusOK)},
		{"preflight bad op", propReq{method: http.MethodGet, path: mpath + "/preflight?op=power"}, resolved(http.StatusBadRequest)},
		{"match", propReq{method: http.MethodGet, path: mpath + "/match"}, resolved(http.StatusServiceUnavailable)},
		{"console-probe", propReq{method: http.MethodGet, path: mpath + "/console-probe?nonce=deadbeef"}, resolved(http.StatusOK)},
		{"console-probe bad nonce", propReq{method: http.MethodGet, path: mpath + "/console-probe?nonce=%25"}, resolved(http.StatusBadRequest)},
		{"console-probe bad lookback", propReq{method: http.MethodGet, path: mpath + "/console-probe?nonce=ab&lookback=0"}, resolved(http.StatusBadRequest)},
		{"dhcp-leases", propReq{method: http.MethodGet, path: "/api/v1/site-configs/" + slug + "/dhcp-leases"}, siteCode(http.StatusServiceUnavailable)},
		{"dhcp-lease-history", propReq{method: http.MethodGet, path: "/api/v1/site-configs/" + slug + "/dhcp-lease-history?mac=zz&ip=x&limit=5"}, siteCode(http.StatusOK)},
		{"dhcp-lease-history bad limit", propReq{method: http.MethodGet, path: "/api/v1/site-configs/" + slug + "/dhcp-lease-history?limit=many"}, siteCode(http.StatusBadRequest)},
		{"dhcp-lease-history negative limit", propReq{method: http.MethodGet, path: "/api/v1/site-configs/" + slug + "/dhcp-lease-history?limit=-3"}, siteCode(http.StatusOK)},
		{"device creds (no credential group)", propReq{method: http.MethodGet, path: "/api/v1/devices/" + ref.seg + "/creds"}, []int{http.StatusForbidden}},
		{"device creds", propReq{method: http.MethodGet, path: "/api/v1/devices/" + ref.seg + "/creds", token: propCredToken}, []int{http.StatusServiceUnavailable}},
		{"pdu resolve", propReq{method: http.MethodGet, path: "/api/v1/pdus/" + ref.seg + "/outlet/1/power/resolve?verb=cycle"}, []int{http.StatusServiceUnavailable}},
		{"pdu power", propReq{method: http.MethodPost, path: "/api/v1/pdus/" + ref.seg + "/outlet/1/power", body: `{"verb":"cycle","preflight_token":"x"}`}, []int{http.StatusServiceUnavailable}},
		{"netbox bootstrap", propReq{method: http.MethodPost, path: "/api/v1/admin/netbox/bootstrap"}, []int{http.StatusServiceUnavailable}},
		{"netbox migrate", propReq{method: http.MethodPost, path: "/api/v1/admin/netbox/migrate-chassis-serial", body: `{"dry_run":true}`}, []int{http.StatusServiceUnavailable}},
		{"client-config", propReq{method: http.MethodGet, path: "/api/v1/client-config", token: "-"}, []int{http.StatusOK}},
		{"healthz", propReq{method: http.MethodGet, path: "/healthz", token: "-"}, []int{http.StatusOK}},
	}
	// netbox-diff takes a UUID only (no hostname form).
	nd := probe{"netbox-diff", propReq{method: http.MethodGet, path: mpath + "/netbox-diff"}, []int{http.StatusServiceUnavailable}}
	if ref.kind == pmRefBad || ref.kind == pmRefDotted {
		nd.want = []int{http.StatusBadRequest}
	}
	probes = append(probes, nd)

	p := probes[rapid.IntRange(0, len(probes)-1).Draw(r.t, "probe")]
	resp := r.call(p.name, p.req)
	r.expect(p.name, resp, p.want...)
}

// pmSortedKeys is used by reports.
func pmSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
