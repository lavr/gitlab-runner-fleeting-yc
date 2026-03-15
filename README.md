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

- A Yandex Cloud Instance Group with a **FIXED** scale policy (Fleeting manages the size itself)
- Instance template: Ubuntu 22.04 with Docker installed, user `ubuntu` in the `docker` group
- SSH public key of the runner manager must be set in the instance template metadata
- A Service Account with the required IAM permissions (see below)

## IAM Permissions

The Service Account used by the plugin requires the following minimum permissions:

- `compute.instanceGroups.get` — read instance group info
- `compute.instanceGroups.update` — change target size (scale up)
- `compute.instanceGroups.delete` — delete specific instances (scale down via `DeleteInstances`)
- `compute.instances.get` — read instance details

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

The plugin **does not** manage SSH keys. You must configure the SSH key in the instance template metadata so that the runner manager can connect to the VMs.

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
| `instance_group_id` | string | **Yes** | — | Instance Group ID to manage |
| `key_file` | string | No | — | Path to IAM JSON key file. If empty, uses metadata service |
| `ssh_user` | string | No | `ubuntu` | Username for SSH connections |

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
