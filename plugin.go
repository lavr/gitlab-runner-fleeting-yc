package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/hashicorp/go-hclog"
	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"gopkg.in/yaml.v2"
)

const shutdownDeleteTimeout = 5 * time.Minute

const managedByLabel = "fleeting-managed-by"
const managedByValue = "fleeting-plugin-yandexcloud"

// InstanceGroup implements provider.InstanceGroup for Yandex Cloud.
type InstanceGroup struct {
	Config

	log          hclog.Logger
	client       InstanceGroupClient
	sdk          *ycsdk.SDK
	settings     provider.Settings
	createdGroup bool

	sshPrivateKey []byte
	sshPublicKey  string

	// waitOp waits for an operation to complete. Set by Init; overridable for tests.
	waitOp func(ctx context.Context, op *operation.Operation) error
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

	if g.waitOp == nil {
		g.waitOp = g.defaultWaitOp
	}

	// Template file flow: find or create instance group.
	justCreated := false
	if g.TemplateFile != "" {
		groupID, err := g.findExistingGroup(ctx)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("searching for existing group: %w", err)
		}
		if groupID != "" {
			g.log.Info("reusing existing instance group", "group_id", groupID)
			g.InstanceGroupID = groupID
			g.createdGroup = true // group has managed-by label, so we own it
		} else {
			yamlContent, err := os.ReadFile(g.TemplateFile)
			if err != nil {
				return provider.ProviderInfo{}, fmt.Errorf("reading template file %s: %w", g.TemplateFile, err)
			}
			groupID, err = g.createGroup(ctx, string(yamlContent))
			if err != nil {
				return provider.ProviderInfo{}, fmt.Errorf("creating instance group: %w", err)
			}
			g.InstanceGroupID = groupID
			g.createdGroup = true
			justCreated = true
			g.log.Info("created instance group from template", "group_id", groupID)
		}
	}

	group, err := g.client.Get(ctx, &ig.GetInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
	})
	if err != nil {
		// Do not rollback on Get errors: they may be transient (Unavailable,
		// DeadlineExceeded, eventual consistency). The created group is healthy;
		// on the next Init call, findExistingGroup will rediscover it by label.
		return provider.ProviderInfo{}, fmt.Errorf("getting instance group %s: %w", g.InstanceGroupID, err)
	}

	if group.GetFolderId() != g.FolderID {
		validationErr := fmt.Errorf("instance group %s belongs to folder %s, but config specifies folder %s", g.InstanceGroupID, group.GetFolderId(), g.FolderID)
		if justCreated {
			if rbErr := g.rollbackCreatedGroup(ctx); rbErr != nil {
				return provider.ProviderInfo{}, errors.Join(validationErr, rbErr)
			}
		}
		return provider.ProviderInfo{}, validationErr
	}

	if _, ok := group.GetScalePolicy().GetScaleType().(*ig.ScalePolicy_FixedScale_); !ok {
		validationErr := fmt.Errorf("instance group %s must use a fixed scale policy; got %T", g.InstanceGroupID, group.GetScalePolicy().GetScaleType())
		if justCreated {
			if rbErr := g.rollbackCreatedGroup(ctx); rbErr != nil {
				return provider.ProviderInfo{}, errors.Join(validationErr, rbErr)
			}
		}
		return provider.ProviderInfo{}, validationErr
	}

	if g.GenerateSSHKey {
		// With OPPORTUNISTIC deploy policy, YC will never forcefully
		// recreate running instances.  Outdated VMs keep the old SSH
		// key and will be reported as "creating" forever — an
		// unrecoverable state without manual intervention.
		// This applies to both reused groups (pre-existing VMs) and
		// freshly created groups with fixed_scale.size > 0 (VMs booted
		// during creation, before the key is injected).
		strategy := group.GetDeployPolicy().GetStrategy()
		if strategy == ig.DeployPolicy_OPPORTUNISTIC {
			if justCreated {
				if rbErr := g.rollbackCreatedGroup(ctx); rbErr != nil {
					return provider.ProviderInfo{}, errors.Join(
						fmt.Errorf("generate_ssh_key is incompatible with OPPORTUNISTIC deploy policy on instance group %s; "+
							"instances created before key injection would never be recreated — "+
							"switch to PROACTIVE deploy policy", g.InstanceGroupID),
						rbErr,
					)
				}
			}
			return provider.ProviderInfo{}, fmt.Errorf(
				"generate_ssh_key is incompatible with OPPORTUNISTIC deploy policy on instance group %s; "+
					"instances would never be recreated with the new key — "+
					"switch to PROACTIVE deploy policy or delete the group and let the plugin recreate it",
				g.InstanceGroupID,
			)
		}
		if !justCreated {
			g.log.Warn("generate_ssh_key is enabled on a reused instance group; " +
				"already-running instances will not have the new public key until " +
				"they are recreated by the deploy policy")
		}
		privKey, pubKey, err := generateED25519Key()
		if err != nil {
			keygenErr := fmt.Errorf("generating SSH key: %w", err)
			if justCreated {
				if rbErr := g.rollbackCreatedGroup(ctx); rbErr != nil {
					return provider.ProviderInfo{}, errors.Join(keygenErr, rbErr)
				}
			}
			return provider.ProviderInfo{}, keygenErr
		}
		g.sshPrivateKey = privKey
		g.sshPublicKey = pubKey
		if err := g.injectSSHKey(ctx, group); err != nil {
			injectErr := fmt.Errorf("injecting SSH key: %w", err)
			if justCreated {
				if rbErr := g.rollbackCreatedGroup(ctx); rbErr != nil {
					return provider.ProviderInfo{}, errors.Join(injectErr, rbErr)
				}
			}
			return provider.ProviderInfo{}, injectErr
		}
		g.log.Info("generated and injected ephemeral SSH key")
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
			state := mapState(inst.GetStatus())
			// When SSH key generation is active, RUNNING_OUTDATED instances
			// still have the old public key and cannot be connected to with
			// the new private key. Report them as creating so the fleeting
			// framework does not attempt to use them until they are recreated
			// by the deploy policy with the updated template.
			if g.sshPrivateKey != nil && inst.GetStatus() == ig.ManagedInstance_RUNNING_OUTDATED {
				state = provider.StateCreating
			}
			fn(inst.GetId(), state)
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

			if g.sshPrivateKey != nil {
				info.ConnectorConfig.Key = g.sshPrivateKey
				info.ConnectorConfig.UseStaticCredentials = true
				// The generated key is authorized for g.SSHUser; override
				// any connector_config.username to prevent auth failures
				// when the two diverge.
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

// Shutdown logs the shutdown and cleans up. If the group was created by the plugin
// and DeleteOnShutdown is set, the group is deleted.
func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	g.log.Info("shutting down yandex cloud plugin")

	// Zero out private key material before shutdown.
	for i := range g.sshPrivateKey {
		g.sshPrivateKey[i] = 0
	}
	g.sshPrivateKey = nil

	defer func() {
		if g.sdk != nil {
			_ = g.sdk.Shutdown(context.Background())
		}
	}()

	if g.createdGroup && g.DeleteOnShutdown {
		// Use a detached context with a timeout so that delete completes
		// even if the caller's context is cancelled (e.g. on SIGTERM),
		// but does not block indefinitely if the API stalls.
		deleteCtx, cancel := context.WithTimeout(context.Background(), shutdownDeleteTimeout)
		defer cancel()
		g.log.Info("deleting instance group", "group_id", g.InstanceGroupID)
		op, err := g.client.Delete(deleteCtx, &ig.DeleteInstanceGroupRequest{
			InstanceGroupId: g.InstanceGroupID,
		})
		if err != nil {
			return fmt.Errorf("deleting instance group %s: %w", g.InstanceGroupID, err)
		}
		if g.waitOp != nil {
			if err := g.waitOp(deleteCtx, op); err != nil {
				return fmt.Errorf("waiting for instance group deletion: %w", err)
			}
		}
		g.log.Info("instance group deleted", "group_id", g.InstanceGroupID)
	}

	return nil
}

// rollbackCreatedGroup attempts to delete a group that was just created but
// failed subsequent validation. It uses a detached context so the rollback
// succeeds even if the caller's context is canceled. Returns an error if
// rollback fails so the caller can surface it alongside the validation error.
func (g *InstanceGroup) rollbackCreatedGroup(_ context.Context) error {
	g.log.Warn("rolling back newly created instance group", "group_id", g.InstanceGroupID)
	rollbackCtx, cancel := context.WithTimeout(context.Background(), shutdownDeleteTimeout)
	defer cancel()
	op, err := g.client.Delete(rollbackCtx, &ig.DeleteInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
	})
	if err != nil {
		return fmt.Errorf("rollback: deleting instance group %s: %w", g.InstanceGroupID, err)
	}
	if g.waitOp != nil {
		if err := g.waitOp(rollbackCtx, op); err != nil {
			return fmt.Errorf("rollback: waiting for deletion of instance group %s: %w", g.InstanceGroupID, err)
		}
	}
	g.createdGroup = false
	return nil
}

// defaultWaitOp waits for a YC operation to complete using the SDK.
func (g *InstanceGroup) defaultWaitOp(ctx context.Context, op *operation.Operation) error {
	sdkOp, err := g.sdk.WrapOperation(op, nil)
	if err != nil {
		return fmt.Errorf("wrapping operation: %w", err)
	}
	return sdkOp.Wait(ctx)
}

// injectLabels merges the given labels into the YAML content's labels map.
func injectLabels(yamlContent string, labels map[string]string) (string, error) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &doc); err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}
	if doc == nil {
		doc = make(map[string]interface{})
	}

	existing, _ := doc["labels"].(map[interface{}]interface{})
	if existing == nil {
		existing = make(map[interface{}]interface{})
	}
	for k, v := range labels {
		existing[k] = v
	}
	doc["labels"] = existing

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshalling YAML: %w", err)
	}
	return string(out), nil
}

// overrideName sets the name field in the YAML content to the given name.
func overrideName(yamlContent string, name string) (string, error) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &doc); err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}
	if doc == nil {
		doc = make(map[string]interface{})
	}
	doc["name"] = name

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshalling YAML: %w", err)
	}
	return string(out), nil
}

// findExistingGroup searches for an instance group by name and managed-by label.
// Returns the group ID if exactly one ACTIVE or STARTING match is found, empty string if none,
// or error if multiple matches or an owned group is in an inactive/stopping state
// (STOPPED/PAUSED/STOPPING). Groups in other states (DELETING, STATUS_UNSPECIFIED)
// are silently skipped.
func (g *InstanceGroup) findExistingGroup(ctx context.Context) (string, error) {
	var matches []*ig.InstanceGroup
	var pageToken string
	for {
		resp, err := g.client.List(ctx, &ig.ListInstanceGroupsRequest{
			FolderId:  g.FolderID,
			PageSize:  1000,
			PageToken: pageToken,
		})
		if err != nil {
			return "", fmt.Errorf("listing instance groups: %w", err)
		}

		for _, group := range resp.GetInstanceGroups() {
			if group.GetName() != g.GroupName {
				continue
			}
			if group.GetLabels()[managedByLabel] != managedByValue {
				continue
			}
			// Error on owned groups in inactive or stopping states to prevent duplicates.
			if s := group.GetStatus(); s == ig.InstanceGroup_STOPPED || s == ig.InstanceGroup_PAUSED || s == ig.InstanceGroup_STOPPING {
				return "", fmt.Errorf("found managed instance group %s in %s state; delete it or restore it before restarting",
					group.GetId(), s)
			}
			// Only reuse groups in known active states.
			s := group.GetStatus()
			if s != ig.InstanceGroup_ACTIVE && s != ig.InstanceGroup_STARTING {
				continue
			}
			matches = append(matches, group)
		}

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0].GetId(), nil
	default:
		return "", fmt.Errorf("found %d instance groups matching name %q with label %s=%s; specify instance_group_id explicitly",
			len(matches), g.GroupName, managedByLabel, managedByValue)
	}
}

// createGroup creates an instance group from YAML content, injecting managed-by labels
// and overriding the name for idempotency, then waits for the operation to complete.
func (g *InstanceGroup) createGroup(ctx context.Context, yamlContent string) (string, error) {
	labeled, err := injectLabels(yamlContent, map[string]string{
		managedByLabel: managedByValue,
	})
	if err != nil {
		return "", fmt.Errorf("injecting labels: %w", err)
	}

	// Override name in YAML to match GroupName for idempotent lookups.
	labeled, err = overrideName(labeled, g.GroupName)
	if err != nil {
		return "", fmt.Errorf("overriding group name: %w", err)
	}

	op, err := g.client.CreateFromYaml(ctx, &ig.CreateInstanceGroupFromYamlRequest{
		FolderId:          g.FolderID,
		InstanceGroupYaml: labeled,
	})
	if err != nil {
		return "", fmt.Errorf("calling CreateFromYaml: %w", err)
	}

	if g.waitOp != nil {
		if err := g.waitOp(ctx, op); err != nil {
			return "", fmt.Errorf("waiting for creation: %w", err)
		}
	}

	// Extract instance group ID from operation metadata.
	var meta ig.CreateInstanceGroupMetadata
	if op.GetMetadata() != nil {
		if err := op.GetMetadata().UnmarshalTo(&meta); err != nil {
			return "", fmt.Errorf("unmarshalling operation metadata: %w", err)
		}
	}
	if meta.GetInstanceGroupId() == "" {
		return "", fmt.Errorf("operation metadata does not contain instance group ID")
	}

	return meta.GetInstanceGroupId(), nil
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
