package main

import (
	"context"
	"fmt"
	"math"

	"github.com/hashicorp/go-hclog"
	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// InstanceGroup implements provider.InstanceGroup for Yandex Cloud.
type InstanceGroup struct {
	Config

	log      hclog.Logger
	client   InstanceGroupClient
	sdk      *ycsdk.SDK
	settings provider.Settings
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

// Init initializes the plugin, creating a YC SDK client and validating the instance group.
func (g *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.log = logger
	g.settings = settings

	if err := g.Config.validate(); err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("invalid config: %w", err)
	}

	if g.client == nil {
		sdk, client, err := g.buildClient(ctx)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("creating YC SDK client: %w", err)
		}
		g.sdk = sdk
		g.client = client
	}

	group, err := g.client.Get(ctx, &ig.GetInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
	})
	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("getting instance group %s: %w", g.InstanceGroupID, err)
	}

	if group.GetFolderId() != g.FolderID {
		return provider.ProviderInfo{}, fmt.Errorf("instance group %s belongs to folder %s, but config specifies folder %s", g.InstanceGroupID, group.GetFolderId(), g.FolderID)
	}

	if _, ok := group.GetScalePolicy().GetScaleType().(*ig.ScalePolicy_FixedScale_); !ok {
		return provider.ProviderInfo{}, fmt.Errorf("instance group %s must use a fixed scale policy; got %T", g.InstanceGroupID, group.GetScalePolicy().GetScaleType())
	}

	g.log.Info("initialized yandex cloud plugin",
		"group_name", group.GetName(),
		"group_id", group.GetId(),
		"folder_id", group.GetFolderId(),
		"target_size", group.GetManagedInstancesState().GetTargetSize(),
	)

	return provider.ProviderInfo{
		ID:      "yandexcloud",
		MaxSize: math.MaxInt32, // effectively unbounded; the fleeting framework limits via max_instances from autoscaler config
	}, nil
}

// Update lists all instances in the group and calls fn for each with the mapped state.
func (g *InstanceGroup) Update(ctx context.Context, fn func(instance string, state provider.State)) error {
	var pageToken string
	for {
		resp, err := g.client.ListInstances(ctx, &ig.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupID,
			PageSize:        1000,
			PageToken:       pageToken,
		})
		if err != nil {
			return fmt.Errorf("listing instances: %w", err)
		}

		for _, inst := range resp.GetInstances() {
			fn(inst.GetId(), mapState(inst.GetStatus()))
		}

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	return nil
}

// Increase requests additional instances by updating the group's target size.
func (g *InstanceGroup) Increase(ctx context.Context, n int) (int, error) {
	group, err := g.client.Get(ctx, &ig.GetInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
	})
	if err != nil {
		return 0, fmt.Errorf("getting current size: %w", err)
	}

	currentSize := int(group.GetScalePolicy().GetFixedScale().GetSize())
	if n <= 0 {
		return 0, nil
	}
	newSize := currentSize + n

	_, err = g.client.Update(ctx, &ig.UpdateInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"scale_policy"},
		},
		ScalePolicy: &ig.ScalePolicy{
			ScaleType: &ig.ScalePolicy_FixedScale_{
				FixedScale: &ig.ScalePolicy_FixedScale{
					Size: int64(newSize),
				},
			},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("updating target size to %d: %w", newSize, err)
	}

	g.log.Info("increased instance group size", "from", currentSize, "to", newSize, "delta", n)
	return n, nil
}

// Decrease removes specific instances from the group using DeleteInstances.
func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	_, err := g.client.DeleteInstances(ctx, &ig.DeleteInstancesRequest{
		InstanceGroupId:    g.InstanceGroupID,
		ManagedInstanceIds: instances,
	})
	if err != nil {
		g.log.Error("failed to delete instances", "instances", instances, "error", err)
		return nil, fmt.Errorf("deleting instances: %w", err)
	}

	g.log.Info("decreased instance group", "removed", len(instances))
	return instances, nil
}

// ConnectInfo returns connection details for a specific instance.
func (g *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	var pageToken string
	for {
		resp, err := g.client.ListInstances(ctx, &ig.ListInstanceGroupInstancesRequest{
			InstanceGroupId: g.InstanceGroupID,
			PageSize:        1000,
			PageToken:       pageToken,
		})
		if err != nil {
			return provider.ConnectInfo{}, fmt.Errorf("listing instances: %w", err)
		}

		for _, inst := range resp.GetInstances() {
			if inst.GetId() != instance {
				continue
			}

			externalIP := extractExternalIP(inst)
			internalIP := extractInternalIP(inst)

			info := provider.ConnectInfo{
				ConnectorConfig: g.settings.ConnectorConfig,
				ID:              inst.GetId(),
			}

			// Override fields the plugin is authoritative for.
			info.ConnectorConfig.OS = "linux"
			info.ConnectorConfig.Protocol = provider.ProtocolSSH
			if info.ConnectorConfig.Username == "" {
				info.ConnectorConfig.Username = g.SSHUser
			}

			if externalIP != "" {
				info.ExternalAddr = externalIP + ":22"
			}
			if internalIP != "" {
				info.InternalAddr = internalIP + ":22"
			}

			return info, nil
		}

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	return provider.ConnectInfo{}, fmt.Errorf("instance %s not found", instance)
}

// Heartbeat is a no-op; Yandex Cloud manages instance health.
func (g *InstanceGroup) Heartbeat(_ context.Context, _ string) error {
	return nil
}

// Shutdown logs the shutdown and cleans up. No resources are deleted.
func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	g.log.Info("shutting down yandex cloud plugin")
	if g.sdk != nil {
		return g.sdk.Shutdown(ctx)
	}
	return nil
}

func (g *InstanceGroup) buildClient(ctx context.Context) (*ycsdk.SDK, InstanceGroupClient, error) {
	var creds ycsdk.Credentials
	if g.KeyFile != "" {
		key, err := iamkey.ReadFromJSONFile(g.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading key file %s: %w", g.KeyFile, err)
		}
		creds, err = ycsdk.ServiceAccountKey(key)
		if err != nil {
			return nil, nil, fmt.Errorf("creating credentials from key file: %w", err)
		}
		g.log.Info("using service account key file for authentication", "key_file", g.KeyFile)
	} else {
		creds = ycsdk.InstanceServiceAccount()
		g.log.Info("using instance metadata service account for authentication")
	}

	sdk, err := ycsdk.Build(ctx, ycsdk.Config{
		Credentials: creds,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("building YC SDK: %w", err)
	}

	return sdk, &sdkClient{inner: sdk.InstanceGroup().InstanceGroup()}, nil
}

func extractExternalIP(inst *ig.ManagedInstance) string {
	if len(inst.GetNetworkInterfaces()) == 0 {
		return ""
	}
	return inst.GetNetworkInterfaces()[0].GetPrimaryV4Address().GetOneToOneNat().GetAddress()
}

func extractInternalIP(inst *ig.ManagedInstance) string {
	if len(inst.GetNetworkInterfaces()) == 0 {
		return ""
	}
	return inst.GetNetworkInterfaces()[0].GetPrimaryV4Address().GetAddress()
}

// mapState converts a Yandex Cloud ManagedInstance status to a fleeting provider.State.
func mapState(status ig.ManagedInstance_Status) provider.State {
	switch status {
	case ig.ManagedInstance_RUNNING_ACTUAL, ig.ManagedInstance_RUNNING_OUTDATED:
		return provider.StateRunning
	case ig.ManagedInstance_CREATING_INSTANCE,
		ig.ManagedInstance_STARTING_INSTANCE,
		ig.ManagedInstance_OPENING_TRAFFIC,
		ig.ManagedInstance_AWAITING_WARMUP_DURATION,
		ig.ManagedInstance_AWAITING_STARTUP_DURATION,
		ig.ManagedInstance_CHECKING_HEALTH,
		ig.ManagedInstance_UPDATING_INSTANCE,
		ig.ManagedInstance_PREPARING_RESOURCES:
		return provider.StateCreating
	case ig.ManagedInstance_STOPPING_INSTANCE,
		ig.ManagedInstance_STOPPED,
		ig.ManagedInstance_DELETING_INSTANCE,
		ig.ManagedInstance_CLOSING_TRAFFIC:
		return provider.StateDeleting
	case ig.ManagedInstance_DELETED:
		return provider.StateDeleted
	default:
		return provider.StateCreating
	}
}
