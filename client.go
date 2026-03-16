package main

import (
	"context"

	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"google.golang.org/grpc"
)

// InstanceGroupClient abstracts the Yandex Cloud Instance Group API for testability.
type InstanceGroupClient interface {
	Get(ctx context.Context, req *ig.GetInstanceGroupRequest) (*ig.InstanceGroup, error)
	ListInstances(ctx context.Context, req *ig.ListInstanceGroupInstancesRequest) (*ig.ListInstanceGroupInstancesResponse, error)
	Update(ctx context.Context, req *ig.UpdateInstanceGroupRequest) (*operation.Operation, error)
	DeleteInstances(ctx context.Context, req *ig.DeleteInstancesRequest) (*operation.Operation, error)
	CreateFromYaml(ctx context.Context, req *ig.CreateInstanceGroupFromYamlRequest) (*operation.Operation, error)
	Delete(ctx context.Context, req *ig.DeleteInstanceGroupRequest) (*operation.Operation, error)
	List(ctx context.Context, req *ig.ListInstanceGroupsRequest) (*ig.ListInstanceGroupsResponse, error)
}

// sdkClient wraps the real YC SDK client, adapting the grpc.CallOption signatures.
type sdkClient struct {
	inner interface {
		Get(ctx context.Context, req *ig.GetInstanceGroupRequest, opts ...grpc.CallOption) (*ig.InstanceGroup, error)
		ListInstances(ctx context.Context, req *ig.ListInstanceGroupInstancesRequest, opts ...grpc.CallOption) (*ig.ListInstanceGroupInstancesResponse, error)
		Update(ctx context.Context, req *ig.UpdateInstanceGroupRequest, opts ...grpc.CallOption) (*operation.Operation, error)
		DeleteInstances(ctx context.Context, req *ig.DeleteInstancesRequest, opts ...grpc.CallOption) (*operation.Operation, error)
		CreateFromYaml(ctx context.Context, req *ig.CreateInstanceGroupFromYamlRequest, opts ...grpc.CallOption) (*operation.Operation, error)
		Delete(ctx context.Context, req *ig.DeleteInstanceGroupRequest, opts ...grpc.CallOption) (*operation.Operation, error)
		List(ctx context.Context, req *ig.ListInstanceGroupsRequest, opts ...grpc.CallOption) (*ig.ListInstanceGroupsResponse, error)
	}
}

func (c *sdkClient) Get(ctx context.Context, req *ig.GetInstanceGroupRequest) (*ig.InstanceGroup, error) {
	return c.inner.Get(ctx, req)
}

func (c *sdkClient) ListInstances(ctx context.Context, req *ig.ListInstanceGroupInstancesRequest) (*ig.ListInstanceGroupInstancesResponse, error) {
	return c.inner.ListInstances(ctx, req)
}

func (c *sdkClient) Update(ctx context.Context, req *ig.UpdateInstanceGroupRequest) (*operation.Operation, error) {
	return c.inner.Update(ctx, req)
}

func (c *sdkClient) DeleteInstances(ctx context.Context, req *ig.DeleteInstancesRequest) (*operation.Operation, error) {
	return c.inner.DeleteInstances(ctx, req)
}

func (c *sdkClient) CreateFromYaml(ctx context.Context, req *ig.CreateInstanceGroupFromYamlRequest) (*operation.Operation, error) {
	return c.inner.CreateFromYaml(ctx, req)
}

func (c *sdkClient) Delete(ctx context.Context, req *ig.DeleteInstanceGroupRequest) (*operation.Operation, error) {
	return c.inner.Delete(ctx, req)
}

func (c *sdkClient) List(ctx context.Context, req *ig.ListInstanceGroupsRequest) (*ig.ListInstanceGroupsResponse, error) {
	return c.inner.List(ctx, req)
}
