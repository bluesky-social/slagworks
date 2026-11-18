# Supermicro SMCIPMITool

On Supermicro hosts, the site worker can drive BMC operations through
Supermicro's own command-line tool, SMCIPMITool, instead of the generic
bmclib/ipmitool path. Slagworks does not ship SMCIPMITool: it is
proprietary Supermicro software, and each operator downloads it from
Supermicro under Supermicro's license.

The tool is optional. Without it, Supermicro hosts use the generic path
and slagworks reminds you that the tool is missing (see "Where slagworks
prompts you" below).

## Why install it

- **Reliability.** Supermicro BMCs built on AMI MegaRAC firmware (for
  example the H13 generation) have only a handful of IPMI session slots.
  Generic `ipmitool` sessions can exhaust them and wedge the BMC until it
  is reset. SMCIPMITool talks to these BMCs more reliably.
- **Boot-order commands.** `slagctl machine power boot-set` and
  `boot-status` need a power controller that can read and set boot
  overrides. Today only the SMCIPMITool controller can, so these commands
  report "not supported" on hosts without it.

With the tool installed, a Supermicro host's power on/off/cycle/status,
one-time boot override (used by deploys), boot-order reads and writes, and
chassis-serial reads all go through SMCIPMITool. Serial-over-LAN console
capture always stays on `ipmitool`.

Slagworks identifies Supermicro hosts by the NetBox manufacturer slug
`supermicro` on the device type.

## Install

Do this on every site worker that manages Supermicro hosts, including
standby workers in an HA pair.

1. Download the Linux x64 bundle with the bundled JRE
   (`SMCIPMITool_<version>_bundleJRE_Linux_x64.tar.gz`) from Supermicro's
   [SMCIPMITool download area](https://www.supermicro.com/wdl/utility/SMCIPMItool/).
2. Extract it to a fixed location, for example `/opt/smcipmitool`:

   ```sh
   tar -xzf SMCIPMITool_<version>_bundleJRE_Linux_x64.tar.gz -C /opt
   mv /opt/SMCIPMITool_<version>_bundleJRE_Linux_x64 /opt/smcipmitool
   ```
3. Install a small launcher and make it executable. It runs the tool with
   the Java runtime bundled in the download, using the main class and jars
   named in the bundle's `SMCIPMITool.lax`:

   ```sh
   cat >/usr/local/bin/SMCIPMITool <<'EOF'
   #!/bin/sh
   home=/opt/smcipmitool
   exec "$home/jre/bin/java" -Djava.library.path="$home" \
     -cp "$home/SMCIPMITool.jar:$home/sc.jar:$home/ko.jar" \
     com.supermicro.ipmi.text.SuperBladeTool "$@"
   EOF
   chmod 755 /usr/local/bin/SMCIPMITool
   ```

   This avoids the bundle's own `SMCIPMITool` launcher script, which looks
   for its configuration relative to the current directory.

   The worker looks for `SMCIPMITool` on its `PATH` by default. If you put
   the launcher somewhere else, set `SLAGWORKS_SMCIPMITOOL_BIN` to its path
   in `/etc/slagworks/site.env`.
4. Check that it can talk to one of your BMCs. `slagworksd-site` runs as
   root, so test as root:

   ```sh
   sudo SMCIPMITool <bmc-ip> <user> <password> ipmi power status
   ```

   A working install prints a line like `Power is currently on.` Don't
   skip this: slagworks only checks that the launcher exists, so a broken
   install (for example, a download without the bundled JRE) still shows
   as `installed` while every call to it fails. The tool
   takes the password as an argument, so it is visible in the process
   list and your shell history while you test; use a throwaway credential
   or clear your history afterwards.
5. Restart the worker so global sees the new status right away:
   `systemctl restart slagworksd-site`. Power operations pick the tool up
   without a restart, but the status shown in `slagctl worker list` is
   reported when the worker connects.

To confirm, run `slagctl worker list` (the `SMCIPMITOOL` column should say
`installed`, meaning the launcher was found) and
`slagctl machine power status <supermicro-host>` (the transport in
parentheses should be `smcipmitool`, meaning a call through it succeeded).

## Settings

Both settings go in the site worker's environment (`/etc/slagworks/site.env`).

- **`SLAGWORKS_SMCIPMITOOL`** — how the worker uses the tool on Supermicro
  hosts:
  - `auto` (default): use it when installed, otherwise fall back to the
    generic path.
  - `required`: power operations on Supermicro hosts fail when the tool is
    missing, instead of falling back. Use this once your fleet depends on
    the tool, so a worker that lost it fails loudly rather than quietly
    returning to the less reliable path.
  - `off`: never use it, even if it is installed. Slagworks stops
    prompting you to install it.
- **`SLAGWORKS_SMCIPMITOOL_BIN`** — path to the launcher. Default:
  `SMCIPMITool` on the worker's `PATH`.

## Where slagworks prompts you

When a site has Supermicro hosts and a worker there doesn't have the tool:

- **`slagctl worker list`** adds an `SMCIPMITOOL` column and a hint naming
  the workers that are missing it (`missing-required` for workers in
  `required` mode, where Supermicro power operations fail). The column
  only appears when some site has Supermicro hosts.
- **The site worker log** gets a warning when it first receives a snapshot
  with Supermicro hosts, and again whenever the number of them changes.
  In `required` mode this is an error.
- **`slagctl machine power status`** and **`boot-status`** print a hint
  for Supermicro hosts that were served by the generic path.
