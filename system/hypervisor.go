package system

// Hypervisor configuration. Centralised so a future fork (or a backwards-
// compatibility branch) only has to flip these constants.
//
// ZNAS 6.5.2 switched from LXD to Incus. Every literal that used to be
// "lxc" / "/var/lib/lxd" / "lxd.service" / "lxd" group is referenced via
// these constants (or substituted directly for grep-ability). The engine
// name is *not* a runtime feature flag — keeping it as a constant gives
// reviewers one place to verify the migration is complete.
const (
	// HVName is the CLI binary name (e.g. `incus list`).
	HVName = "incus"

	// HVServiceUnit is the systemd unit that runs the daemon.
	HVServiceUnit = "incus.service"

	// HVUserGroup is the OS group whose members get full daemon access.
	// Incus also offers an `incus` group with read-only access; we require
	// the admin group because ZNAS performs mutations.
	HVUserGroup = "incus-admin"

	// HVStateDir is the daemon state directory (DB, profiles, NVRAM,
	// per-instance qemu.nvram, AppArmor policy stubs, server cert, etc).
	HVStateDir = "/var/lib/incus"

	// HVLogDir is the daemon log directory (qemu.log / qemu.conf /
	// qemu.monitor / qemu.spice / console.log).
	HVLogDir = "/var/log/incus"

	// HVSocketPath is the daemon's local Unix socket (root-only by default;
	// the HVUserGroup grants access).
	HVSocketPath = "/var/lib/incus/unix.socket"

	// HVRunSocketPath is a secondary system-wide socket exposed under
	// /run/incus/unix.socket (used by `incus admin` and the agent on some
	// distros).
	HVRunSocketPath = "/run/incus/unix.socket"

	// HVAppArmorPrefix is the prefix Incus uses for per-instance AppArmor
	// profile names (`incus-<name>_<...>`).
	HVAppArmorPrefix = "incus-"
)
