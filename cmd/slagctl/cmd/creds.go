package cmd

import (
	"fmt"
	"io"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
)

// machineCredsCmd prints the OOB credentials for any NetBox device by proxying
// the pop's bmc-creds-admin AWS secret through slagworksd global. The endpoint
// passes through BMC, PDU, serial-device, and serial-server credential kinds.
//
// This lives under `machine` (`slagctl machine creds <device>`),
// but it is deliberately NOT machine-only. The server's /devices/<ident>/creds
// endpoint resolves ANY NetBox device (server, switch, PDU) by hostname, id, or
// serial — it does NOT go through the registered-machine resolver. That is
// how an operator fetches a BMC-less switch's PDU creds: name the PDU
// device directly. Do not route this through resolveMachine; doing so would
// reject every non-machine device.
//
// The one shape the devices endpoint can't resolve is a slagworks machine
// UUID, which is meaningless to NetBox. So resolveDeviceIdent translates a
// UUID arg to the machine's serial first (and passes every other shape
// through verbatim), giving `creds` the same UUID-accepting arg surface as
// the rest of the machine subcommands without losing the non-machine path.
//
// It does NOT filter by credential kind. Name any device and get back
// whatever cred the secret holds for it.
//
// The privileged secret read happens server-side under the admin auth gate;
// the operator never needs direct AWS access to the secret. Output is
// plaintext credentials — handle accordingly.
func machineCredsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds <device>",
		Short: "print OOB credentials for a NetBox device",
		Long: `Look up a NetBox device by hostname, id, or serial and print its
out-of-band credentials from the pop's bmc-creds-admin AWS secret.

Works for any device modeled in NetBox — registered as a slagworks
machine or not — and for all five credential kinds the secret carries
(bmc, amt, pdu, serial, serial-server). The secret read is proxied through
slagworksd global under the admin auth gate, so the operator needs no
direct access to the AWS secret.

A server that sets SLAGWORKS_OIDC_CREDENTIAL_GROUPS returns credentials
only to operators who are also in one of those groups; anyone else gets
a 403 naming that setting. Ask a slagworks administrator for membership,
then run "slagctl auth login" for a token that carries the group.

To get the credentials for the PDUs feeding a BMC-less switch, name the
PDU devices directly (e.g. "slagctl machine creds pdu-0102-left").

Output is plaintext credentials — handle accordingly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A slagworks machine UUID isn't a NetBox identifier, so the
			// devices endpoint can't resolve it directly — translate it to
			// the machine's serial. Every other shape (name/id/serial,
			// incl. non-machine PDUs/switches) passes through verbatim.
			ident, err := resolveDeviceIdent(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var creds api.DeviceCreds
			path := "/api/v1/devices/" + url.PathEscape(ident) + "/creds"
			if err := apiClient.Get(ctx(cmd), path, nil, &creds); err != nil {
				return fmt.Errorf("resolve creds for %q: %w", ident, err)
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), creds)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "device:   %s\n", emptyDash(creds.Device))
			fmt.Fprintf(out, "netbox_id: %d\n", creds.NetBoxID)
			fmt.Fprintf(out, "serial:   %s\n", emptyDash(creds.Serial))
			fmt.Fprintf(out, "pop:      %s\n", emptyDash(creds.Pop))
			fmt.Fprintf(out, "kind:     %s\n", emptyDash(creds.Kind))
			fmt.Fprintf(out, "mgmt_ip:  %s\n", emptyDash(creds.MgmtIP))
			fmt.Fprintf(out, "username: %s\n", emptyDash(creds.Username))
			fmt.Fprintf(out, "password: %s\n", emptyDash(creds.Password))
			if len(creds.ExtraConfig) > 0 {
				fmt.Fprintf(out, "extra_config:\n")
				for k, v := range creds.ExtraConfig {
					fmt.Fprintf(out, "  %s: %v\n", k, v)
				}
			}
			printCredHint(out, creds)
			return nil
		},
	}
	return cmd
}

// printCredHint emits a kind-appropriate ready-to-paste command line. The
// hint is best-effort; it prints only when the fields it needs are present.
func printCredHint(out io.Writer, creds api.DeviceCreds) {
	if creds.MgmtIP == "" || creds.Username == "" || creds.Password == "" {
		return
	}
	switch creds.Kind {
	case "bmc":
		// IPMI_PASSWORD + -E keeps the password out of /proc/<pid>/cmdline
		// and shell history; -P would leak it to the process list. Shell-
		// quote the cred fields so a metacharacter-laden password doesn't
		// break the paste or execute.
		fmt.Fprintf(out, "\nIPMI_PASSWORD=%s ipmitool -I lanplus -H %s -U %s -E ...\n",
			shellQuote(creds.Password), creds.MgmtIP, shellQuote(creds.Username))
	case "amt":
		// Intel AMT: WS-Man over TLS on 16993 (digest auth), SOL/IDE-R
		// redirection on 16995. The AMT cert is self-signed, hence -k.
		fmt.Fprintf(out, "\ncurl -k --digest -u %s https://%s:16993/wsman   # Intel AMT WS-Man; password above\n",
			shellQuote(creds.Username), creds.MgmtIP)
	case "pdu":
		// APC NMC speaks SSH. Password goes on stdin-equivalent via the
		// operator's own ssh; we just show the target. (Don't embed the
		// password in the command line — it'd leak to the process list.)
		fmt.Fprintf(out, "\nssh %s@%s   # APC NMC CLI; password above\n",
			shellQuote(creds.Username), creds.MgmtIP)
	}
}
