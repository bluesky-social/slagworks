// Package netlib ships the Python helper library the assets/network/ config
// scripts import at runtime. The six network scripts are standalone Python
// executables the deploy agent downloads one at a time and execs; they share a
// lot of mechanical plumbing (CIDR munging, the oob-gw VIP resolver, the
// emit-bundle dict shape, netplan YAML fragments) that used to be copy-pasted
// per file. That shared code now lives in slagworks_netlib.py, embedded here so
// the agent binary carries it and can write it next to each script it runs.
//
// Why co-located-sidecar and not a repo import path: at deploy time the agent
// writes the matched script to a temp file and execs it (execkit.RunConfigScript).
// There is no shared Python import path in the deploy initrd — so a script's
// `import slagworks_netlib` only resolves if the lib file sits in the script's
// own directory (Python puts the script's dir on sys.path[0]). The harness
// writes both files into one temp dir; that co-location IS the linkage, no
// PYTHONPATH env needed in production.
package netlib

import _ "embed"

// ModuleName is the import name the scripts use (`import slagworks_netlib`) and
// therefore the basename the sidecar file must be written under.
const ModuleName = "slagworks_netlib"

// Source is the embedded Python helper library. RunConfigScript writes it as
// <ModuleName>.py alongside the network script before exec.
//
//go:embed slagworks_netlib.py
var Source []byte
