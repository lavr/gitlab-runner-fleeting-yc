# fleeting-plugin-yandexcloud

A [Fleeting](https://docs.gitlab.com/runner/fleet_scaling/) plugin for autoscaling GitLab Runner instances using [Yandex Cloud Instance Groups](https://cloud.yandex.ru/docs/compute/concepts/instance-groups/).

This plugin implements the `provider.InstanceGroup` interface from the [Fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) library, allowing GitLab Runner's `docker-autoscaler` executor to manage Yandex Cloud VM instances.

## Compatibility

| Component | Version |
|---|---|
| GitLab Runner | >= 16.11 |
| Fleeting | latest |
| Go | >= 1.25 |

## Prerequisites

- A Yandex Cloud Instance Group with a **FIXED** scale policy (Fleeting manages the size itself), **or** a YAML template file for auto-creation (see [Auto-Create Instance Group](#auto-create-instance-group))
- Instance template: Ubuntu 22.04 with Docker installed, user `ubuntu` in the `docker` group
- SSH public key of the runner manager must be set in the instance template metadata (or use `generate_ssh_key` for automatic key management)
- A Service Account with the required IAM permissions (see below)

## IAM Permissions

The Service Account used by the plugin requires the following minimum permissions:

- `compute.instanceGroups.get` — read instance group info
- `compute.instanceGroups.update` — change target size (scale up)
- `compute.instanceGroups.delete` — delete specific instances (scale down via `DeleteInstances`)
- `compute.instances.get` — read instance details

When using `template_file` mode (auto-create), the following additional permissions are required:

- `compute.instanceGroups.create` — create instance groups from YAML templates
- `compute.instanceGroups.list` — list instance groups (for idempotency lookups)

## Installation

### Via fleeting install (Runner >= 16.11)

In your `config.toml`, set:

```toml
plugin = "lavr/gitlab-runner-fleeting-yc:latest"
```

Then run:

```bash
gitlab-runner fleeting install
```

### Manual installation

```bash
curl -L https://github.com/lavr/gitlab-runner-fleeting-yc/releases/latest/download/fleeting-plugin-yandexcloud-linux-amd64.tar.gz | tar xz
sudo mv fleeting-plugin-yandexcloud /usr/local/bin/
```

## SSH Key Setup

There are two ways to handle SSH keys:

### Option 1: Automatic key generation (recommended)

Set `generate_ssh_key = true` in `plugin_config`. The plugin will:
1. Generate an ephemeral ED25519 key pair on startup
2. Inject the public key into the instance group template metadata (`ssh-keys`)
3. Provide the private key to the runner via `ConnectInfo()`

No manual key management is needed — keys are fully automated.

```toml
[runners.autoscaler.plugin_config]
  folder_id         = "b1gxxxxxxxxxxxxxxxxx"
  instance_group_id = "cl1xxxxxxxxxxxxxxxxx"
  ssh_user          = "ubuntu"
  generate_ssh_key  = true

[runners.autoscaler.connector_config]
  username          = "ubuntu"
  use_external_addr = true
  # No key_path needed — the plugin provides the key automatically
```

### Option 2: Manual key management

Configure the SSH key in the instance template metadata so that the runner manager can connect to the VMs.

In your instance template metadata, add:

```
ssh-keys: ubuntu:<contents of your public key>
```

The runner manager's private key should be configured in `connector_config.key_path` in `config.toml`.

## Configuration Reference

All fields are set under `[runners.autoscaler.plugin_config]` in `config.toml`.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `folder_id` | string | **Yes** | — | Yandex Cloud folder ID |
| `instance_group_id` | string | **Yes**\* | — | Instance Group ID to manage |
| `template_file` | string | **Yes**\* | — | Path to YAML template for auto-creating an instance group |
| `delete_on_shutdown` | bool | No | `false` | Delete the auto-created group when the plugin shuts down |
| `group_name` | string | No | `fleeting-plugin-yandexcloud` | Name used to find or create the instance group (for idempotency) |
| `key_file` | string | No | — | Path to IAM JSON key file. If empty, uses metadata service |
| `ssh_user` | string | No | `ubuntu` | Username for SSH connections |
| `generate_ssh_key` | bool | No | `false` | Auto-generate ephemeral ED25519 SSH key pair and inject into instance template metadata |

\* Exactly one of `instance_group_id` or `template_file` must be provided. They are mutually exclusive.

### Authentication

The plugin supports two authentication methods:

1. **IAM key file** — set `key_file` to the path of a JSON service account key
2. **Metadata service** — if `key_file` is empty, the plugin uses the instance's service account (for runners running on YC VMs)

## Full config.toml Example

```toml
concurrent = 10

[[runners]]
  name     = "yc-docker-autoscaler"
  url      = "https://gitlab.example.com/"
  token    = "glrt-xxxxxxxxxxxx"
  executor = "docker-autoscaler"

  [runners.docker]
    image       = "alpine:latest"
    pull_policy = ["if-not-present"]

  [runners.autoscaler]
    plugin                = "fleeting-plugin-yandexcloud"
    capacity_per_instance = 1
    max_use_count         = 10
    max_instances         = 5

    instance_ready_command = "cloud-init status --wait || test $? -eq 2"

    [runners.autoscaler.plugin_config]
      folder_id         = "b1gxxxxxxxxxxxxxxxxx"
      instance_group_id = "cl1xxxxxxxxxxxxxxxxx"
      # key_file = "/etc/gitlab-runner/yc-key.json"
      ssh_user          = "ubuntu"

    [runners.autoscaler.connector_config]
      username          = "ubuntu"
      use_external_addr = true
      # key_path = "/etc/gitlab-runner/runner-ssh-key"

    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time  = "20m"
```

## Creating Instance Group

Create an instance group with the YC CLI:

```bash
yc compute instance-group create \
  --name gitlab-runners \
  --folder-id b1gxxxxxxxxxxxxxxxxx \
  --service-account-id ajexxxxxxxxxxxxxxxxx \
  --fixed-scale-size 0 \
  --template '{
    "platform_id": "standard-v3",
    "resources_spec": {
      "memory": "4294967296",
      "cores": "2"
    },
    "boot_disk_spec": {
      "mode": "READ_WRITE",
      "disk_spec": {
        "size": "21474836480",
        "image_id": "<ubuntu-22.04-docker-image-id>"
      }
    },
    "network_interface_specs": [{
      "network_id": "<network-id>",
      "subnet_ids": ["<subnet-id>"],
      "primary_v4_address_spec": {
        "one_to_one_nat_spec": {
          "ip_version": "IPV4"
        }
      }
    }],
    "metadata": {
      "ssh-keys": "ubuntu:<your-runner-manager-public-key>"
    }
  }' \
  --zone ru-central1-a
```

Start with `--fixed-scale-size 0` — Fleeting will manage the size.

## Auto-Create Instance Group

Instead of creating an instance group manually and providing its ID, you can supply a YAML template file. The plugin will create the group automatically during `Init()`.

### How it works

1. On startup, the plugin searches for an existing group by `group_name` and the label `fleeting-managed-by=fleeting-plugin-yandexcloud`.
2. If a matching active group is found, it is reused (idempotent restart). If a managed group is found in `STOPPED` or `PAUSED` state, the plugin returns an error — delete or restore the group before restarting.
3. If no matching group exists, the plugin reads the YAML template, injects the managed-by label, and calls the YC `CreateFromYaml` API. If post-creation validation fails (e.g. the template uses auto-scale instead of fixed-scale), the plugin rolls back by deleting the newly created group.
4. On shutdown, if `delete_on_shutdown` is `true` and the group was created by the plugin, it is deleted (with a 5-minute timeout to prevent indefinite blocking).

### Example config

```toml
[runners.autoscaler.plugin_config]
  folder_id          = "b1gxxxxxxxxxxxxxxxxx"
  template_file      = "/etc/gitlab-runner/instance-group-template.yaml"
  group_name         = "my-runners"         # optional, default: "fleeting-plugin-yandexcloud"
  delete_on_shutdown = true                  # optional, default: false
  ssh_user           = "ubuntu"
```

An example YAML template is provided in [`examples/instance-group-template.yaml`](examples/instance-group-template.yaml). The format is the same as `yc compute instance-group create --file=spec.yaml`.

### Idempotency

If the plugin restarts, it will find the previously created group by name and label instead of creating a duplicate. If multiple groups match, the plugin returns an error and asks you to specify `instance_group_id` explicitly.

## Troubleshooting

### "instance group not found" error

Verify that `instance_group_id` is correct and the service account has `compute.instanceGroups.get` permission.

### "no external IPv4 address" error

Your instances don't have NAT configured. Either:
- Add a one-to-one NAT to the instance template, or
- Set `use_external_addr = false` in `connector_config` to use internal IPs (requires network connectivity between runner manager and VMs)

### Authentication failures

- **Using key_file:** Verify the file exists and contains a valid JSON IAM key
- **Using metadata service:** Ensure the runner manager VM has a service account attached with the required permissions

### Instances not becoming ready

Ensure your VM image has:
- Docker installed and running
- The `ubuntu` user in the `docker` group
- SSH server running
- `cloud-init` support (for `instance_ready_command`)

### Scale-up is slow

Instance Group operations in Yandex Cloud may take 1-3 minutes. This is normal behavior. Adjust `idle_count` and `idle_time` in autoscaler policies to maintain warm instances.
