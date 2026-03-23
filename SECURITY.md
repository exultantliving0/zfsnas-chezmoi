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
# since v2.0.0 — zpool scrub / scrub -s (Scrub Management page)
# since v4.0.0 — zpool offline/online/clear (Pool Fixer Wizard)
# since v5.0.0 — zfs load-key / unload-key (native encryption)
# since v6.3.21 — zpool replace -f (Pool Fixer Wizard: disk replacement for DEGRADED pools)
Cmnd_Alias ZFSNAS_ZFS = \
    /usr/sbin/zpool list *, \
    /usr/sbin/zpool status, \
    /usr/sbin/zpool status *, \
    /usr/sbin/zpool get *, \
    /usr/sbin/zpool create *, \
    /usr/sbin/zpool import, \
    /usr/sbin/zpool import *, \
    /usr/sbin/zpool import -f *, \
    /usr/sbin/zpool add *, \
    /usr/sbin/zpool attach *, \
    /usr/sbin/zpool detach *, \
    /usr/sbin/zpool remove *, \
    /usr/sbin/zpool destroy *, \
    /usr/sbin/zpool upgrade *, \
    /usr/sbin/zpool scrub *, \
    /usr/sbin/zpool scrub -s *, \
    /usr/sbin/zpool offline *, \
    /usr/sbin/zpool online *, \
    /usr/sbin/zpool clear *, \
    /usr/sbin/zpool replace -f *, \
    /usr/sbin/zfs list *, \
    /usr/sbin/zfs get *, \
    /usr/sbin/zfs set *, \
    /usr/sbin/zfs inherit *, \
    /usr/sbin/zfs create *, \
    /usr/sbin/zfs destroy *, \
    /usr/sbin/zfs destroy -r *, \
    /usr/sbin/zfs snapshot *, \
    /usr/sbin/zfs rollback -r *, \
    /usr/sbin/zfs clone *, \
    /usr/sbin/zfs mount *, \
    /usr/sbin/zfs load-key *, \
    /usr/sbin/zfs unload-key *

# ── Samba (SMB shares) ────────────────────────────────────────────────────────
# since v1.0.0 — Samba service control, user provisioning, share config write
# since v6.0.0 — find (recycle bin cleanup: delete files older than retention in .recycle/)
# since v6.1.0 — smbstatus -S (active session listing on the SMB shares page)
Cmnd_Alias ZFSNAS_SMB = \
    /usr/bin/systemctl reload smbd, \
    /usr/bin/systemctl restart smbd, \
    /usr/bin/systemctl start smbd, \
    /usr/bin/systemctl stop smbd, \
    /usr/bin/systemctl start nmbd, \
    /usr/bin/systemctl stop nmbd, \
    /usr/sbin/useradd -M -s /usr/sbin/nologin *, \
    /usr/sbin/usermod -aG sambashare *, \
    /usr/bin/smbpasswd -s -a *, \
    /usr/bin/chmod 777 *, \
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
    /usr/sbin/smartctl -j -a *, \
    /usr/sbin/smartctl -j -i *, \
    /usr/bin/nvme smart-log -o json *

# ── Disk preparation & wipe ───────────────────────────────────────────────────
# since v1.0.0 — wipe and partition a disk before adding it to a pool;
#   wipefs clears signatures, sgdisk creates GPT layout, dd zero-fills,
#   partprobe + udevadm settle kernel/udev state, blkid reads UUIDs
Cmnd_Alias ZFSNAS_DISK = \
    /usr/bin/wipefs -a *, \
    /usr/bin/sgdisk --zap-all *, \
    /usr/bin/sgdisk -n 1\:0\:0 -t 1\:BF01 *, \
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
#   config files, service control; uninstall purges packages and removes /etc/nut
Cmnd_Alias ZFSNAS_UPS = \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get purge -y nut nut-client nut-server, \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get remove -y nut nut-client, \
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
    /usr/sbin/shutdown -r now, \
    /usr/sbin/shutdown -h now, \
    /usr/sbin/modprobe zfs, \
    /usr/bin/systemctl restart zfsnas, \
    /usr/bin/tee /etc/modprobe.d/zfs.conf, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_max, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_min

# ── OS updates & service installation ────────────────────────────────────────
# since v1.0.0 — prerequisite package install (apt-get install) and
#   systemd service setup (tee + daemon-reload + enable)
# since v3.0.0 — OS package updates (apt-get upgrade) from the Settings page
Cmnd_Alias ZFSNAS_APT = \
    /usr/bin/apt-get update -qq, \
    /usr/bin/apt-get install -y *, \
    /usr/bin/apt-get upgrade -y, \
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
- **`chmod 777`** — the portal applies this to newly created SMB share paths. If your shares always live under a fixed parent (e.g. `/data`), you can tighten this to `/usr/bin/chmod 777 /data/*`.
- **`tee` for config files** — write access is limited to the three specific paths listed (`smb.conf`, `exports`, `zfsnas.service`). The wildcard form `tee *` is intentionally avoided.
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
- **Command paths** — paths shown are for Ubuntu 22.04/24.04. Some tools (`sgdisk`, `wipefs`, `nvme`) may live under `/usr/sbin/` instead of `/usr/bin/` on older releases; verify with `which <command>`.

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
