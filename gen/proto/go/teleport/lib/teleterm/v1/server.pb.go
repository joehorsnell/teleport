// Copyright 2021 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.31.0
// 	protoc        (unknown)
// source: teleport/lib/teleterm/v1/server.proto

package teletermv1

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// Server describes connected Server
type Server struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// uri is the server uri
	Uri string `protobuf:"bytes,1,opt,name=uri,proto3" json:"uri,omitempty"`
	// tunnel indicates if this server is connected over a reverse tunnel
	Tunnel bool `protobuf:"varint,2,opt,name=tunnel,proto3" json:"tunnel,omitempty"`
	// name is the server name
	Name string `protobuf:"bytes,3,opt,name=name,proto3" json:"name,omitempty"`
	// hostname is this server hostname
	Hostname string `protobuf:"bytes,4,opt,name=hostname,proto3" json:"hostname,omitempty"`
	// addr is this server ip address
	Addr string `protobuf:"bytes,5,opt,name=addr,proto3" json:"addr,omitempty"`
	// labels is this server list of labels
	Labels []*Label `protobuf:"bytes,6,rep,name=labels,proto3" json:"labels,omitempty"`
}

func (x *Server) Reset() {
	*x = Server{}
	if protoimpl.UnsafeEnabled {
		mi := &file_teleport_lib_teleterm_v1_server_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Server) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Server) ProtoMessage() {}

func (x *Server) ProtoReflect() protoreflect.Message {
	mi := &file_teleport_lib_teleterm_v1_server_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Server.ProtoReflect.Descriptor instead.
func (*Server) Descriptor() ([]byte, []int) {
	return file_teleport_lib_teleterm_v1_server_proto_rawDescGZIP(), []int{0}
}

func (x *Server) GetUri() string {
	if x != nil {
		return x.Uri
	}
	return ""
}

func (x *Server) GetTunnel() bool {
	if x != nil {
		return x.Tunnel
	}
	return false
}

func (x *Server) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *Server) GetHostname() string {
	if x != nil {
		return x.Hostname
	}
	return ""
}

func (x *Server) GetAddr() string {
	if x != nil {
		return x.Addr
	}
	return ""
}

func (x *Server) GetLabels() []*Label {
	if x != nil {
		return x.Labels
	}
	return nil
}

var File_teleport_lib_teleterm_v1_server_proto protoreflect.FileDescriptor

var file_teleport_lib_teleterm_v1_server_proto_rawDesc = []byte{
	0x0a, 0x25, 0x74, 0x65, 0x6c, 0x65, 0x70, 0x6f, 0x72, 0x74, 0x2f, 0x6c, 0x69, 0x62, 0x2f, 0x74,
	0x65, 0x6c, 0x65, 0x74, 0x65, 0x72, 0x6d, 0x2f, 0x76, 0x31, 0x2f, 0x73, 0x65, 0x72, 0x76, 0x65,
	0x72, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x18, 0x74, 0x65, 0x6c, 0x65, 0x70, 0x6f, 0x72,
	0x74, 0x2e, 0x6c, 0x69, 0x62, 0x2e, 0x74, 0x65, 0x6c, 0x65, 0x74, 0x65, 0x72, 0x6d, 0x2e, 0x76,
	0x31, 0x1a, 0x24, 0x74, 0x65, 0x6c, 0x65, 0x70, 0x6f, 0x72, 0x74, 0x2f, 0x6c, 0x69, 0x62, 0x2f,
	0x74, 0x65, 0x6c, 0x65, 0x74, 0x65, 0x72, 0x6d, 0x2f, 0x76, 0x31, 0x2f, 0x6c, 0x61, 0x62, 0x65,
	0x6c, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0xaf, 0x01, 0x0a, 0x06, 0x53, 0x65, 0x72, 0x76,
	0x65, 0x72, 0x12, 0x10, 0x0a, 0x03, 0x75, 0x72, 0x69, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52,
	0x03, 0x75, 0x72, 0x69, 0x12, 0x16, 0x0a, 0x06, 0x74, 0x75, 0x6e, 0x6e, 0x65, 0x6c, 0x18, 0x02,
	0x20, 0x01, 0x28, 0x08, 0x52, 0x06, 0x74, 0x75, 0x6e, 0x6e, 0x65, 0x6c, 0x12, 0x12, 0x0a, 0x04,
	0x6e, 0x61, 0x6d, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x6e, 0x61, 0x6d, 0x65,
	0x12, 0x1a, 0x0a, 0x08, 0x68, 0x6f, 0x73, 0x74, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x04, 0x20, 0x01,
	0x28, 0x09, 0x52, 0x08, 0x68, 0x6f, 0x73, 0x74, 0x6e, 0x61, 0x6d, 0x65, 0x12, 0x12, 0x0a, 0x04,
	0x61, 0x64, 0x64, 0x72, 0x18, 0x05, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x61, 0x64, 0x64, 0x72,
	0x12, 0x37, 0x0a, 0x06, 0x6c, 0x61, 0x62, 0x65, 0x6c, 0x73, 0x18, 0x06, 0x20, 0x03, 0x28, 0x0b,
	0x32, 0x1f, 0x2e, 0x74, 0x65, 0x6c, 0x65, 0x70, 0x6f, 0x72, 0x74, 0x2e, 0x6c, 0x69, 0x62, 0x2e,
	0x74, 0x65, 0x6c, 0x65, 0x74, 0x65, 0x72, 0x6d, 0x2e, 0x76, 0x31, 0x2e, 0x4c, 0x61, 0x62, 0x65,
	0x6c, 0x52, 0x06, 0x6c, 0x61, 0x62, 0x65, 0x6c, 0x73, 0x42, 0x54, 0x5a, 0x52, 0x67, 0x69, 0x74,
	0x68, 0x75, 0x62, 0x2e, 0x63, 0x6f, 0x6d, 0x2f, 0x67, 0x72, 0x61, 0x76, 0x69, 0x74, 0x61, 0x74,
	0x69, 0x6f, 0x6e, 0x61, 0x6c, 0x2f, 0x74, 0x65, 0x6c, 0x65, 0x70, 0x6f, 0x72, 0x74, 0x2f, 0x67,
	0x65, 0x6e, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2f, 0x67, 0x6f, 0x2f, 0x74, 0x65, 0x6c, 0x65,
	0x70, 0x6f, 0x72, 0x74, 0x2f, 0x6c, 0x69, 0x62, 0x2f, 0x74, 0x65, 0x6c, 0x65, 0x74, 0x65, 0x72,
	0x6d, 0x2f, 0x76, 0x31, 0x3b, 0x74, 0x65, 0x6c, 0x65, 0x74, 0x65, 0x72, 0x6d, 0x76, 0x31, 0x62,
	0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_teleport_lib_teleterm_v1_server_proto_rawDescOnce sync.Once
	file_teleport_lib_teleterm_v1_server_proto_rawDescData = file_teleport_lib_teleterm_v1_server_proto_rawDesc
)

func file_teleport_lib_teleterm_v1_server_proto_rawDescGZIP() []byte {
	file_teleport_lib_teleterm_v1_server_proto_rawDescOnce.Do(func() {
		file_teleport_lib_teleterm_v1_server_proto_rawDescData = protoimpl.X.CompressGZIP(file_teleport_lib_teleterm_v1_server_proto_rawDescData)
	})
	return file_teleport_lib_teleterm_v1_server_proto_rawDescData
}

var file_teleport_lib_teleterm_v1_server_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_teleport_lib_teleterm_v1_server_proto_goTypes = []interface{}{
	(*Server)(nil), // 0: teleport.lib.teleterm.v1.Server
	(*Label)(nil),  // 1: teleport.lib.teleterm.v1.Label
}
var file_teleport_lib_teleterm_v1_server_proto_depIdxs = []int32{
	1, // 0: teleport.lib.teleterm.v1.Server.labels:type_name -> teleport.lib.teleterm.v1.Label
	1, // [1:1] is the sub-list for method output_type
	1, // [1:1] is the sub-list for method input_type
	1, // [1:1] is the sub-list for extension type_name
	1, // [1:1] is the sub-list for extension extendee
	0, // [0:1] is the sub-list for field type_name
}

func init() { file_teleport_lib_teleterm_v1_server_proto_init() }
func file_teleport_lib_teleterm_v1_server_proto_init() {
	if File_teleport_lib_teleterm_v1_server_proto != nil {
		return
	}
	file_teleport_lib_teleterm_v1_label_proto_init()
	if !protoimpl.UnsafeEnabled {
		file_teleport_lib_teleterm_v1_server_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Server); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_teleport_lib_teleterm_v1_server_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_teleport_lib_teleterm_v1_server_proto_goTypes,
		DependencyIndexes: file_teleport_lib_teleterm_v1_server_proto_depIdxs,
		MessageInfos:      file_teleport_lib_teleterm_v1_server_proto_msgTypes,
	}.Build()
	File_teleport_lib_teleterm_v1_server_proto = out.File
	file_teleport_lib_teleterm_v1_server_proto_rawDesc = nil
	file_teleport_lib_teleterm_v1_server_proto_goTypes = nil
	file_teleport_lib_teleterm_v1_server_proto_depIdxs = nil
}
