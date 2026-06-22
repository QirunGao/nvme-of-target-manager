# nvme-of-target-manager

Lightweight NVMe-oF target manager for Linux `configfs`/`nvmet`.

It is a tiny CLI around the kernel's existing NVMe target configfs interface:
read one TOML file, compare the requested target with current configfs state,
then start, stop, or report that target.

For multiple targets, use one TOML file per target and one systemd template
instance per file. There is no daemon, database, controller, or background
agent.

## Install

Download the package for your distro from GitHub Releases, then install it:

```sh
sudo dpkg -i nvme-of-target-manager_0.1.0_amd64.deb
sudo systemctl daemon-reload
```

or:

```sh
sudo rpm -Uvh nvme-of-target-manager_0.1.0_x86_64.rpm
sudo systemctl daemon-reload
```

The package is intentionally small and installs:

- `/usr/sbin/nvme-of-target-manager`
- `/etc/nvme-of/config.toml`
- `/lib/systemd/system/nvme-of-target-manager@.service`
- `/etc/modules-load.d/nvme-of-target.conf`

## Release Builds

GitHub Actions uses GoReleaser to build lightweight Linux binaries plus `.deb`
and `.rpm` packages for amd64 and arm64.

Create a GitHub release by pushing a version tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

There is also a manual GoReleaser workflow in GitHub Actions for re-running a
release from the UI.

## Configure Modules

The package installs `/etc/modules-load.d/nvme-of-target.conf`:

```conf
configfs
nvmet
# nvmet-tcp
# nvmet-rdma
```

Uncomment the transport module you use if you want systemd to preload it:

```sh
sudoedit /etc/modules-load.d/nvme-of-target.conf
sudo systemctl restart systemd-modules-load.service
```

The manager also attempts to load the configured transport module when it runs.

## Configure A Target

Create one config file per target under `/etc/nvme-of/`.

Example:

```toml
[subsystem]
nqn = "nqn.2026-04.local:disk1"

[namespace]
id = 1
backing_dev = "/dev/nvme1n1"

[port]
id = 1
transport = "tcp"
address_family = "ipv4"
address = "192.168.1.10"
service_id = 4420

[hosts]
allow_any_host = true
allowed = []

# Optional. RDMA-CM ToS hint for RoCE targets.
# This does not configure VLAN, PFC, DCB, tc, ethtool, or switches.
#
# [qos]
# enabled = true
# rdma_device = "mlx5_0"
# rdma_port = 1
# roce_tos = 106
# pcp_priority = 3
```

For an explicit host allow-list:

```toml
[hosts]
allow_any_host = false
allowed = [
  "nqn.2014-08.org.nvmexpress:uuid:00000000-0000-0000-0000-000000000000",
]
```

Save it as, for example:

```sh
sudo cp /etc/nvme-of/config.toml /etc/nvme-of/disk1.toml
sudoedit /etc/nvme-of/disk1.toml
```

## Run With systemd

Enable one service instance per config file:

```sh
sudo systemctl enable --now nvme-of-target-manager@disk1.toml.service
```

For multiple targets:

```sh
sudo systemctl enable --now nvme-of-target-manager@disk1.toml.service
sudo systemctl enable --now nvme-of-target-manager@disk2.toml.service
```

The template instance name maps to `/etc/nvme-of/<name>`, so
`nvme-of-target-manager@disk1.toml.service` uses `/etc/nvme-of/disk1.toml`.

## Multiple Targets

Use one TOML file per target. To avoid accidental overlap:

- Use a unique `subsystem.nqn` for each target.
- Use a unique `port.id` for each target unless you intentionally want to share
  one listener.
- If multiple targets share the same `port.id`, keep `transport`,
  `address_family`, `address`, and `service_id` identical in those configs.
- `namespace.id` only needs to be unique within the same `subsystem.nqn`.
  Different targets can all use `namespace.id = 1`.
- `allowed_hosts` belongs to a subsystem. Different targets can allow the same
  host NQN.
- Global host objects under `/sys/kernel/config/nvmet/hosts` are created if
  needed and are not removed by this tool.
- If multiple configs enable `[qos]` for the same `rdma_device` and
  `rdma_port`, keep `roce_tos` and `pcp_priority` identical. The manager does
  not arbitrate shared RDMA-CM port QoS.

## RDMA-CM QoS

For RoCE targets, the optional `[qos]` section writes one configfs attribute:

```text
/sys/kernel/config/rdma_cm/<rdma_device>/ports/<rdma_port>/default_roce_tos
```

Example:

```toml
[qos]
enabled = true
rdma_device = "mlx5_0"
rdma_port = 1
roce_tos = 106
pcp_priority = 3
```

`pcp_priority` must match the high three bits of `roce_tos`; for example,
`106 >> 5 == 3`.

This is only an RDMA-CM ToS hint. VLAN PCP maps, PFC, DCB, `tc`, `ethtool`, and
switch QoS are host/network fabric configuration and should be managed outside
this tool.

`stop` does not reset RDMA-CM QoS because it is device/port level state, not a
single target artifact.

## CLI

You can also run the tool directly:

```sh
sudo nvme-of-target-manager start  -c /etc/nvme-of/disk1.toml
sudo nvme-of-target-manager stop   -c /etc/nvme-of/disk1.toml
sudo nvme-of-target-manager status -c /etc/nvme-of/disk1.toml
```

`status` prints one of:

- `active`
- `inactive`
- `dirty`
- `blocked: <reason>`

It exits with `0` only when the target is `active`.

## What Gets Removed

`stop` removes only the artifacts for the selected config:

- the configured port-to-subsystem link
- the configured namespace
- symlinks under the configured subsystem's `allowed_hosts`
- the configured subsystem
- the configured port directory only when it is empty

It does not remove global host objects under
`/sys/kernel/config/nvmet/hosts`.
