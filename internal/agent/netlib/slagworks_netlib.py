# slagworks network-config script helper library.
#
# Shipped by the configscript engine, NOT imported from the repo at runtime:
# the deploy agent writes this file next to the (temp) network script it runs,
# so the script's own directory (Python's sys.path[0]) resolves
# `import slagworks_netlib`. There is no PYTHONPATH plumbing in production; the
# co-located file is the whole mechanism (internal/agent/execkit RunConfigScript
# writes the sidecars, internal/agent/netlib embeds this source).
#
# It exists because the six assets/network/ scripts hand-rolled the same
# plumbing per file: CIDR string-munging, the oob-gw VIP resolver, private-IP
# and BMC-name predicates, the emit-bundle dict shape, and the netplan YAML
# fragments (the loopback stanza, the OOB mgmt leg, the LACP bond block). This
# is DEDUP, not redesign: every helper reproduces the exact bytes the scripts
# emitted before, so the netscript golden tests stay green.
#
# The scripts still own their topology interpretation and their hard-fail
# error messages (those are host-family-specific and test-asserted); the lib
# only owns the shared mechanics.

import ipaddress
import json
import os
import re
import sys

# BMC/iDRAC port names to exclude when picking a host's OOB leg. ipmi0/idrac
# ports are modeled with a private OOB IP (the mgmt_only conflation),
# so a host-NIC picker must exclude them by name or one is mistaken for the
# management NIC. Shared by every oob-gw script.
BMC_NAME_RE = re.compile(r"^(ipmi|idrac|bmc|mgmt|lom)", re.IGNORECASE)


def load_facts():
    """Read the NetBox interface facts JSON from SLAGWORKS_NETWORK_FACTS.

    The single entry point every network script shares: resolves the env var
    the agent injects (execkit.RunConfigScript) and returns the decoded list.
    Exits non-zero with a clear message when the var is unset — that only
    happens when the script is run outside the agent harness."""
    facts_path = os.environ.get("SLAGWORKS_NETWORK_FACTS")
    if not facts_path:
        sys.stderr.write("ERROR: SLAGWORKS_NETWORK_FACTS must be set\n")
        sys.exit(1)
    with open(facts_path) as f:
        return json.load(f)


def fail(msg):
    """Write msg to stderr and exit non-zero — the loud-fail path every script
    takes when a host it matched isn't fully modeled. msg should already end
    with a newline (kept explicit so callers control multi-line messages)."""
    sys.stderr.write(msg)
    sys.exit(1)


def emit(bundle):
    """Print the emit-bundle dict as JSON on stdout — the one output contract
    (docs/dev/extensibility.md "Netscript contract"). Plain json.dumps, no
    custom separators, so the bytes match what the scripts printed before."""
    print(json.dumps(bundle))


def bundle(files, enable_units=None):
    """Build the emit-bundle dict: {"files": [...], "enable_units": [...]}.
    enable_units defaults to [] (the netplan-only case). Key order matches the
    hand-rolled dicts so json.dumps is byte-identical."""
    return {"files": files, "enable_units": enable_units or []}


def file_entry(path, content, mode=None, owner=None, group=None):
    """One bundle file dict. Keys are inserted path, content, then mode/owner/
    group only when set — the exact order (and omit-when-empty behavior) the
    scripts used, so the emitted JSON is unchanged."""
    e = {"path": path, "content": content}
    if mode:
        e["mode"] = mode
    if owner:
        e["owner"] = owner
    if group:
        e["group"] = group
    return e


def addr_only(cidr):
    """The address part of a CIDR, e.g. '10.254.28.1/31' -> '10.254.28.1'. A
    no-op on a bare address, so it is safe to call on gateways too."""
    return cidr.split("/", 1)[0]


def subnet24(cidr):
    """First three octets of a CIDR's address, e.g. '10.254.28.1/31' ->
    '10.254.28'."""
    return ".".join(addr_only(cidr).split(".")[:3])


def is_private_ip(cidr):
    """True if the CIDR's address is RFC1918 private. False (not an exception)
    on an unparseable value, so callers can filter facts defensively."""
    try:
        return ipaddress.ip_interface(cidr).ip.is_private
    except ValueError:
        return False


def is_bmc_name(name):
    """True if the interface name looks like a BMC/iDRAC port (ipmi*/idrac*/
    bmc*/mgmt*/lom*) — the name-based exclusion for OOB-leg pickers."""
    return bool(BMC_NAME_RE.match(name or ""))


def vip_nameserver(ip_or_gateway):
    """The pop's oob-gw keepalived VIP at {first two octets}.240.1 — the
    deploy-window resolver every script points at (10.254.240.1 for pop1,
    10.253.240.1 for pop2). Accepts either a bare gateway ('10.253.3.254') or a
    CIDR ('10.254.240.5/22'); addr_only normalizes both. It is L2-reachable on
    the host's own segment, so it works WITHOUT a default route (which the L3 /
    oob-gw scripts deliberately don't emit). Returns "" if the input isn't a
    dotted IPv4 quad, so the caller can omit the nameservers stanza."""
    parts = addr_only(ip_or_gateway).split(".")
    if len(parts) != 4:
        return ""
    return f"{parts[0]}.{parts[1]}.240.1"


def leaf_side_of_31(host_cidr):
    """The other address of a /31 pair (the leaf side / host's next hop),
    computed by flipping the last bit so it does not depend on host-odd/leaf-
    even parity. Returns "" if host_cidr is not an IPv4 /31."""
    try:
        iface = ipaddress.ip_interface(host_cidr)
    except ValueError:
        return ""
    if iface.network.prefixlen != 31 or iface.version != 4:
        return ""
    return str(ipaddress.ip_address(int(iface.ip) ^ 1))


def loopback_lines(loopback):
    """netplan `lo` stanza: 127.0.0.1/8 + the identity loopback /32. Shared by
    the L3 kworker and pop1 oob-gw scripts (byte-identical block)."""
    return [
        "    lo:",
        "      addresses:",
        '        - "127.0.0.1/8"',
        f'        - "{loopback}"',
    ]


def oob_leg_lines(name, mac, ip, nameserver):
    """netplan stanza for a static OOB management leg (no default route),
    matched by MAC + set-name, with an inline-list nameservers stanza when
    nameserver is non-empty. Shared by the pop1 and pop2-oldcage oob-gw
    scripts."""
    lines = [
        f"    {name}:",
        "      match:",
        f'        macaddress: "{mac}"',
        f"      set-name: {name}",
        "      dhcp4: false",
        "      dhcp6: false",
        "      addresses:",
        f'        - "{ip}"',
    ]
    if nameserver:
        lines.append("      nameservers:")
        lines.append(f"        addresses: [{nameserver}]")
    return lines


def bond_member_lines(members, mtu):
    """netplan ethernets stanzas for LACP bond members (match-by-MAC +
    set-name + mtu). members is a list of (set_name, mac) sorted by set_name.
    Shared by the two pop2 L2-bond scripts (hot-spare + oldcage oob-gw)."""
    lines = []
    for set_name, mac in members:
        lines.append(f"    {set_name}:")
        lines.append("      match:")
        lines.append(f'        macaddress: "{mac}"')
        lines.append(f"      set-name: {set_name}")
        lines.append(f"      mtu: {mtu}")
    return lines


def bond_lines(bond_name, bond_ip, mtu, members, gateway=None, nameserver=None):
    """netplan `bonds:` block: 802.3ad / lacp-rate fast / layer3+4 hash (the
    leaf bond34 policy), the bond address, and OPTIONALLY a default route
    (gateway) and a list-form nameservers stanza (nameserver). The hot-spare
    script passes both (a plain L2 host whose only egress is the bond); the
    oldcage oob-gw passes neither (its default lives on the WAN via FRR).
    Shared by both pop2 L2-bond scripts."""
    lines = [
        "  bonds:",
        f"    {bond_name}:",
        "      interfaces:",
    ]
    for set_name, _ in members:
        lines.append(f"        - {set_name}")
    lines.append(f"      mtu: {mtu}")
    lines.append("      parameters:")
    lines.append("        mode: 802.3ad")
    lines.append("        lacp-rate: fast")
    lines.append("        mii-monitor-interval: 100")
    lines.append("        transmit-hash-policy: layer3+4")
    lines.append("      addresses:")
    lines.append(f'        - "{bond_ip}"')
    if gateway:
        lines.append("      routes:")
        lines.append("        - to: default")
        lines.append(f"          via: {gateway}")
    if nameserver:
        lines.append("      nameservers:")
        lines.append("        addresses:")
        lines.append(f'          - "{nameserver}"')
    return lines
