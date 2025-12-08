// Copyright IBM Corp. 2022, 2025
// SPDX-License-Identifier: MPL-2.0

// Package msgpackrpc implements a MessagePack-RPC ClientCodec and ServerCodec
// for the rpc package, using the same API as the Go standard library
// for jsonrpc.
package msgpackrpc

import (
	"net"

	"github.com/hashicorp/consul-net-rpc/net/rpc"
)

// Dial connects to a MessagePack-RPC server at the specified network address.
func Dial(network, address string) (*rpc.Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), err
}

// NewClient returns a new rpc.Client to handle requests to the set of
// services at the other end of the connection.
func NewClient(conn net.Conn) *rpc.Client {
	return rpc.NewClientWithCodec(NewClientCodec(conn))
}

// NewClientCodec returns a new rpc.ClientCodec using MessagePack-RPC on conn.
func NewClientCodec(conn net.Conn) rpc.ClientCodec {
	return NewCodec(true, true, conn)
}

// NewServerCodec returns a new rpc.ServerCodec using MessagePack-RPC on conn.
func NewServerCodec(conn net.Conn) rpc.ServerCodec {
	return NewCodec(true, true, conn)
}
