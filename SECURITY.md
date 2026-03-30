# Security

## Sudo access model

ZFS NAS Portal requires `sudo` privileges because most ZFS, Samba, NFS, SMART, and system operations must run as root. Three configurations are supported — the portal's **Prerequisites** tab reports which one is active.

| Mode | How it works | Recommended? |
|---|---|---|
| **Running as root** | Process UID is 0; no sudo needed | No — high blast radius |
| **NOPASSWD: ALL** | Blanket root delegation | Acceptable for home / lab use |
| **Hardened sudoers** | Whitelist of specific commands only | Yes — production deployments |

---

## Restricting sudo (recommended for production)

### 1 — Create a dedicated service account

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin zfsnas
```

Place the binary and the `config/` directory somewhere the new user owns (e.g. `/opt/zfsnas/`):

```bash
sudo mkdir -p /opt/zfsnas
sudo chown zfsnas:zfsnas /opt/zfsnas
sudo cp zfsnas /opt/zfsnas/
```

### 2 — Add the sudoers entry

Run `sudo visudo -f /etc/sudoers.d/zfsnas` and paste the block below.

> **Note:** Paths are correct for Ubuntu 22.04 / 24.04 LTS. Verify with `which <command>` if you are on a different distribution.

```sudoers
# ── ZFS pool & dataset management ────────────────────────────────────────────
# since v1.0.0 — pool creation, import, status, dataset CRUD, snapshots
# since v2.0.0 — zpool scrub (Scrub Management page)
# since v4.0.0 — zpool offline/online/clear (Pool Fixer Wizard)
# since v5.0.0 — zfs load-key / unload-key (native encryption)
# since v6.1.0 — zfs send / zfs recv (remote & local dataset replication)
# since v6.3.21 — zpool replace (Pool Fixer Wizard: disk replacement for DEGRADED pools)
# since v6.3.26 — zfs allow (InterLink push & schedule replication: ZFS delegation to service user)
Cmnd_Alias ZFSNAS_ZFS = \
    /usr/sbin/zpool *, \
    /usr/sbin/zfs *

# ── Samba (SMB shares) ────────────────────────────────────────────────────────
# since v1.0.0 — Samba service control, user provisioning, share config write
# since v3.0.0 — useradd with optional --uid/--gid/--no-user-group (custom UID/GID feature);
#                groupadd -g <gid> for primary group pre-creation;
#                userdel -f / groupdel / gpasswd -d for user deletion
# since v6.0.0 — find (recycle bin cleanup: delete files older than retention in .recycle/)
# since v6.1.0 — smbstatus -S (active session listing on the SMB shares page)
# since v6.3.21 — mkdir -p / chmod 0700 / chown for SMB home folder creation;
#                 rmdir for home folder removal on user delete
# since v6.3.27 — chgrp/chmod 0770 replace chmod 777 for share path setup;
#                 groupadd --system sambashare ensures the group exists
Cmnd_Alias ZFSNAS_SMB = \
    /usr/bin/systemctl reload smbd, \
    /usr/bin/systemctl restart smbd, \
    /usr/bin/systemctl start smbd, \
    /usr/bin/systemctl stop smbd, \
    /usr/bin/systemctl start nmbd, \
    /usr/bin/systemctl stop nmbd, \
    /usr/sbin/useradd *, \
    /usr/sbin/usermod -aG sambashare *, \
    /usr/sbin/userdel -f *, \
    /usr/sbin/groupadd *, \
    /usr/sbin/groupdel *, \
    /usr/bin/gpasswd -d * sambashare, \
    /usr/bin/smbpasswd *, \
    /usr/bin/chgrp sambashare *, \
    /usr/bin/chmod 0770 *, \
    /usr/bin/chmod 0700 *, \
    /usr/bin/chown * *, \
    /usr/bin/mkdir -p *, \
    /usr/bin/rmdir *, \
    /usr/bin/tee /etc/samba/smb.conf, \
    /usr/bin/find *, \
    /usr/bin/smbstatus -S

# ── NFS shares ────────────────────────────────────────────────────────────────
# since v2.0.0 — NFS share management and export config write
Cmnd_Alias ZFSNAS_NFS = \
    /usr/sbin/exportfs -ra, \
    /usr/bin/systemctl start nfs-server, \
    /usr/bin/systemctl stop nfs-server, \
    /usr/bin/tee /etc/exports

# ── SMART & hardware monitoring ───────────────────────────────────────────────
# since v1.0.0 — SMART disk health data (SAS/SATA)
# since v3.0.0 — NVMe health and temperature monitoring
Cmnd_Alias ZFSNAS_SMART = \
    /usr/sbin/smartctl *, \
    /usr/bin/nvme smart-log -o json *

# ── Disk preparation & wipe ───────────────────────────────────────────────────
# since v1.0.0 — wipe and partition a disk before adding it to a pool;
#   wipefs clears signatures, sgdisk creates GPT layout, dd zero-fills,
#   partprobe + udevadm settle kernel/udev state, blkid reads UUIDs
# NOTE: sgdisk lives at /usr/sbin/sgdisk on Debian 12 and at /usr/bin/sgdisk on
#   some Ubuntu releases. Verify the correct path with `which sgdisk` and adjust
#   the two sgdisk lines below if needed.
Cmnd_Alias ZFSNAS_DISK = \
    /usr/sbin/wipefs -a *, \
    /usr/sbin/sgdisk *, \
    /usr/bin/dd if=/dev/zero *, \
    /usr/sbin/partprobe *, \
    /usr/bin/udevadm settle *, \
    /usr/sbin/blkid -o export

# ── iSCSI sharing ─────────────────────────────────────────────────────────────
# since v6.1.0 — iSCSI target management via targetcli-fb; service control for
#   rtslib-fb-targetctl / targetclid / tgt (whichever is installed)
Cmnd_Alias ZFSNAS_ISCSI = \
    /usr/bin/targetcli, \
    /usr/bin/systemctl start rtslib-fb-targetctl, \
    /usr/bin/systemctl stop rtslib-fb-targetctl, \
    /usr/bin/systemctl restart rtslib-fb-targetctl, \
    /usr/bin/systemctl start targetclid, \
    /usr/bin/systemctl stop targetclid, \
    /usr/bin/systemctl restart targetclid, \
    /usr/bin/systemctl start tgt, \
    /usr/bin/systemctl stop tgt, \
    /usr/bin/systemctl restart tgt

# ── S3 Object Server (MinIO) ──────────────────────────────────────────────────
# since v6.3.20 — optional MinIO S3 server; binary download, system account
#   setup, service unit, env file, data directory, TLS certs, and service control
Cmnd_Alias ZFSNAS_MINIO = \
    /usr/bin/wget -q -O /usr/local/bin/minio *, \
    /usr/bin/wget -q -O /usr/local/bin/mc *, \
    /usr/bin/chmod +x /usr/local/bin/minio, \
    /usr/bin/chmod +x /usr/local/bin/mc, \
    /usr/sbin/useradd --system --home-dir /var/lib/minio --shell /usr/sbin/nologin minio-user, \
    /usr/bin/mkdir -p /var/lib/minio, \
    /usr/bin/mkdir -p /var/lib/minio/.minio/certs, \
    /usr/bin/mkdir -p *, \
    /usr/bin/chown minio-user\:minio-user /var/lib/minio, \
    /usr/bin/chown -R minio-user\:minio-user /var/lib/minio/.minio/certs, \
    /usr/bin/chown -R minio-user\:minio-user *, \
    /usr/bin/chown root\:minio-user /etc/default/minio, \
    /usr/bin/chmod 640 /etc/default/minio, \
    /usr/bin/chmod 640 /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/cp * /var/lib/minio/.minio/certs/public.crt, \
    /usr/bin/cp * /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/rm -f /var/lib/minio/.minio/certs/public.crt, \
    /usr/bin/rm -f /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/tee /etc/systemd/system/minio.service, \
    /usr/bin/tee /etc/default/minio, \
    /usr/bin/systemctl enable minio, \
    /usr/bin/systemctl disable minio, \
    /usr/bin/systemctl start minio, \
    /usr/bin/systemctl stop minio, \
    /usr/bin/systemctl restart minio

# ── UPS Management (NUT) ──────────────────────────────────────────────────────
# since v6.3.22 — optional NUT daemon; install, auto-detect UPS, write /etc/nut/
#   config files, service control; uninstall purges packages and removes /etc/nut;
#   udevadm control/trigger used after writing USB udev rules for UPS detection
Cmnd_Alias ZFSNAS_UPS = \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get purge -y nut nut-client nut-server, \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get remove -y nut nut-client, \
    /usr/bin/udevadm control --reload-rules, \
    /usr/bin/udevadm trigger --subsystem-match=usb, \
    /usr/bin/systemctl enable nut-server, \
    /usr/bin/systemctl disable nut-server, \
    /usr/bin/systemctl start nut-server, \
    /usr/bin/systemctl stop nut-server, \
    /usr/bin/systemctl restart nut-server, \
    /usr/bin/systemctl enable nut-client, \
    /usr/bin/systemctl disable nut-client, \
    /usr/bin/systemctl start nut-client, \
    /usr/bin/systemctl stop nut-client, \
    /usr/bin/systemctl reset-failed, \
    /usr/sbin/nut-scanner *, \
    /usr/bin/chown root\:nut /etc/nut, \
    /usr/bin/chmod 750 /etc/nut, \
    /usr/bin/chown root\:nut /etc/nut/nut.conf, \
    /usr/bin/chown root\:nut /etc/nut/ups.conf, \
    /usr/bin/chown root\:nut /etc/nut/upsd.conf, \
    /usr/bin/chown root\:nut /etc/nut/upsd.users, \
    /usr/bin/chown root\:nut /etc/nut/upsmon.conf, \
    /usr/bin/chmod 640 /etc/nut/nut.conf, \
    /usr/bin/chmod 640 /etc/nut/ups.conf, \
    /usr/bin/chmod 640 /etc/nut/upsd.conf, \
    /usr/bin/chmod 640 /etc/nut/upsd.users, \
    /usr/bin/chmod 640 /etc/nut/upsmon.conf, \
    /usr/bin/tee /etc/nut/nut.conf, \
    /usr/bin/tee /etc/nut/upsd.conf, \
    /usr/bin/tee /etc/nut/upsd.users, \
    /usr/bin/tee /etc/nut/upsmon.conf, \
    /usr/bin/tee /etc/nut/ups.conf, \
    /usr/bin/rm -rf /etc/nut, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver.target.wants, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver@.service.d, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver@*

# ── Folder usage scanning ─────────────────────────────────────────────────────
# since v6.0.0 — Folder TreeMap feature; scans dataset mount points for per-folder sizes
Cmnd_Alias ZFSNAS_SCAN = \
    /usr/bin/du -b -d 6 *

# ── System management ─────────────────────────────────────────────────────────
# since v1.0.0 — timezone setting, shutdown/reboot from power menu, ZFS kernel module load
# since v3.0.0 — systemctl restart zfsnas ("Restart Portal" in the power menu)
# since v6.3.22 — tee entries for ARC Level 1 tuning (modprobe.d + sysfs parameters)
Cmnd_Alias ZFSNAS_SYSTEM = \
    /usr/bin/timedatectl set-timezone *, \
    /usr/sbin/shutdown *, \
    /usr/sbin/modprobe zfs, \
    /usr/bin/systemctl restart zfsnas, \
    /usr/bin/tee /etc/modprobe.d/zfs.conf, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_max, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_min

# ── OS updates & service installation ────────────────────────────────────────
# since v1.0.0 — prerequisite package install (apt-get install) and
#   systemd service setup (tee + daemon-reload + enable)
# since v3.0.0 — OS package updates (apt-get upgrade) from the Settings page
# since v6.1.0 — apt-get remove / autoremove for optional feature uninstall
#   (e.g. targetcli-fb removal when iSCSI feature is disabled)
Cmnd_Alias ZFSNAS_APT = \
    /usr/bin/apt-get *, \
    /usr/bin/tee /etc/systemd/system/zfsnas.service, \
    /usr/bin/systemctl daemon-reload, \
    /usr/bin/systemctl enable zfsnas

# ── Grant all of the above, passwordless, to the service account ──────────────
zfsnas ALL=(ALL) NOPASSWD: \
    ZFSNAS_ZFS, ZFSNAS_SMB, ZFSNAS_NFS, ZFSNAS_ISCSI, ZFSNAS_MINIO, ZFSNAS_UPS, ZFSNAS_SMART, ZFSNAS_DISK, ZFSNAS_SCAN, ZFSNAS_SYSTEM, ZFSNAS_APT
```

### 3 — Run the portal as the service account

Update `/etc/systemd/system/zfsnas.service` to use the new user:

```ini
[Service]
User=zfsnas
WorkingDirectory=/opt/zfsnas
ExecStart=/opt/zfsnas/zfsnas
```

### Notes

- **Web terminal** — the browser terminal runs a shell as the `zfsnas` user. With the restricted sudoers entry above, any `sudo` command typed in that terminal is still limited to the whitelist. If you do not use the web terminal feature you can remove the `/ws/terminal` route or simply accept that a logged-in admin can run a shell with the same restrictions.
- **`chgrp sambashare *` / `chmod 0770 *`** — applied to the directory of every newly created SMB share, so that members of the `sambashare` group can read and write while other OS users cannot. If your shares always live under a fixed parent (e.g. `/data`), you can tighten the wildcards to `/usr/bin/chgrp sambashare /data/*` and `/usr/bin/chmod 0770 /data/*`.
- **`tee` for config files** — write access is limited to the specific paths listed (`smb.conf`, `exports`, `zfsnas.service`, `zfs.conf`, and the two ARC sysfs paths). The wildcard form `tee *` is intentionally avoided.
- **`dd` / `wipefs` / `sgdisk`** — used by the "Wipe Disk" feature before adding a disk to a pool. These are destructive by design; ensure only trusted admins have access to the portal.
- **`zfs load-key` / `zfs unload-key`** — used for ZFS native encryption (v5.0.0+). Key files are stored in `config/keystore/` and are only readable by the `zfsnas` user.
- **`systemctl restart zfsnas`** — used by the "Restart Portal" option in the power menu (v3.0.0+). Only available to admin-role users.
- **`smbstatus -S`** — used to list active SMB sessions per share (v6.1.0+). On Debian/Ubuntu the `smbstatus` binary only exposes session data to root, so sudo is required. NFS session data comes from `showmount -a` and `/proc/fs/nfsd/clients/`, which are readable without sudo.
- **`find *`** — used to delete files and empty directories in `.recycle/` folders (v6.0.0+). The wildcard allows any path to be passed; scope is limited in practice to share mount points. If you want to tighten this, restrict to your shares root (e.g. `/usr/bin/find /data/*`).
- **`targetcli`** — used by the iSCSI sharing feature (v6.1.0+) to configure LIO targets, backstores, ACLs, and CHAP credentials via a piped script. The portal generates the full targetcli command sequence internally; no user-supplied input is passed directly to the shell. If you do not use iSCSI, you can omit `ZFSNAS_ISCSI` from the grant line and remove the `targetcli-fb` package.
- **iSCSI service names** — three possible service names are listed (`rtslib-fb-targetctl`, `targetclid`, `tgt`) to cover different `targetcli-fb` package versions and distributions. Only the service actually installed on your system will be used; the unused entries are harmless.
- **`wget` for MinIO binaries** — the portal downloads `minio` and `mc` directly from `dl.min.io` via `sudo wget`. The destination paths are fixed (`/usr/local/bin/minio` and `/usr/local/bin/mc`); only the source URL is a wildcard. If you prefer not to allow `wget` with sudo, pre-install the binaries manually and omit the `wget` lines from the sudoers entry.
- **`mkdir -p *` and `chown -R minio-user:minio-user *`** — the MinIO data directory is user-configured and can be any path, so these entries use wildcards. Scope is limited in practice to the path you set in the MinIO configuration. If your data directory is always under a fixed parent (e.g. `/data`), you can tighten these to `/usr/bin/mkdir -p /data/*` and `/usr/bin/chown -R minio-user\:minio-user /data/*`.
- **`nut-scanner *`** — the NUT USB/HID scanner runs under sudo because it requires raw USB device access. The portal always invokes it with either `-C -N` (fast USB-only scan) or `-a -t 2` (full scan with 2 s timeout). If you do not use the UPS feature you can omit `ZFSNAS_UPS` from the grant line entirely.
- **`/sbin/shutdown -h +0` (UPS watcher)** — the UPS shutdown watcher calls `/sbin/shutdown -h +0` directly (no `sudo` wrapper) to halt the system when battery thresholds are breached. This command does not appear in the sudoers whitelist; it relies on the process having shutdown permission via polkit (standard on Ubuntu 22.04+) or on the portal running as root. If you observe shutdown failures, add a polkit rule granting the `zfsnas` user shutdown permission, or add `/sbin/shutdown -h +0` to `ZFSNAS_SYSTEM` and update the code to use `sudo`.
- **`env DEBIAN_FRONTEND=noninteractive apt-get purge/remove`** — NUT uninstall wraps `apt-get` with the `env` command to suppress interactive prompts. The sudoers entry therefore targets `/usr/bin/env` (not `/usr/bin/apt-get` directly) and must match the exact argument sequence shown in `ZFSNAS_UPS`. The NUT *install* path (`apt-get install -y nut nut-client`) is already covered by the wildcard entry in `ZFSNAS_APT`.
- **`tee /etc/modprobe.d/zfs.conf` and sysfs tee entries** — used by the ARC Level 1 tuning feature (v6.3.22+) to persist ZFS ARC size limits across reboots and to apply them immediately via kernel sysfs. These are write-only paths; the portal only ever pipes well-formed `options zfs ...` lines or numeric byte values through `tee`.
- **`zfs send *` / `zfs recv *` / `zfs receive *`** — used by the remote replication feature (snapshot policies and standalone replication tasks) and the local dataset-to-dataset replication feature. `zfs send` streams a snapshot to stdout; `zfs recv`/`zfs receive` reads a stream from stdin. Both are required locally for local replication (`zfs send | zfs recv` piped on the same host). For remote replication and InterLink push, only `zfs send` is run locally — the remote side runs `zfs recv` via SSH under its own service account (no local sudo required for that side). Including both forms (`recv` and `receive`) is necessary because OpenZFS accepts either spelling.
- **`zfs allow *`** — used by the InterLink push feature and scheduled replication via InterLink servers to delegate ZFS permissions (`snapshot,send,receive,create,mount`) to the service account on every pool. This is required so that `zfs send` / `zfs recv` can be invoked without sudo when the remote side accepts the SSH connection as the non-root service user. The portal always restricts delegation to the service account's own username and to the specific permission set shown above.
- **`useradd *` / `groupadd *`** — broad wildcards are required because user creation accepts optional `--uid`, `--gid`, and `--no-user-group` flags (v3.0.0+ custom UID/GID feature). The original narrower forms (`useradd -M -s /usr/sbin/nologin *`, `groupadd --system sambashare`) are replaced to cover all combinations the portal may generate.
- **`userdel -f *` / `groupdel *` / `gpasswd -d * sambashare`** — used when deleting a portal user: Samba password entry is removed, the user is removed from the `sambashare` group, the Linux account is deleted, and the primary group is cleaned up.
- **`smbpasswd -x *`** — deletes the Samba password database entry for a user. The existing `smbpasswd -s -a *` entry covers creation/update; `-x` is needed for deletion.
- **`mkdir -p *` / `rmdir *` / `chmod 0700 *` / `chown * *`** (in `ZFSNAS_SMB`) — used when creating or deleting SMB home folders (v6.3.21+). The path is the user's home directory under the configured dataset mount point. `chmod 0700` and `chown` ensure only the owner can access their home directory.
- **`udevadm control --reload-rules` / `udevadm trigger --subsystem-match=usb`** — run after writing a udev rule for UPS USB detection (v6.3.22+). These are distinct from `udevadm settle` (used in disk prep) and require their own sudoers entries.
- **Command paths** — `sgdisk` lives at `/usr/sbin/sgdisk` on Debian 12 and at `/usr/bin/sgdisk` on some Ubuntu releases. The entries above use `/usr/sbin/sgdisk` (the Debian default). Verify the correct path with `which sgdisk` and adjust if needed. Other tools listed under `/usr/bin/` or `/usr/sbin/` follow the same rule — check with `which <command>` on your system.

---

## TLS / HTTPS

The portal generates a self-signed certificate on first run (`config/certs/`). Connections are HTTPS-only; there is no HTTP fallback. For production use you can replace the auto-generated certificate with one signed by a trusted CA — place your `cert.pem` and `key.pem` in the same directory.

---

## Authentication

- Session-based auth with server-side session store; cookies are `HttpOnly` and `Secure`.
- Three roles: **admin** (full access), **read-only** (no mutations), **smb-only** (SMB share access only).
- All login attempts are written to the audit log, including the client IP.

---

## Reporting vulnerabilities

Please open a [GitHub issue](https://github.com/macgaver/zfsnas-chezmoi/issues) or contact the maintainer directly via GitHub.
