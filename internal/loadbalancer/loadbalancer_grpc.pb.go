// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.2.0
// - protoc             v3.21.12
// source: internal/loadbalancer/loadbalancer.proto

package loadbalancer

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// LoadBalancerClient is the client API for LoadBalancer service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type LoadBalancerClient interface {
	// Get all information about all defined Load Balancers
	GetLoadBalancers(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (LoadBalancer_GetLoadBalancersClient, error)
	// Get information about a specific Load Balancer. If no such name exists, return
	// an empty structure
	GetLoadBalancer(ctx context.Context, in *LoadBalancerName, opts ...grpc.CallOption) (*LoadBalancerInformation, error)
	Create(ctx context.Context, in *CreateLoadBalancer, opts ...grpc.CallOption) (*LoadBalancerInformation, error)
	Delete(ctx context.Context, in *LoadBalancerName, opts ...grpc.CallOption) (*Error, error)
	AddTarget(ctx context.Context, in *AddTargetRequest, opts ...grpc.CallOption) (*Error, error)
	DelTarget(ctx context.Context, in *DelTargetRequest, opts ...grpc.CallOption) (*Error, error)
}

type loadBalancerClient struct {
	cc grpc.ClientConnInterface
}

func NewLoadBalancerClient(cc grpc.ClientConnInterface) LoadBalancerClient {
	return &loadBalancerClient{cc}
}

func (c *loadBalancerClient) GetLoadBalancers(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (LoadBalancer_GetLoadBalancersClient, error) {
	stream, err := c.cc.NewStream(ctx, &LoadBalancer_ServiceDesc.Streams[0], "/loadbalancer.LoadBalancer/GetLoadBalancers", opts...)
	if err != nil {
		return nil, err
	}
	x := &loadBalancerGetLoadBalancersClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type LoadBalancer_GetLoadBalancersClient interface {
	Recv() (*LoadBalancerInformation, error)
	grpc.ClientStream
}

type loadBalancerGetLoadBalancersClient struct {
	grpc.ClientStream
}

func (x *loadBalancerGetLoadBalancersClient) Recv() (*LoadBalancerInformation, error) {
	m := new(LoadBalancerInformation)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *loadBalancerClient) GetLoadBalancer(ctx context.Context, in *LoadBalancerName, opts ...grpc.CallOption) (*LoadBalancerInformation, error) {
	out := new(LoadBalancerInformation)
	err := c.cc.Invoke(ctx, "/loadbalancer.LoadBalancer/GetLoadBalancer", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *loadBalancerClient) Create(ctx context.Context, in *CreateLoadBalancer, opts ...grpc.CallOption) (*LoadBalancerInformation, error) {
	out := new(LoadBalancerInformation)
	err := c.cc.Invoke(ctx, "/loadbalancer.LoadBalancer/Create", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *loadBalancerClient) Delete(ctx context.Context, in *LoadBalancerName, opts ...grpc.CallOption) (*Error, error) {
	out := new(Error)
	err := c.cc.Invoke(ctx, "/loadbalancer.LoadBalancer/Delete", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *loadBalancerClient) AddTarget(ctx context.Context, in *AddTargetRequest, opts ...grpc.CallOption) (*Error, error) {
	out := new(Error)
	err := c.cc.Invoke(ctx, "/loadbalancer.LoadBalancer/AddTarget", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *loadBalancerClient) DelTarget(ctx context.Context, in *DelTargetRequest, opts ...grpc.CallOption) (*Error, error) {
	out := new(Error)
	err := c.cc.Invoke(ctx, "/loadbalancer.LoadBalancer/DelTarget", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadBalancerServer is the server API for LoadBalancer service.
// All implementations must embed UnimplementedLoadBalancerServer
// for forward compatibility
type LoadBalancerServer interface {
	// Get all information about all defined Load Balancers
	GetLoadBalancers(*emptypb.Empty, LoadBalancer_GetLoadBalancersServer) error
	// Get information about a specific Load Balancer. If no such name exists, return
	// an empty structure
	GetLoadBalancer(context.Context, *LoadBalancerName) (*LoadBalancerInformation, error)
	Create(context.Context, *CreateLoadBalancer) (*LoadBalancerInformation, error)
	Delete(context.Context, *LoadBalancerName) (*Error, error)
	AddTarget(context.Context, *AddTargetRequest) (*Error, error)
	DelTarget(context.Context, *DelTargetRequest) (*Error, error)
	mustEmbedUnimplementedLoadBalancerServer()
}

// UnimplementedLoadBalancerServer must be embedded to have forward compatible implementations.
type UnimplementedLoadBalancerServer struct {
}

func (UnimplementedLoadBalancerServer) GetLoadBalancers(*emptypb.Empty, LoadBalancer_GetLoadBalancersServer) error {
	return status.Errorf(codes.Unimplemented, "method GetLoadBalancers not implemented")
}
func (UnimplementedLoadBalancerServer) GetLoadBalancer(context.Context, *LoadBalancerName) (*LoadBalancerInformation, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetLoadBalancer not implemented")
}
func (UnimplementedLoadBalancerServer) Create(context.Context, *CreateLoadBalancer) (*LoadBalancerInformation, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Create not implemented")
}
func (UnimplementedLoadBalancerServer) Delete(context.Context, *LoadBalancerName) (*Error, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Delete not implemented")
}
func (UnimplementedLoadBalancerServer) AddTarget(context.Context, *AddTargetRequest) (*Error, error) {
	return nil, status.Errorf(codes.Unimplemented, "method AddTarget not implemented")
}
func (UnimplementedLoadBalancerServer) DelTarget(context.Context, *DelTargetRequest) (*Error, error) {
	return nil, status.Errorf(codes.Unimplemented, "method DelTarget not implemented")
}
func (UnimplementedLoadBalancerServer) mustEmbedUnimplementedLoadBalancerServer() {}

// UnsafeLoadBalancerServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to LoadBalancerServer will
// result in compilation errors.
type UnsafeLoadBalancerServer interface {
	mustEmbedUnimplementedLoadBalancerServer()
}

func RegisterLoadBalancerServer(s grpc.ServiceRegistrar, srv LoadBalancerServer) {
	s.RegisterService(&LoadBalancer_ServiceDesc, srv)
}

func _LoadBalancer_GetLoadBalancers_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(emptypb.Empty)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(LoadBalancerServer).GetLoadBalancers(m, &loadBalancerGetLoadBalancersServer{stream})
}

type LoadBalancer_GetLoadBalancersServer interface {
	Send(*LoadBalancerInformation) error
	grpc.ServerStream
}

type loadBalancerGetLoadBalancersServer struct {
	grpc.ServerStream
}

func (x *loadBalancerGetLoadBalancersServer) Send(m *LoadBalancerInformation) error {
	return x.ServerStream.SendMsg(m)
}

func _LoadBalancer_GetLoadBalancer_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(LoadBalancerName)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(LoadBalancerServer).GetLoadBalancer(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/loadbalancer.LoadBalancer/GetLoadBalancer",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(LoadBalancerServer).GetLoadBalancer(ctx, req.(*LoadBalancerName))
	}
	return interceptor(ctx, in, info, handler)
}

func _LoadBalancer_Create_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(CreateLoadBalancer)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(LoadBalancerServer).Create(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/loadbalancer.LoadBalancer/Create",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(LoadBalancerServer).Create(ctx, req.(*CreateLoadBalancer))
	}
	return interceptor(ctx, in, info, handler)
}

func _LoadBalancer_Delete_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(LoadBalancerName)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(LoadBalancerServer).Delete(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/loadbalancer.LoadBalancer/Delete",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(LoadBalancerServer).Delete(ctx, req.(*LoadBalancerName))
	}
	return interceptor(ctx, in, info, handler)
}

func _LoadBalancer_AddTarget_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(AddTargetRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(LoadBalancerServer).AddTarget(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/loadbalancer.LoadBalancer/AddTarget",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(LoadBalancerServer).AddTarget(ctx, req.(*AddTargetRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _LoadBalancer_DelTarget_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(DelTargetRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(LoadBalancerServer).DelTarget(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/loadbalancer.LoadBalancer/DelTarget",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(LoadBalancerServer).DelTarget(ctx, req.(*DelTargetRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// LoadBalancer_ServiceDesc is the grpc.ServiceDesc for LoadBalancer service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var LoadBalancer_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "loadbalancer.LoadBalancer",
	HandlerType: (*LoadBalancerServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetLoadBalancer",
			Handler:    _LoadBalancer_GetLoadBalancer_Handler,
		},
		{
			MethodName: "Create",
			Handler:    _LoadBalancer_Create_Handler,
		},
		{
			MethodName: "Delete",
			Handler:    _LoadBalancer_Delete_Handler,
		},
		{
			MethodName: "AddTarget",
			Handler:    _LoadBalancer_AddTarget_Handler,
		},
		{
			MethodName: "DelTarget",
			Handler:    _LoadBalancer_DelTarget_Handler,
		},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "GetLoadBalancers",
			Handler:       _LoadBalancer_GetLoadBalancers_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "internal/loadbalancer/loadbalancer.proto",
}
