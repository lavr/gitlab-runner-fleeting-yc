package main

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/hashicorp/go-hclog"
	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// mockClient implements InstanceGroupClient for testing.
type mockClient struct {
	group     *ig.InstanceGroup
	instances []*ig.ManagedInstance

	getErr             error
	listInstancesErr   error
	updateErr          error
	deleteInstancesErr error

	lastUpdateReq          *ig.UpdateInstanceGroupRequest
	lastDeleteInstancesReq *ig.DeleteInstancesRequest

	// Pagination support: maps page token to response.
	// If nil, returns all instances in one page.
	paginatedResponses map[string]*ig.ListInstanceGroupInstancesResponse
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

func TestInit_MissingInstanceGroupID(t *testing.T) {
	g := &InstanceGroup{
		Config: Config{FolderID: "test-folder"},
	}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil {
		t.Fatal("expected error for missing instance_group_id, got nil")
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

func TestConfig_MissingInstanceGroupID(t *testing.T) {
	c := Config{FolderID: "f"}
	if err := c.validate(); err == nil {
		t.Fatal("expected validation error for missing instance_group_id")
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
