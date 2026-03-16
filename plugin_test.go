package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/anypb"
)

// mockClient implements InstanceGroupClient for testing.
type mockClient struct {
	group     *ig.InstanceGroup
	instances []*ig.ManagedInstance

	getErr             error
	listInstancesErr   error
	updateErr          error
	deleteInstancesErr error
	createFromYamlErr  error
	deleteErr          error
	listErr            error

	lastUpdateReq          *ig.UpdateInstanceGroupRequest
	lastDeleteInstancesReq *ig.DeleteInstancesRequest
	lastCreateFromYamlReq  *ig.CreateInstanceGroupFromYamlRequest
	lastDeleteReq          *ig.DeleteInstanceGroupRequest
	lastListReq            *ig.ListInstanceGroupsRequest
	createFromYamlOp       *operation.Operation

	// Pagination support: maps page token to response.
	// If nil, returns all instances in one page.
	paginatedResponses map[string]*ig.ListInstanceGroupInstancesResponse

	// List groups response for the List method.
	listGroupsResponse *ig.ListInstanceGroupsResponse
}

func (m *mockClient) Get(_ context.Context, _ *ig.GetInstanceGroupRequest) (*ig.InstanceGroup, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.group, nil
}

func (m *mockClient) ListInstances(_ context.Context, req *ig.ListInstanceGroupInstancesRequest) (*ig.ListInstanceGroupInstancesResponse, error) {
	if m.listInstancesErr != nil {
		return nil, m.listInstancesErr
	}
	if m.paginatedResponses != nil {
		resp, ok := m.paginatedResponses[req.GetPageToken()]
		if !ok {
			return &ig.ListInstanceGroupInstancesResponse{}, nil
		}
		return resp, nil
	}
	return &ig.ListInstanceGroupInstancesResponse{
		Instances: m.instances,
	}, nil
}

func (m *mockClient) Update(_ context.Context, req *ig.UpdateInstanceGroupRequest) (*operation.Operation, error) {
	m.lastUpdateReq = req
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &operation.Operation{}, nil
}

func (m *mockClient) DeleteInstances(_ context.Context, req *ig.DeleteInstancesRequest) (*operation.Operation, error) {
	m.lastDeleteInstancesReq = req
	if m.deleteInstancesErr != nil {
		return nil, m.deleteInstancesErr
	}
	return &operation.Operation{}, nil
}

func (m *mockClient) CreateFromYaml(_ context.Context, req *ig.CreateInstanceGroupFromYamlRequest) (*operation.Operation, error) {
	m.lastCreateFromYamlReq = req
	if m.createFromYamlErr != nil {
		return nil, m.createFromYamlErr
	}
	if m.createFromYamlOp != nil {
		return m.createFromYamlOp, nil
	}
	return &operation.Operation{}, nil
}

func (m *mockClient) Delete(_ context.Context, req *ig.DeleteInstanceGroupRequest) (*operation.Operation, error) {
	m.lastDeleteReq = req
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	return &operation.Operation{}, nil
}

func (m *mockClient) List(_ context.Context, req *ig.ListInstanceGroupsRequest) (*ig.ListInstanceGroupsResponse, error) {
	m.lastListReq = req
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.listGroupsResponse != nil {
		return m.listGroupsResponse, nil
	}
	return &ig.ListInstanceGroupsResponse{}, nil
}

func newTestGroup(targetSize int64) *ig.InstanceGroup {
	return &ig.InstanceGroup{
		Id:       "test-group-id",
		Name:     "test-group",
		FolderId: "test-folder",
		ManagedInstancesState: &ig.ManagedInstancesState{
			TargetSize: targetSize,
		},
		ScalePolicy: &ig.ScalePolicy{
			ScaleType: &ig.ScalePolicy_FixedScale_{
				FixedScale: &ig.ScalePolicy_FixedScale{
					Size: targetSize,
				},
			},
		},
	}
}

func newTestInstance(id string, status ig.ManagedInstance_Status, internalIP, externalIP string) *ig.ManagedInstance {
	ni := &ig.NetworkInterface{
		PrimaryV4Address: &ig.PrimaryAddress{
			Address: internalIP,
		},
	}
	if externalIP != "" {
		ni.PrimaryV4Address.OneToOneNat = &ig.OneToOneNat{
			Address: externalIP,
		}
	}
	return &ig.ManagedInstance{
		Id:                id,
		Status:            status,
		NetworkInterfaces: []*ig.NetworkInterface{ni},
	}
}

func defaultConfig() Config {
	return Config{
		FolderID:        "test-folder",
		InstanceGroupID: "test-group-id",
		SSHUser:         "ubuntu",
	}
}

func TestInit_ValidGroup(t *testing.T) {
	mock := &mockClient{
		group: newTestGroup(2),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
	}

	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if info.ID != "yandexcloud" {
		t.Errorf("expected ID 'yandexcloud', got %q", info.ID)
	}
	if info.MaxSize != math.MaxInt32 {
		t.Errorf("expected MaxSize math.MaxInt32, got %d", info.MaxSize)
	}
}

func TestInit_InvalidGroup(t *testing.T) {
	mock := &mockClient{
		getErr: fmt.Errorf("group not found"),
	}
	cfg := defaultConfig()
	cfg.InstanceGroupID = "nonexistent-group"
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error for invalid group, got nil")
	}
}

func TestInit_AutoScaleGroupRejected(t *testing.T) {
	group := newTestGroup(2)
	group.ScalePolicy = &ig.ScalePolicy{
		ScaleType: &ig.ScalePolicy_AutoScale_{
			AutoScale: &ig.ScalePolicy_AutoScale{
				MinZoneSize: 1,
				MaxSize:     10,
			},
		},
	}
	mock := &mockClient{group: group}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error for auto-scale group, got nil")
	}
}

func TestInit_MissingInstanceGroupIDAndTemplateFile(t *testing.T) {
	g := &InstanceGroup{
		Config: Config{FolderID: "test-folder"},
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when neither instance_group_id nor template_file is set, got nil")
	}
}

func TestInit_MissingFolderID(t *testing.T) {
	g := &InstanceGroup{
		Config: Config{InstanceGroupID: "test-group"},
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error for missing folder_id, got nil")
	}
}

func TestUpdate_MapsStates(t *testing.T) {
	tests := []struct {
		status ig.ManagedInstance_Status
		want   provider.State
	}{
		{ig.ManagedInstance_RUNNING_ACTUAL, provider.StateRunning},
		{ig.ManagedInstance_RUNNING_OUTDATED, provider.StateRunning},
		{ig.ManagedInstance_CREATING_INSTANCE, provider.StateCreating},
		{ig.ManagedInstance_STARTING_INSTANCE, provider.StateCreating},
		{ig.ManagedInstance_OPENING_TRAFFIC, provider.StateCreating},
		{ig.ManagedInstance_AWAITING_WARMUP_DURATION, provider.StateCreating},
		{ig.ManagedInstance_AWAITING_STARTUP_DURATION, provider.StateCreating},
		{ig.ManagedInstance_CHECKING_HEALTH, provider.StateCreating},
		{ig.ManagedInstance_UPDATING_INSTANCE, provider.StateCreating},
		{ig.ManagedInstance_PREPARING_RESOURCES, provider.StateCreating},
		{ig.ManagedInstance_STOPPING_INSTANCE, provider.StateDeleting},
		{ig.ManagedInstance_STOPPED, provider.StateDeleting},
		{ig.ManagedInstance_DELETING_INSTANCE, provider.StateDeleting},
		{ig.ManagedInstance_CLOSING_TRAFFIC, provider.StateDeleting},
		{ig.ManagedInstance_DELETED, provider.StateDeleted},
		{ig.ManagedInstance_STATUS_UNSPECIFIED, provider.StateCreating},
	}

	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			mock := &mockClient{
				instances: []*ig.ManagedInstance{
					newTestInstance("inst-1", tt.status, "10.0.0.1", "1.2.3.4"),
				},
			}
			g := &InstanceGroup{
				Config: Config{FolderID: "f", InstanceGroupID: "test-group"},
				client: mock,
				log:    hclog.NewNullLogger(),
			}

			var gotState provider.State
			err := g.Update(context.Background(), func(instance string, state provider.State) {
				gotState = state
			})
			if err != nil {
				t.Fatalf("Update failed: %v", err)
			}
			if gotState != tt.want {
				t.Errorf("status %v: got state %q, want %q", tt.status, gotState, tt.want)
			}
		})
	}
}

func TestUpdate_ListInstancesError(t *testing.T) {
	mock := &mockClient{
		listInstancesErr: fmt.Errorf("api error"),
	}
	g := &InstanceGroup{
		Config: Config{FolderID: "f", InstanceGroupID: "test-group"},
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	err := g.Update(context.Background(), func(string, provider.State) {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestUpdate_Pagination(t *testing.T) {
	mock := &mockClient{
		paginatedResponses: map[string]*ig.ListInstanceGroupInstancesResponse{
			"": {
				Instances:     []*ig.ManagedInstance{newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "1.2.3.4")},
				NextPageToken: "page2",
			},
			"page2": {
				Instances: []*ig.ManagedInstance{newTestInstance("inst-2", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.2", "1.2.3.5")},
			},
		},
	}
	g := &InstanceGroup{
		Config: Config{FolderID: "f", InstanceGroupID: "test-group"},
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	var instances []string
	err := g.Update(context.Background(), func(instance string, state provider.State) {
		instances = append(instances, instance)
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if len(instances) != 2 {
		t.Errorf("expected 2 instances across pages, got %d", len(instances))
	}
}

func TestIncrease_AddsInstances(t *testing.T) {
	mock := &mockClient{
		group: newTestGroup(3),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	n, err := g.Increase(context.Background(), 5)
	if err != nil {
		t.Fatalf("Increase failed: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 instances added, got %d", n)
	}
	if mock.lastUpdateReq == nil {
		t.Fatal("expected Update to be called")
	}
	fs, ok := mock.lastUpdateReq.ScalePolicy.ScaleType.(*ig.ScalePolicy_FixedScale_)
	if !ok {
		t.Fatal("expected FixedScale scale policy")
	}
	if fs.FixedScale.Size != 8 {
		t.Errorf("expected new size 8, got %d", fs.FixedScale.Size)
	}
}

func TestIncrease_ZeroDelta(t *testing.T) {
	mock := &mockClient{
		group: newTestGroup(5),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	n, err := g.Increase(context.Background(), 0)
	if err != nil {
		t.Fatalf("Increase failed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 instances added, got %d", n)
	}
	if mock.lastUpdateReq != nil {
		t.Error("expected Update NOT to be called for zero delta")
	}
}

func TestIncrease_GetError(t *testing.T) {
	mock := &mockClient{
		getErr: fmt.Errorf("api error"),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	n, err := g.Increase(context.Background(), 3)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 on error, got %d", n)
	}
}

func TestIncrease_UpdateError(t *testing.T) {
	mock := &mockClient{
		group:     newTestGroup(2),
		updateErr: fmt.Errorf("update failed"),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	n, err := g.Increase(context.Background(), 3)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 on error, got %d", n)
	}
}

func TestDecrease_RemovesSpecificInstances(t *testing.T) {
	mock := &mockClient{}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	ids := []string{"inst-1", "inst-2"}
	removed, err := g.Decrease(context.Background(), ids)
	if err != nil {
		t.Fatalf("Decrease failed: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("expected 2 removed, got %d", len(removed))
	}
	if mock.lastDeleteInstancesReq == nil {
		t.Fatal("expected DeleteInstances to be called")
	}
	if len(mock.lastDeleteInstancesReq.ManagedInstanceIds) != 2 {
		t.Errorf("expected 2 IDs in request, got %d", len(mock.lastDeleteInstancesReq.ManagedInstanceIds))
	}
}

func TestDecrease_EmptySlice(t *testing.T) {
	mock := &mockClient{}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	removed, err := g.Decrease(context.Background(), nil)
	if err != nil {
		t.Fatalf("Decrease failed: %v", err)
	}
	if removed != nil {
		t.Errorf("expected nil removed, got %v", removed)
	}
	if mock.lastDeleteInstancesReq != nil {
		t.Error("expected DeleteInstances NOT to be called for empty input")
	}
}

func TestDecrease_Error(t *testing.T) {
	mock := &mockClient{
		deleteInstancesErr: fmt.Errorf("delete failed"),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	_, err := g.Decrease(context.Background(), []string{"inst-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestConnectInfo_ExternalIP(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{
			newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "203.0.113.1"),
		},
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	info, err := g.ConnectInfo(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.ExternalAddr != "203.0.113.1:22" {
		t.Errorf("expected external addr '203.0.113.1:22', got %q", info.ExternalAddr)
	}
	if info.InternalAddr != "10.0.0.1:22" {
		t.Errorf("expected internal addr '10.0.0.1:22', got %q", info.InternalAddr)
	}
	if info.Username != "ubuntu" {
		t.Errorf("expected username 'ubuntu', got %q", info.Username)
	}
	if info.Protocol != provider.ProtocolSSH {
		t.Errorf("expected protocol SSH, got %q", info.Protocol)
	}
}

func TestConnectInfo_InternalOnly(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{
			newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.5", ""),
		},
	}
	cfg := defaultConfig()
	cfg.SSHUser = "admin"
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	info, err := g.ConnectInfo(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.InternalAddr != "10.0.0.5:22" {
		t.Errorf("expected internal addr '10.0.0.5:22', got %q", info.InternalAddr)
	}
	if info.ExternalAddr != "" {
		t.Errorf("expected empty external addr, got %q", info.ExternalAddr)
	}
	if info.Username != "admin" {
		t.Errorf("expected username 'admin', got %q", info.Username)
	}
}

func TestConnectInfo_ForcesSSHProtocol(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{
			newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "203.0.113.1"),
		},
	}
	g := &InstanceGroup{
		Config:   defaultConfig(),
		client:   mock,
		log:      hclog.NewNullLogger(),
		settings: provider.Settings{ConnectorConfig: provider.ConnectorConfig{Protocol: "winrm"}},
	}

	info, err := g.ConnectInfo(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.Protocol != provider.ProtocolSSH {
		t.Errorf("expected protocol SSH regardless of connector_config, got %q", info.Protocol)
	}
}

func TestConnectInfo_NotFound(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{},
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	_, err := g.ConnectInfo(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance, got nil")
	}
}

func TestConnectInfo_ListInstancesError(t *testing.T) {
	mock := &mockClient{
		listInstancesErr: fmt.Errorf("api error"),
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	_, err := g.ConnectInfo(context.Background(), "inst-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestConnectInfo_Pagination(t *testing.T) {
	mock := &mockClient{
		paginatedResponses: map[string]*ig.ListInstanceGroupInstancesResponse{
			"": {
				Instances:     []*ig.ManagedInstance{newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "1.2.3.4")},
				NextPageToken: "page2",
			},
			"page2": {
				Instances: []*ig.ManagedInstance{newTestInstance("inst-2", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.2", "1.2.3.5")},
			},
		},
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
	}

	// Should find inst-2 on the second page
	info, err := g.ConnectInfo(context.Background(), "inst-2")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.ExternalAddr != "1.2.3.5:22" {
		t.Errorf("expected external addr '1.2.3.5:22', got %q", info.ExternalAddr)
	}
}

func TestConfig_Defaults(t *testing.T) {
	c := Config{FolderID: "f", InstanceGroupID: "test"}
	if err := c.validate(); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if c.SSHUser != "ubuntu" {
		t.Errorf("expected default SSHUser 'ubuntu', got %q", c.SSHUser)
	}
}

func TestConfig_MissingBothInstanceGroupIDAndTemplateFile(t *testing.T) {
	c := Config{FolderID: "f"}
	if err := c.validate(); err == nil {
		t.Fatal("expected validation error when neither instance_group_id nor template_file is set")
	}
}

func TestConfig_MutuallyExclusive(t *testing.T) {
	c := Config{FolderID: "f", InstanceGroupID: "test", TemplateFile: "template.yaml"}
	if err := c.validate(); err == nil {
		t.Fatal("expected validation error when both instance_group_id and template_file are set")
	}
}

func TestConfig_TemplateFileMode(t *testing.T) {
	c := Config{FolderID: "f", TemplateFile: "template.yaml"}
	if err := c.validate(); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if c.GroupName != defaultGroupName {
		t.Errorf("expected default GroupName %q, got %q", defaultGroupName, c.GroupName)
	}
	if c.SSHUser != "ubuntu" {
		t.Errorf("expected default SSHUser 'ubuntu', got %q", c.SSHUser)
	}
}

func TestConfig_CustomGroupName(t *testing.T) {
	c := Config{FolderID: "f", TemplateFile: "template.yaml", GroupName: "my-group"}
	if err := c.validate(); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if c.GroupName != "my-group" {
		t.Errorf("expected GroupName 'my-group', got %q", c.GroupName)
	}
}

func TestConfig_MissingFolderID(t *testing.T) {
	c := Config{InstanceGroupID: "test"}
	if err := c.validate(); err == nil {
		t.Fatal("expected validation error for missing folder_id")
	}
}

func TestHeartbeat_NoOp(t *testing.T) {
	g := &InstanceGroup{}
	if err := g.Heartbeat(context.Background(), "inst-1"); err != nil {
		t.Errorf("Heartbeat should be no-op, got error: %v", err)
	}
}

func TestShutdown(t *testing.T) {
	g := &InstanceGroup{
		log: hclog.NewNullLogger(),
	}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

// Helper to create an operation with CreateInstanceGroupMetadata.
func newCreateOp(groupID string) *operation.Operation {
	meta := &ig.CreateInstanceGroupMetadata{InstanceGroupId: groupID}
	anyMeta, err := anypb.New(meta)
	if err != nil {
		panic(err)
	}
	return &operation.Operation{Metadata: anyMeta}
}

// Helper to write a temp YAML template file.
func writeTempTemplate(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

const testYAMLTemplate = `name: test-group
service_account_id: ajeXXX
instance_template:
  platform_id: standard-v3
scale_policy:
  fixed_scale:
    size: 0
`

func TestInit_TemplateFile_CreateNewGroup(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)
	createdGroupID := "created-group-id"

	mock := &mockClient{
		group:              newTestGroup(0),
		listGroupsResponse: &ig.ListInstanceGroupsResponse{},
		createFromYamlOp:   newCreateOp(createdGroupID),
	}
	// Override Get to return the group with the created ID.
	mock.group.Id = createdGroupID
	mock.group.FolderId = "test-folder"

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if info.ID != "yandexcloud" {
		t.Errorf("expected ID 'yandexcloud', got %q", info.ID)
	}
	if g.InstanceGroupID != createdGroupID {
		t.Errorf("expected InstanceGroupID %q, got %q", createdGroupID, g.InstanceGroupID)
	}
	if !g.createdGroup {
		t.Error("expected createdGroup to be true")
	}
	if mock.lastCreateFromYamlReq == nil {
		t.Fatal("expected CreateFromYaml to be called")
	}
	if mock.lastCreateFromYamlReq.FolderId != "test-folder" {
		t.Errorf("expected FolderId 'test-folder', got %q", mock.lastCreateFromYamlReq.FolderId)
	}
	// Verify that labels were injected.
	if !strings.Contains(mock.lastCreateFromYamlReq.InstanceGroupYaml, managedByLabel) {
		t.Error("expected managed-by label to be injected into YAML")
	}
}

func TestInit_TemplateFile_ReuseExistingGroup(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)
	existingGroupID := "existing-group-id"

	mock := &mockClient{
		group: newTestGroup(2),
		listGroupsResponse: &ig.ListInstanceGroupsResponse{
			InstanceGroups: []*ig.InstanceGroup{
				{
					Id:       existingGroupID,
					Name:     defaultGroupName,
					FolderId: "test-folder",
					Labels:   map[string]string{managedByLabel: managedByValue},
					Status:   ig.InstanceGroup_ACTIVE,
					ScalePolicy: &ig.ScalePolicy{
						ScaleType: &ig.ScalePolicy_FixedScale_{
							FixedScale: &ig.ScalePolicy_FixedScale{Size: 2},
						},
					},
				},
			},
		},
	}
	mock.group.Id = existingGroupID
	mock.group.FolderId = "test-folder"

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if g.InstanceGroupID != existingGroupID {
		t.Errorf("expected InstanceGroupID %q, got %q", existingGroupID, g.InstanceGroupID)
	}
	if !g.createdGroup {
		t.Error("expected createdGroup to be true when reusing managed group")
	}
	if mock.lastCreateFromYamlReq != nil {
		t.Error("expected CreateFromYaml NOT to be called when reusing existing group")
	}
}

func TestInit_TemplateFile_MultipleMatchingGroupsError(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)

	mock := &mockClient{
		listGroupsResponse: &ig.ListInstanceGroupsResponse{
			InstanceGroups: []*ig.InstanceGroup{
				{Id: "g1", Name: defaultGroupName, Labels: map[string]string{managedByLabel: managedByValue}, Status: ig.InstanceGroup_ACTIVE},
				{Id: "g2", Name: defaultGroupName, Labels: map[string]string{managedByLabel: managedByValue}, Status: ig.InstanceGroup_ACTIVE},
			},
		},
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when multiple matching groups found, got nil")
	}
	if !strings.Contains(err.Error(), "found 2 instance groups") {
		t.Errorf("expected error about multiple groups, got: %v", err)
	}
}

func TestInit_TemplateFile_CreateFromYamlError(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)

	mock := &mockClient{
		listGroupsResponse: &ig.ListInstanceGroupsResponse{},
		createFromYamlErr:  fmt.Errorf("API quota exceeded"),
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when CreateFromYaml fails, got nil")
	}
	if !strings.Contains(err.Error(), "API quota exceeded") {
		t.Errorf("expected API error in message, got: %v", err)
	}
}

func TestInit_TemplateFile_FileNotFound(t *testing.T) {
	mock := &mockClient{
		listGroupsResponse: &ig.ListInstanceGroupsResponse{},
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: "/nonexistent/template.yaml",
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when template file not found, got nil")
	}
}

func TestInit_TemplateFile_ListError(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)

	mock := &mockClient{
		listErr: fmt.Errorf("permission denied"),
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when List fails, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected permission denied error, got: %v", err)
	}
}

func TestShutdown_DeleteOnShutdown(t *testing.T) {
	mock := &mockClient{}

	g := &InstanceGroup{
		Config: Config{
			FolderID:         "test-folder",
			TemplateFile:     "template.yaml",
			DeleteOnShutdown: true,
			InstanceGroupID:  "group-to-delete",
		},
		client:       mock,
		log:          hclog.NewNullLogger(),
		createdGroup: true,
		waitOp:       func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	err := g.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if mock.lastDeleteReq == nil {
		t.Fatal("expected Delete to be called")
	}
	if mock.lastDeleteReq.InstanceGroupId != "group-to-delete" {
		t.Errorf("expected group ID 'group-to-delete', got %q", mock.lastDeleteReq.InstanceGroupId)
	}
}

func TestShutdown_NoDeleteForExternalGroup(t *testing.T) {
	mock := &mockClient{}

	// createdGroup=false means the plugin did not create the group, so it should not delete it.
	// Note: delete_on_shutdown with instance_group_id is rejected by config validation,
	// but Shutdown still guards against deletion via the createdGroup flag.
	g := &InstanceGroup{
		Config: Config{
			FolderID:        "test-folder",
			InstanceGroupID: "external-group",
		},
		client:       mock,
		log:          hclog.NewNullLogger(),
		createdGroup: false,
	}

	err := g.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if mock.lastDeleteReq != nil {
		t.Error("expected Delete NOT to be called for external group")
	}
}

func TestShutdown_NoDeleteWhenFlagNotSet(t *testing.T) {
	mock := &mockClient{}

	g := &InstanceGroup{
		Config: Config{
			FolderID:         "test-folder",
			TemplateFile:     "template.yaml",
			DeleteOnShutdown: false,
			InstanceGroupID:  "created-group",
		},
		client:       mock,
		log:          hclog.NewNullLogger(),
		createdGroup: true,
	}

	err := g.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if mock.lastDeleteReq != nil {
		t.Error("expected Delete NOT to be called when DeleteOnShutdown is false")
	}
}

func TestShutdown_DeleteError(t *testing.T) {
	mock := &mockClient{
		deleteErr: fmt.Errorf("delete failed"),
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:         "test-folder",
			TemplateFile:     "template.yaml",
			DeleteOnShutdown: true,
			InstanceGroupID:  "group-to-delete",
		},
		client:       mock,
		log:          hclog.NewNullLogger(),
		createdGroup: true,
		waitOp:       func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	err := g.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected error when Delete fails, got nil")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Errorf("expected delete error, got: %v", err)
	}
}

func TestInjectLabels_EmptyLabels(t *testing.T) {
	input := `name: test
instance_template:
  platform_id: standard-v3
`
	result, err := injectLabels(input, map[string]string{"key1": "val1"})
	if err != nil {
		t.Fatalf("injectLabels failed: %v", err)
	}
	if !strings.Contains(result, "key1") || !strings.Contains(result, "val1") {
		t.Errorf("expected label key1=val1 in result, got:\n%s", result)
	}
}

func TestInjectLabels_MergeWithExisting(t *testing.T) {
	input := `name: test
labels:
  existing: value
instance_template:
  platform_id: standard-v3
`
	result, err := injectLabels(input, map[string]string{"new-key": "new-val"})
	if err != nil {
		t.Fatalf("injectLabels failed: %v", err)
	}
	if !strings.Contains(result, "existing") || !strings.Contains(result, "value") {
		t.Errorf("expected existing label preserved, got:\n%s", result)
	}
	if !strings.Contains(result, "new-key") || !strings.Contains(result, "new-val") {
		t.Errorf("expected new label added, got:\n%s", result)
	}
}

func TestInjectLabels_OverwriteExisting(t *testing.T) {
	input := `name: test
labels:
  key: old-value
`
	result, err := injectLabels(input, map[string]string{"key": "new-value"})
	if err != nil {
		t.Fatalf("injectLabels failed: %v", err)
	}
	if !strings.Contains(result, "new-value") {
		t.Errorf("expected label to be overwritten with new-value, got:\n%s", result)
	}
	if strings.Contains(result, "old-value") {
		t.Errorf("expected old-value to be replaced, got:\n%s", result)
	}
}

func TestInjectLabels_EmptyDocument(t *testing.T) {
	result, err := injectLabels("", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("injectLabels failed on empty input: %v", err)
	}
	if !strings.Contains(result, "key") || !strings.Contains(result, "val") {
		t.Errorf("expected label key=val in result, got:\n%s", result)
	}
}

func TestInjectLabels_InvalidYAML(t *testing.T) {
	_, err := injectLabels("{{invalid yaml", map[string]string{"key": "val"})
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestOverrideName(t *testing.T) {
	input := `name: original-name
instance_template:
  platform_id: standard-v3
`
	result, err := overrideName(input, "new-name")
	if err != nil {
		t.Fatalf("overrideName failed: %v", err)
	}
	if !strings.Contains(result, "new-name") {
		t.Errorf("expected name to be overridden to 'new-name', got:\n%s", result)
	}
	if strings.Contains(result, "original-name") {
		t.Errorf("expected original name to be replaced, got:\n%s", result)
	}
}

func TestOverrideName_EmptyDocument(t *testing.T) {
	result, err := overrideName("", "my-group")
	if err != nil {
		t.Fatalf("overrideName failed on empty input: %v", err)
	}
	if !strings.Contains(result, "my-group") {
		t.Errorf("expected name 'my-group' in result, got:\n%s", result)
	}
}

func TestConfig_DeleteOnShutdownRequiresTemplateFile(t *testing.T) {
	c := Config{FolderID: "f", InstanceGroupID: "test", DeleteOnShutdown: true}
	if err := c.validate(); err == nil {
		t.Fatal("expected validation error when delete_on_shutdown is set without template_file")
	}
}

func TestInit_TemplateFile_SkipsDeletingGroup(t *testing.T) {
	templatePath := writeTempTemplate(t, testYAMLTemplate)
	createdGroupID := "created-group-id"

	mock := &mockClient{
		group: newTestGroup(0),
		// The existing group is in DELETING status — should be skipped.
		listGroupsResponse: &ig.ListInstanceGroupsResponse{
			InstanceGroups: []*ig.InstanceGroup{
				{
					Id:       "deleting-group-id",
					Name:     defaultGroupName,
					FolderId: "test-folder",
					Labels:   map[string]string{managedByLabel: managedByValue},
					Status:   ig.InstanceGroup_DELETING,
				},
			},
		},
		createFromYamlOp: newCreateOp(createdGroupID),
	}
	mock.group.Id = createdGroupID
	mock.group.FolderId = "test-folder"

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	// Should have created a new group since the existing one is DELETING.
	if g.InstanceGroupID != createdGroupID {
		t.Errorf("expected new group %q, got %q", createdGroupID, g.InstanceGroupID)
	}
	if mock.lastCreateFromYamlReq == nil {
		t.Error("expected CreateFromYaml to be called since existing group is DELETING")
	}
}

func TestInit_TemplateFile_ErrorsOnInactiveGroups(t *testing.T) {
	for _, status := range []ig.InstanceGroup_Status{
		ig.InstanceGroup_STOPPED,
		ig.InstanceGroup_PAUSED,
		ig.InstanceGroup_STOPPING,
	} {
		t.Run(status.String(), func(t *testing.T) {
			templatePath := writeTempTemplate(t, testYAMLTemplate)

			mock := &mockClient{
				listGroupsResponse: &ig.ListInstanceGroupsResponse{
					InstanceGroups: []*ig.InstanceGroup{
						{
							Id:       "inactive-group-id",
							Name:     defaultGroupName,
							FolderId: "test-folder",
							Labels:   map[string]string{managedByLabel: managedByValue},
							Status:   status,
						},
					},
				},
			}

			g := &InstanceGroup{
				Config: Config{
					FolderID:     "test-folder",
					TemplateFile: templatePath,
				},
				client: mock,
				waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
			}

			_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
			if err == nil {
				t.Fatalf("expected error for %s group, got nil", status)
			}
			if !strings.Contains(err.Error(), status.String()) {
				t.Errorf("expected error to mention %s, got: %v", status, err)
			}
			if mock.lastCreateFromYamlReq != nil {
				t.Error("expected CreateFromYaml NOT to be called for inactive group")
			}
		})
	}
}

func TestInit_TemplateFile_RollbackOnValidationFailure(t *testing.T) {
	templatePath := writeTempTemplate(t, `name: test-group
service_account_id: ajeXXX
instance_template:
  platform_id: standard-v3
scale_policy:
  auto_scale:
    min_zone_size: 1
    max_size: 10
`)
	createdGroupID := "created-group-id"

	// The group has auto_scale, which will fail fixed-scale validation.
	autoScaleGroup := &ig.InstanceGroup{
		Id:       createdGroupID,
		FolderId: "test-folder",
		ScalePolicy: &ig.ScalePolicy{
			ScaleType: &ig.ScalePolicy_AutoScale_{
				AutoScale: &ig.ScalePolicy_AutoScale{
					MinZoneSize: 1,
					MaxSize:     10,
				},
			},
		},
	}

	mock := &mockClient{
		group:              autoScaleGroup,
		listGroupsResponse: &ig.ListInstanceGroupsResponse{},
		createFromYamlOp:   newCreateOp(createdGroupID),
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error for auto-scale group, got nil")
	}
	if !strings.Contains(err.Error(), "fixed scale policy") {
		t.Errorf("expected fixed scale error, got: %v", err)
	}
	// Verify that the plugin attempted to roll back the created group.
	if mock.lastDeleteReq == nil {
		t.Fatal("expected Delete to be called for rollback")
	}
	if mock.lastDeleteReq.InstanceGroupId != createdGroupID {
		t.Errorf("expected rollback delete for %q, got %q", createdGroupID, mock.lastDeleteReq.InstanceGroupId)
	}
}

func TestInit_TemplateFile_RollbackFailureJoinsErrors(t *testing.T) {
	templatePath := writeTempTemplate(t, `name: test-group
service_account_id: ajeXXX
instance_template:
  platform_id: standard-v3
scale_policy:
  auto_scale:
    min_zone_size: 1
    max_size: 10
`)
	createdGroupID := "created-group-id"

	autoScaleGroup := &ig.InstanceGroup{
		Id:       createdGroupID,
		FolderId: "test-folder",
		ScalePolicy: &ig.ScalePolicy{
			ScaleType: &ig.ScalePolicy_AutoScale_{
				AutoScale: &ig.ScalePolicy_AutoScale{
					MinZoneSize: 1,
					MaxSize:     10,
				},
			},
		},
	}

	mock := &mockClient{
		group:              autoScaleGroup,
		listGroupsResponse: &ig.ListInstanceGroupsResponse{},
		createFromYamlOp:   newCreateOp(createdGroupID),
		deleteErr:          fmt.Errorf("rollback delete failed"),
	}

	g := &InstanceGroup{
		Config: Config{
			FolderID:     "test-folder",
			TemplateFile: templatePath,
		},
		client: mock,
		waitOp: func(_ context.Context, _ *operation.Operation) error { return nil },
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Error should contain both the validation error and the rollback error.
	if !strings.Contains(err.Error(), "fixed scale policy") {
		t.Errorf("expected validation error in message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rollback delete failed") {
		t.Errorf("expected rollback error in message, got: %v", err)
	}
}

// newTestGroupWithTemplate creates a test instance group with an InstanceTemplate
// containing the given metadata. Useful for SSH key injection tests.
func newTestGroupWithTemplate(targetSize int64, metadata map[string]string) *ig.InstanceGroup {
	group := newTestGroup(targetSize)
	group.InstanceTemplate = &ig.InstanceTemplate{
		Metadata: metadata,
	}
	return group
}

func TestGenerateED25519Key(t *testing.T) {
	privPEM, authorizedKey, err := generateED25519Key()
	if err != nil {
		t.Fatalf("generateED25519Key failed: %v", err)
	}

	// Verify PEM is parseable.
	block, _ := pem.Decode(privPEM)
	if block == nil {
		t.Fatal("failed to decode PEM block from private key")
	}
	if block.Type != "PRIVATE KEY" {
		t.Errorf("expected PEM type 'PRIVATE KEY', got %q", block.Type)
	}

	// Verify the private key is a valid PKCS8 key.
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse PKCS8 private key: %v", err)
	}
	if _, ok := key.(ed25519.PrivateKey); !ok {
		t.Errorf("expected ed25519.PrivateKey, got %T", key)
	}

	// Verify public key format.
	if !strings.HasPrefix(authorizedKey, "ssh-ed25519 ") {
		t.Errorf("expected authorized key to start with 'ssh-ed25519 ', got %q", authorizedKey)
	}

	// Verify the public key corresponds to the private key.
	privKey := key.(ed25519.PrivateKey)
	expectedPub := privKey.Public().(ed25519.PublicKey)
	sshExpectedPub, err := ssh.NewPublicKey(expectedPub)
	if err != nil {
		t.Fatalf("failed to create SSH public key from private key: %v", err)
	}
	expectedAuthorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshExpectedPub)))
	if authorizedKey != expectedAuthorizedKey {
		t.Errorf("public key does not correspond to private key\ngot:  %q\nwant: %q", authorizedKey, expectedAuthorizedKey)
	}
}

func TestInit_GenerateSSHKey_InjectsMetadata(t *testing.T) {
	group := newTestGroupWithTemplate(2, map[string]string{
		"existing-key": "existing-value",
	})
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Verify Update was called with ssh-keys in metadata.
	if mock.lastUpdateReq == nil {
		t.Fatal("expected Update to be called for SSH key injection")
	}
	// Verify the Update targets the correct instance group.
	if mock.lastUpdateReq.GetInstanceGroupId() != "test-group-id" {
		t.Errorf("expected Update for group 'test-group-id', got %q", mock.lastUpdateReq.GetInstanceGroupId())
	}
	// Verify the field mask is set correctly.
	paths := mock.lastUpdateReq.GetUpdateMask().GetPaths()
	if len(paths) != 1 || paths[0] != "instance_template.metadata" {
		t.Errorf("expected field mask [instance_template.metadata], got %v", paths)
	}
	meta := mock.lastUpdateReq.GetInstanceTemplate().GetMetadata()
	if meta == nil {
		t.Fatal("expected metadata in update request")
	}
	sshKeys, ok := meta["ssh-keys"]
	if !ok || sshKeys == "" {
		t.Fatal("expected ssh-keys in metadata")
	}
	if !strings.HasPrefix(sshKeys, "ubuntu:ssh-ed25519 ") {
		t.Errorf("expected ssh-keys to start with 'ubuntu:ssh-ed25519 ', got %q", sshKeys)
	}
	// Verify existing metadata is preserved.
	if meta["existing-key"] != "existing-value" {
		t.Errorf("expected existing metadata preserved, got %q", meta["existing-key"])
	}
}

func TestInit_GenerateSSHKey_AppendsToExistingKeys(t *testing.T) {
	existingSSHKeys := "otheruser:ssh-rsa AAAA..."
	group := newTestGroupWithTemplate(2, map[string]string{
		"ssh-keys": existingSSHKeys,
	})
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if mock.lastUpdateReq == nil {
		t.Fatal("expected Update to be called")
	}
	sshKeys := mock.lastUpdateReq.GetInstanceTemplate().GetMetadata()["ssh-keys"]

	// Existing keys should be preserved at the beginning.
	if !strings.HasPrefix(sshKeys, existingSSHKeys+"\n") {
		t.Errorf("expected existing ssh-keys to be preserved, got %q", sshKeys)
	}
	// New key should be appended.
	if !strings.Contains(sshKeys, "ubuntu:ssh-ed25519 ") {
		t.Errorf("expected new key to be appended, got %q", sshKeys)
	}
}

func TestInit_GenerateSSHKey_UpdateError(t *testing.T) {
	group := newTestGroupWithTemplate(2, nil)
	mock := &mockClient{
		group:     group,
		updateErr: fmt.Errorf("metadata update failed"),
	}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error when Update fails, got nil")
	}
	if !strings.Contains(err.Error(), "metadata update failed") {
		t.Errorf("expected metadata update error, got: %v", err)
	}
}

func TestInit_GenerateSSHKey_Disabled(t *testing.T) {
	group := newTestGroupWithTemplate(2, nil)
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = false
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if mock.lastUpdateReq != nil {
		t.Error("expected Update NOT to be called when GenerateSSHKey is false")
	}
	if g.sshPrivateKey != nil {
		t.Error("expected sshPrivateKey to be nil when GenerateSSHKey is false")
	}
}

func TestInit_GenerateSSHKey_NilTemplate(t *testing.T) {
	// Instance group with no InstanceTemplate set (nil metadata).
	group := newTestGroup(2)
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if mock.lastUpdateReq == nil {
		t.Fatal("expected Update to be called for SSH key injection")
	}
	meta := mock.lastUpdateReq.GetInstanceTemplate().GetMetadata()
	sshKeys, ok := meta["ssh-keys"]
	if !ok || sshKeys == "" {
		t.Fatal("expected ssh-keys in metadata")
	}
	if !strings.HasPrefix(sshKeys, "ubuntu:ssh-ed25519 ") {
		t.Errorf("expected ssh-keys to start with 'ubuntu:ssh-ed25519 ', got %q", sshKeys)
	}
}

func TestInit_GenerateSSHKey_CustomSSHUser(t *testing.T) {
	group := newTestGroupWithTemplate(2, nil)
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	cfg.SSHUser = "deploy"
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if mock.lastUpdateReq == nil {
		t.Fatal("expected Update to be called")
	}
	sshKeys := mock.lastUpdateReq.GetInstanceTemplate().GetMetadata()["ssh-keys"]
	if !strings.HasPrefix(sshKeys, "deploy:ssh-ed25519 ") {
		t.Errorf("expected ssh-keys to start with 'deploy:ssh-ed25519 ', got %q", sshKeys)
	}
}

func TestInit_GenerateSSHKey_ReplacesExistingUserKey(t *testing.T) {
	// Simulate a previous run that already injected a key for "ubuntu".
	group := newTestGroupWithTemplate(2, map[string]string{
		"ssh-keys": "ubuntu:ssh-ed25519 OLD_KEY\notheruser:ssh-rsa AAAA...",
	})
	mock := &mockClient{group: group}

	cfg := defaultConfig()
	cfg.GenerateSSHKey = true
	g := &InstanceGroup{
		Config: cfg,
		client: mock,
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	sshKeys := mock.lastUpdateReq.GetInstanceTemplate().GetMetadata()["ssh-keys"]
	// The old ubuntu key should be replaced, not duplicated.
	if strings.Contains(sshKeys, "OLD_KEY") {
		t.Errorf("expected old ubuntu key to be replaced, got %q", sshKeys)
	}
	// Other user's key should be preserved.
	if !strings.Contains(sshKeys, "otheruser:ssh-rsa AAAA...") {
		t.Errorf("expected otheruser key to be preserved, got %q", sshKeys)
	}
	// New ubuntu key should be present.
	if !strings.Contains(sshKeys, "ubuntu:ssh-ed25519 ") {
		t.Errorf("expected new ubuntu key, got %q", sshKeys)
	}
}

func TestConnectInfo_WithGeneratedKey(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{
			newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "1.2.3.4"),
		},
	}
	g := &InstanceGroup{
		Config:       defaultConfig(),
		client:       mock,
		log:          hclog.NewNullLogger(),
		sshPrivateKey: []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n"),
		sshPublicKey:  "ssh-ed25519 AAAA...",
	}

	info, err := g.ConnectInfo(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.ConnectorConfig.Key == nil {
		t.Fatal("expected Key to be set in ConnectInfo")
	}
	if string(info.ConnectorConfig.Key) != string(g.sshPrivateKey) {
		t.Errorf("expected Key to match sshPrivateKey")
	}
	if !info.ConnectorConfig.UseStaticCredentials {
		t.Error("expected UseStaticCredentials to be true")
	}
}

func TestShutdown_ZerosPrivateKey(t *testing.T) {
	g := &InstanceGroup{
		Config:        defaultConfig(),
		log:           hclog.NewNullLogger(),
		sshPrivateKey: []byte("sensitive-key-material"),
	}

	err := g.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	if g.sshPrivateKey != nil {
		t.Error("expected sshPrivateKey to be nil after Shutdown")
	}
}

func TestConnectInfo_WithoutGeneratedKey(t *testing.T) {
	mock := &mockClient{
		instances: []*ig.ManagedInstance{
			newTestInstance("inst-1", ig.ManagedInstance_RUNNING_ACTUAL, "10.0.0.1", "1.2.3.4"),
		},
	}
	g := &InstanceGroup{
		Config: defaultConfig(),
		client: mock,
		log:    hclog.NewNullLogger(),
		// sshPrivateKey is nil — feature not enabled.
	}

	info, err := g.ConnectInfo(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ConnectInfo failed: %v", err)
	}
	if info.ConnectorConfig.Key != nil {
		t.Errorf("expected Key to be nil when SSH key generation is off, got %v", info.ConnectorConfig.Key)
	}
	if info.ConnectorConfig.UseStaticCredentials {
		t.Error("expected UseStaticCredentials to be false when SSH key generation is off")
	}
}

func TestInjectLabels_ManagedByLabel(t *testing.T) {
	input := `name: test
`
	result, err := injectLabels(input, map[string]string{managedByLabel: managedByValue})
	if err != nil {
		t.Fatalf("injectLabels failed: %v", err)
	}
	if !strings.Contains(result, managedByLabel) || !strings.Contains(result, managedByValue) {
		t.Errorf("expected managed-by label in result, got:\n%s", result)
	}
}
