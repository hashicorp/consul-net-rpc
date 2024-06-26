// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rpc

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	sharedServerAddr string
	sharedHTTPAddr   string
	once             sync.Once
)

const (
	newHttpPath = "/foo"
)

type Args struct {
	A, B int
}

type Reply struct {
	C int
}

type Arith int

// Some of Arith's methods have value args, some have pointer args, some use contexts. That's deliberate.

func (t *Arith) Add(ctx context.Context, args Args, reply *Reply) error {
	reply.C = args.A + args.B
	return nil
}

func (t *Arith) Mul(args *Args, reply *Reply) error {
	reply.C = args.A * args.B
	return nil
}

func (t *Arith) Div(args Args, reply *Reply) error {
	if args.B == 0 {
		return errors.New("divide by zero")
	}
	reply.C = args.A / args.B
	return nil
}

func (t *Arith) String(args *Args, reply *string) error {
	*reply = fmt.Sprintf("%d+%d=%d", args.A, args.B, args.A+args.B)
	return nil
}

func (t *Arith) Scan(args string, reply *Reply) (err error) {
	_, err = fmt.Sscan(args, &reply.C)
	return
}

func (t *Arith) Error(args *Args, reply *Reply) error {
	return errors.New("ERROR")
}

func (t *Arith) SleepMilli(args *Args, reply *Reply) error {
	time.Sleep(time.Duration(args.A) * time.Millisecond)
	return nil
}

type hidden int

func (t *hidden) Exported(args Args, reply *Reply) error {
	reply.C = args.A + args.B
	return nil
}

type Embed struct {
	hidden
}

type BuiltinTypes struct{}

func (BuiltinTypes) Map(args *Args, reply *map[int]int) error {
	(*reply)[args.A] = args.B
	return nil
}

func (BuiltinTypes) Slice(args *Args, reply *[]int) error {
	*reply = append(*reply, args.A, args.B)
	return nil
}

func (BuiltinTypes) Array(args *Args, reply *[2]int) error {
	(*reply)[0] = args.A
	(*reply)[1] = args.B
	return nil
}

func listenTCP(t testingCleanup) (net.Listener, string) {
	l, e := net.Listen("tcp", "127.0.0.1:0") // any available address
	if e != nil {
		log.Fatalf("net.Listen tcp :0: %v", e)
	}
	if t != nil {
		t.Cleanup(func() {
			_ = l.Close()
		})
	}
	return l, l.Addr().String()
}

func startSharedServer() (string, string) {
	once.Do(startSharedServerOnce)
	return sharedServerAddr, sharedHTTPAddr
}
func startSharedServerOnce() {
	DefaultServer.Register(new(Arith))
	DefaultServer.Register(new(Embed))
	DefaultServer.RegisterName("net.rpc.Arith", new(Arith))
	DefaultServer.Register(BuiltinTypes{})

	var l net.Listener
	l, sharedServerAddr = listenTCP(nil)
	log.Println("Test RPC server listening on", sharedServerAddr)
	go accept(DefaultServer, l)

	mux := http.DefaultServeMux
	handleHTTP(DefaultServer, mux, DefaultRPCPath, DefaultDebugPath)

	sharedHTTPAddr = startHttpServer(nil, mux)
}

type testingCleanup interface {
	Cleanup(func())
}

func startNewServer(t testingCleanup) (*Server, string, string) {
	newServer := NewServer()
	newServer.Register(new(Arith))
	newServer.Register(new(Embed))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))

	l, newServerAddr := listenTCP(t)
	log.Println("NewServer test RPC server listening on", newServerAddr)
	go accept(newServer, l)

	mux := http.NewServeMux()
	handleHTTP(newServer, mux, newHttpPath, "/bar")
	newHTTPAddr := startHttpServer(t, mux)

	return newServer, newServerAddr, newHTTPAddr
}

func startNewServerWithPreBodyInterceptor(t testingCleanup, preBodyinterceptor PreBodyInterceptor) (*Server, string) {
	newServer := NewServerWithOpts(WithPreBodyInterceptor(preBodyinterceptor))

	newServer.Register(new(Arith))
	newServer.Register(new(Embed))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))
	var l net.Listener
	l, newServerAddr := listenTCP(t)
	log.Println("NewServer test RPC server listening on", newServerAddr)
	go accept(newServer, l)

	return newServer, newServerAddr
}

func startNewServerWithInterceptor(t testingCleanup, interceptor ServerServiceCallInterceptor) (*Server, string) {
	newServer := NewServerWithOpts(WithServerServiceCallInterceptor(interceptor))

	newServer.Register(new(Arith))
	newServer.Register(new(Embed))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))

	l, newServerAddr := listenTCP(t)
	log.Println("NewServer test RPC server listening on", newServerAddr)
	go accept(newServer, l)

	return newServer, newServerAddr
}

func startHttpServer(t testingCleanup, mux *http.ServeMux) string {
	server := httptest.NewServer(mux)
	if t != nil {
		t.Cleanup(server.Close)
	}

	httpServerAddr := server.Listener.Addr().String()
	log.Println("Test HTTP RPC server listening on", httpServerAddr)
	return httpServerAddr
}

func TestRPC(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		serverAddr, _ := startSharedServer()
		testRPC(t, DefaultServer, serverAddr, nil)
	})
	t.Run("separate", func(t *testing.T) {
		newServer, serverAddr, _ := startNewServer(t)
		testRPC(t, newServer, serverAddr, nil)
		testNewServerRPC(t, serverAddr)
	})
	t.Run("separate with call interceptor", func(t *testing.T) {
		var callCount atomic.Int32
		interceptor := ServerServiceCallInterceptor(func(_ string, _, _ reflect.Value, handler func() error) {
			callCount.Add(1)
			_ = handler()
		})
		newServer, serverAddr := startNewServerWithInterceptor(t, interceptor)
		testRPC(t, newServer, serverAddr, &callCount)
	})
	t.Run("separate with prebody interceptor", func(t *testing.T) {
		var callCount atomic.Int32
		preBodyInterceptor := PreBodyInterceptor(func(reqServiceMethod string, sourceAddr net.Addr) error {
			callCount.Add(1)
			return nil
		})
		newServer, serverAddr := startNewServerWithPreBodyInterceptor(t, preBodyInterceptor)
		testRPC(t, newServer, serverAddr, &callCount)
	})
}

func testRPC(t *testing.T, srv *Server, addr string, interceptCount *atomic.Int32) {
	client, err := Dial("tcp", addr)
	if err != nil {
		t.Fatal("dialing", err)
	}
	defer client.Close()

	// Synchronous calls
	args := &Args{7, 8}
	reply := new(Reply)
	err = client.Call("Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	// Methods exported from unexported embedded structs
	args = &Args{7, 0}
	reply = new(Reply)
	err = client.Call("Embed.Exported", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	// Nonexistent method
	args = &Args{7, 0}
	reply = new(Reply)
	err = client.Call("Arith.BadOperation", args, reply)
	// expect an error
	if err == nil {
		t.Error("BadOperation: expected error")
	} else if !strings.HasPrefix(err.Error(), "rpc: can't find method ") {
		t.Errorf("BadOperation: expected can't find method error; got %q", err)
	}

	// Unknown service
	args = &Args{7, 8}
	reply = new(Reply)
	err = client.Call("Arith.Unknown", args, reply)
	if err == nil {
		t.Error("expected error calling unknown service")
	} else if !strings.Contains(err.Error(), "method") {
		t.Error("expected error about method; got", err)
	}

	// Out of order.
	args = &Args{7, 8}
	mulReply := new(Reply)
	mulCall := client.Go("Arith.Mul", args, mulReply, nil)
	addReply := new(Reply)
	addCall := client.Go("Arith.Add", args, addReply, nil)

	addCall = <-addCall.Done
	if addCall.Error != nil {
		t.Errorf("Add: expected no error but got string %q", addCall.Error.Error())
	}
	if addReply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", addReply.C, args.A+args.B)
	}

	mulCall = <-mulCall.Done
	if mulCall.Error != nil {
		t.Errorf("Mul: expected no error but got string %q", mulCall.Error.Error())
	}
	if mulReply.C != args.A*args.B {
		t.Errorf("Mul: expected %d got %d", mulReply.C, args.A*args.B)
	}

	// Error test
	args = &Args{7, 0}
	reply = new(Reply)
	err = client.Call("Arith.Div", args, reply)
	// expect an error: zero divide
	if err == nil {
		t.Error("Div: expected error")
	} else if err.Error() != "divide by zero" {
		t.Error("Div: expected divide by zero error; got", err)
	}

	// Bad type.
	reply = new(Reply)
	err = client.Call("Arith.Add", reply, reply) // args, reply would be the correct thing to use
	if err == nil {
		t.Error("expected error calling Arith.Add with wrong arg type")
	} else if !strings.Contains(err.Error(), "type") {
		t.Error("expected error about type; got", err)
	}

	// Non-struct argument
	const Val = 12345
	str := fmt.Sprint(Val)
	reply = new(Reply)
	err = client.Call("Arith.Scan", &str, reply)
	if err != nil {
		t.Errorf("Scan: expected no error but got string %q", err.Error())
	} else if reply.C != Val {
		t.Errorf("Scan: expected %d got %d", Val, reply.C)
	}

	// Non-struct reply
	args = &Args{27, 35}
	str = ""
	err = client.Call("Arith.String", args, &str)
	if err != nil {
		t.Errorf("String: expected no error but got string %q", err.Error())
	}
	expect := fmt.Sprintf("%d+%d=%d", args.A, args.B, args.A+args.B)
	if str != expect {
		t.Errorf("String: expected %s got %s", expect, str)
	}

	args = &Args{7, 8}
	reply = new(Reply)
	err = client.Call("Arith.Mul", args, reply)
	if err != nil {
		t.Errorf("Mul: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A*args.B {
		t.Errorf("Mul: expected %d got %d", reply.C, args.A*args.B)
	}

	// invoke directly
	if interceptCount != nil {
		interceptCount.Store(0)
	}
	rawReply, err := srv.InvokeMethod(context.Background(), "Arith.Mul", func(argvPtr any) error {
		args := argvPtr.(*Args)
		args.A = 4
		args.B = 5
		return nil
	}, net.TCPAddrFromAddrPort(netip.MustParseAddrPort("1.2.3.4:8080")))
	if err != nil {
		t.Errorf("Mul: expected no error but got string %q", err.Error())
	}
	reply = rawReply.Interface().(*Reply)
	if reply.C != 20 {
		t.Errorf("Mul: expected %d got %d", reply.C, 20)
	}
	if interceptCount != nil {
		if interceptCount.Load() != 1 {
			t.Errorf("Mul: expected to intercept call")
		}
	}

	// invoke error directly
	if interceptCount != nil {
		interceptCount.Store(0)
	}
	_, err = srv.InvokeMethod(context.Background(), "Arith.Error", func(argvPtr any) error {
		args := argvPtr.(*Args)
		args.A = 4
		args.B = 5
		return nil
	}, net.TCPAddrFromAddrPort(netip.MustParseAddrPort("1.2.3.4:8080")))
	if err == nil {
		t.Errorf("Error: expected error")
	}
	if interceptCount != nil {
		if interceptCount.Load() != 1 {
			t.Errorf("Mul: expected to intercept call")
		}
	}

	// ServiceName contain "." character
	args = &Args{7, 8}
	reply = new(Reply)
	err = client.Call("net.rpc.Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}
}

func testNewServerRPC(t *testing.T, addr string) {
	client, err := Dial("tcp", addr)
	if err != nil {
		t.Fatal("dialing", err)
	}
	defer client.Close()

	// Synchronous calls
	args := &Args{7, 8}
	reply := new(Reply)
	err = client.Call("newServer.Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}
}

func TestHTTP(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		_, httpServerAddr := startSharedServer()
		testHTTP(t, httpServerAddr, "")
	})
	t.Run("separate", func(t *testing.T) {
		_, _, httpServerAddr := startNewServer(t)
		testHTTP(t, httpServerAddr, newHttpPath)
	})
}

func testHTTP(t *testing.T, serverAddr string, path string) {
	var client *Client
	var err error
	if path == "" {
		client, err = DialHTTP("tcp", serverAddr)
	} else {
		client, err = DialHTTPPath("tcp", serverAddr, path)
	}
	if err != nil {
		t.Fatal("dialing", err)
	}
	defer client.Close()

	// Synchronous calls
	args := &Args{7, 8}
	reply := new(Reply)
	err = client.Call("Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}
}

func TestBuiltinTypes(t *testing.T) {
	_, httpServerAddr := startSharedServer()

	client, err := DialHTTP("tcp", httpServerAddr)
	if err != nil {
		t.Fatal("dialing", err)
	}
	defer client.Close()

	// Map
	args := &Args{7, 8}
	replyMap := map[int]int{}
	err = client.Call("BuiltinTypes.Map", args, &replyMap)
	if err != nil {
		t.Errorf("Map: expected no error but got string %q", err.Error())
	}
	if replyMap[args.A] != args.B {
		t.Errorf("Map: expected %d got %d", args.B, replyMap[args.A])
	}

	// Slice
	args = &Args{7, 8}
	replySlice := []int{}
	err = client.Call("BuiltinTypes.Slice", args, &replySlice)
	if err != nil {
		t.Errorf("Slice: expected no error but got string %q", err.Error())
	}
	if e := []int{args.A, args.B}; !reflect.DeepEqual(replySlice, e) {
		t.Errorf("Slice: expected %v got %v", e, replySlice)
	}

	// Array
	args = &Args{7, 8}
	replyArray := [2]int{}
	err = client.Call("BuiltinTypes.Array", args, &replyArray)
	if err != nil {
		t.Errorf("Array: expected no error but got string %q", err.Error())
	}
	if e := [2]int{args.A, args.B}; !reflect.DeepEqual(replyArray, e) {
		t.Errorf("Array: expected %v got %v", e, replyArray)
	}
}

// CodecEmulator provides a client-like api and a ServerCodec interface.
// Can be used to test ServeRequest.
type CodecEmulator struct {
	server        *Server
	serviceMethod string
	args          *Args
	reply         *Reply
	err           error
}

func (codec *CodecEmulator) Call(serviceMethod string, args *Args, reply *Reply) error {
	codec.serviceMethod = serviceMethod
	codec.args = args
	codec.reply = reply
	codec.err = nil
	serverError := codec.server.ServeRequest(codec)
	if codec.err == nil && serverError != nil {
		codec.err = serverError
	}
	return codec.err
}

func (codec *CodecEmulator) ReadRequestHeader(req *Request) error {
	req.ServiceMethod = codec.serviceMethod
	req.Seq = 0
	return nil
}

func (codec *CodecEmulator) ReadRequestBody(argv interface{}) error {
	if codec.args == nil {
		return io.ErrUnexpectedEOF
	}
	*(argv.(*Args)) = *codec.args
	return nil
}

func (codec *CodecEmulator) WriteResponse(resp *Response, reply interface{}) error {
	if resp.Error != "" {
		codec.err = errors.New(resp.Error)
	} else {
		*codec.reply = *(reply.(*Reply))
	}
	return nil
}

func (codec *CodecEmulator) SourceAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort("1.2.3.4:8080"))
}

func (codec *CodecEmulator) Close() error {
	return nil
}

func TestServeRequest(t *testing.T) {
	srv, _, _ := startNewServer(t)
	testServeRequest(t, srv)
}

func TestServeRequestWithInterceptor(t *testing.T) {
	beforeHandler := 1
	afterHandler := 3

	interceptor := ServerServiceCallInterceptor(func(reqServiceMethod string, argv, replyv reflect.Value, handler func() error) {
		// we will assert on this value later
		beforeHandler = 2

		// these values come from startNewServerWithInterceptor() wiring
		if reqServiceMethod != "Arith.Add" {
			t.Errorf("expected serviceMethod in interceptor to be \"Arith.Add\". Was: %s", reqServiceMethod)
		}

		// argv, replyv reflect.Value,
		actualArgs := argv.Interface().(Args)
		if actualArgs.A != 7 || actualArgs.B != 8 {
			t.Errorf("expected args in interceptor to be {7, 8}. Was: %+v", actualArgs)
		}

		beforeHandlerReply := replyv.Elem().Interface().(Reply)
		if beforeHandlerReply.C != 0 {
			t.Errorf("expected result in interceptor before handler call to be 0. Was %d", beforeHandlerReply.C)
		}

		// let the RPC req happen
		err := handler()
		if err != nil {
			t.Errorf("expected handler err to be nil. Was %s", err)
		}

		actualReply := replyv.Elem().Interface().(Reply)
		if actualReply.C != 15 {
			t.Errorf("expected result in interceptor to be 15. Was %d", actualReply.C)
		}

		// we will assert on this value later
		afterHandler = 4
	})

	newServer, _ := startNewServerWithInterceptor(t, interceptor)

	testServeRequest(t, newServer)

	if beforeHandler != 2 {
		t.Errorf("expected beforeHandler value to be 2. Was %d", beforeHandler)
	}

	if afterHandler != 4 {
		t.Errorf("expected beforeHandler value to be 4. Was %d", afterHandler)
	}
}

func TestPreBodyInterceptor_Success(t *testing.T) {
	preBodyInterceptorCalled := false
	expectedSourceAddr := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("1.2.3.4:8080"))

	preBodyInterceptor := PreBodyInterceptor(func(reqServiceMethod string, sourceAddr net.Addr) error {
		if reqServiceMethod != "Arith.Add" {
			t.Errorf("expected reqServiceMethod to be Arith.Add but was %s", reqServiceMethod)
		}

		if sourceAddr.String() != expectedSourceAddr.String() {
			t.Errorf("expected sourceAddr to be %s but was %s", expectedSourceAddr, sourceAddr)
		}

		preBodyInterceptorCalled = true
		return nil
	})

	newServer, _ := startNewServerWithPreBodyInterceptor(t, preBodyInterceptor)
	testServeRequest(t, newServer)

	if !preBodyInterceptorCalled {
		t.Errorf("expected preBodyInterceptorCalled to be true")
	}
}

func TestPreBodyInterceptor_Failure(t *testing.T) {
	preBodyInterceptorCalled := false

	preBodyInterceptor := PreBodyInterceptor(func(reqServiceMethod string, sourceAddr net.Addr) error {
		preBodyInterceptorCalled = true
		return errors.New("request denied")
	})

	newServer, _ := startNewServerWithPreBodyInterceptor(t, preBodyInterceptor)

	client := CodecEmulator{server: newServer}
	defer client.Close()

	args := &Args{A: 4, B: 2}
	reply := new(Reply)
	// ignore this error, because this is not how a real client reports these errors
	_ = client.Call("Arith.Div", args, reply)

	if !preBodyInterceptorCalled {
		t.Errorf("expected preBodyInterceptorCalled to be true")
	}

	expected := "request denied"
	if client.err.Error() != expected {
		t.Errorf("expected client.err to be %q but was %s", expected, client.err.Error())
	}
}

func testServeRequest(t *testing.T, server *Server) {
	client := CodecEmulator{server: server}
	defer client.Close()

	args := &Args{7, 8}
	reply := new(Reply)
	err := client.Call("Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err.Error())
	}
	if reply.C != args.A+args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	err = client.Call("Arith.Add", nil, reply)
	if err == nil {
		t.Errorf("expected error calling Arith.Add with nil arg")
	}
}

func TestServeRequestWithInterceptor_ServiceCallError(t *testing.T) {
	var handlerError error

	interceptor := ServerServiceCallInterceptor(func(reqServiceMethod string, argv, replyv reflect.Value, handler func() error) {
		handlerError = handler()
	})

	newServer, _ := startNewServerWithInterceptor(t, interceptor)

	client := CodecEmulator{server: newServer}
	defer client.Close()

	args := &Args{A: 7, B: 0}
	reply := new(Reply)
	// ignore this error, because this is not how a real client reports these errors
	_ = client.Call("Arith.Div", args, reply)

	expected := "divide by zero"
	if handlerError == nil || handlerError.Error() != expected {
		t.Fatalf("expected error %v, got %v", expected, handlerError)
	}
}

type ReplyNotPointer int
type ArgNotPublic int
type ReplyNotPublic int
type NeedsPtrType int
type FirstArgShouldBeContext int
type local struct{}

func (t *ReplyNotPointer) ReplyNotPointer(args *Args, reply Reply) error {
	return nil
}

func (t *ArgNotPublic) ArgNotPublic(args *local, reply *Reply) error {
	return nil
}

func (t *ReplyNotPublic) ReplyNotPublic(args *Args, reply *local) error {
	return nil
}

func (t *NeedsPtrType) NeedsPtrType(args *Args, reply *Reply) error {
	return nil
}

func (t *FirstArgShouldBeContext) FirstArgShouldBeContext(fakeCtx *Args, args *Args, reply *Reply) error {
	return nil
}

// Check that registration handles lots of bad methods and a type with no suitable methods.
func TestRegistrationError(t *testing.T) {
	err := DefaultServer.Register(new(ReplyNotPointer))
	if err == nil {
		t.Error("expected error registering ReplyNotPointer")
	}
	err = DefaultServer.Register(new(ArgNotPublic))
	if err == nil {
		t.Error("expected error registering ArgNotPublic")
	}
	err = DefaultServer.Register(new(ReplyNotPublic))
	if err == nil {
		t.Error("expected error registering ReplyNotPublic")
	}
	err = DefaultServer.Register(NeedsPtrType(0))
	if err == nil {
		t.Error("expected error registering NeedsPtrType")
	} else if !strings.Contains(err.Error(), "pointer") {
		t.Error("expected hint when registering NeedsPtrType")
	}
	err = DefaultServer.Register(new(FirstArgShouldBeContext))
	if err == nil {
		t.Error("expected error registering FirstArgShouldBeContext")
	}
}

type WriteFailCodec int

func (WriteFailCodec) WriteRequest(*Request, interface{}) error {
	// the panic caused by this error used to not unlock a lock.
	return errors.New("fail")
}

func (WriteFailCodec) ReadResponseHeader(*Response) error {
	select {}
}

func (WriteFailCodec) ReadResponseBody(interface{}) error {
	select {}
}

func (WriteFailCodec) Close() error {
	return nil
}

func TestSendDeadlock(t *testing.T) {
	client := NewClientWithCodec(WriteFailCodec(0))
	defer client.Close()

	done := make(chan bool)
	go func() {
		testSendDeadlock(client)
		testSendDeadlock(client)
		done <- true
	}()
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock")
	}
}

func testSendDeadlock(client *Client) {
	defer func() {
		recover()
	}()
	args := &Args{7, 8}
	reply := new(Reply)
	client.Call("Arith.Add", args, reply)
}

func dialDirect(serverAddr, _ string) (*Client, error) {
	return Dial("tcp", serverAddr)
}

func dialHTTP(_, httpServerAddr string) (*Client, error) {
	return DialHTTP("tcp", httpServerAddr)
}

func countMallocs(dial func(string, string) (*Client, error), t *testing.T) float64 {
	serverAddr, httpServerAddr := startSharedServer()
	client, err := dial(serverAddr, httpServerAddr)
	if err != nil {
		t.Fatal("error dialing", err)
	}
	defer client.Close()

	args := &Args{7, 8}
	reply := new(Reply)
	return testing.AllocsPerRun(100, func() {
		err := client.Call("Arith.Add", args, reply)
		if err != nil {
			t.Errorf("Add: expected no error but got string %q", err.Error())
		}
		if reply.C != args.A+args.B {
			t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
		}
	})
}

func TestCountMallocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping malloc count in short mode")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		t.Skip("skipping; GOMAXPROCS>1")
	}
	fmt.Printf("mallocs per rpc round trip: %v\n", countMallocs(dialDirect, t))
}

func TestCountMallocsOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping malloc count in short mode")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		t.Skip("skipping; GOMAXPROCS>1")
	}
	fmt.Printf("mallocs per HTTP rpc round trip: %v\n", countMallocs(dialHTTP, t))
}

type writeCrasher struct {
	done chan bool
}

func (writeCrasher) Close() error {
	return nil
}

func (w *writeCrasher) Read(p []byte) (int, error) {
	<-w.done
	return 0, io.EOF
}

func (writeCrasher) Write(p []byte) (int, error) {
	return 0, errors.New("fake write failure")
}

func TestClientWriteError(t *testing.T) {
	w := &writeCrasher{done: make(chan bool)}
	c := NewClient(w)
	defer c.Close()

	res := false
	err := c.Call("foo", 1, &res)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "fake write failure" {
		t.Error("unexpected value of error:", err)
	}
	w.done <- true
}

func TestTCPClose(t *testing.T) {
	_, httpServerAddr := startSharedServer()

	client, err := dialHTTP("", httpServerAddr)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer client.Close()

	args := Args{17, 8}
	var reply Reply
	err = client.Call("Arith.Mul", args, &reply)
	if err != nil {
		t.Fatal("arith error:", err)
	}
	t.Logf("Arith: %d*%d=%d\n", args.A, args.B, reply)
	if reply.C != args.A*args.B {
		t.Errorf("Add: expected %d got %d", reply.C, args.A*args.B)
	}
}

func TestErrorAfterClientClose(t *testing.T) {
	_, httpServerAddr := startSharedServer()

	client, err := dialHTTP("", httpServerAddr)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	err = client.Close()
	if err != nil {
		t.Fatal("close error:", err)
	}
	err = client.Call("Arith.Add", &Args{7, 9}, new(Reply))
	if err != ErrShutdown {
		t.Errorf("Forever: expected ErrShutdown got %v", err)
	}
}

// Tests the fix to issue 11221. Without the fix, this loops forever or crashes.
func TestAcceptExitAfterListenerClose(t *testing.T) {
	newServer := NewServer()
	newServer.Register(new(Arith))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))

	var l net.Listener
	l, _ = listenTCP(t)
	l.Close()
	accept(newServer, l)
}

func TestShutdown(t *testing.T) {
	var l net.Listener
	l, _ = listenTCP(t)
	ch := make(chan net.Conn, 1)
	go func() {
		defer l.Close()
		c, err := l.Accept()
		if err != nil {
			t.Error(err)
		}
		ch <- c
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c1 := <-ch
	if c1 == nil {
		t.Fatal(err)
	}

	newServer := NewServer()
	newServer.Register(new(Arith))
	go serveConn(newServer, c1)

	args := &Args{7, 8}
	reply := new(Reply)
	client := NewClient(c)
	err = client.Call("Arith.Add", args, reply)
	if err != nil {
		t.Fatal(err)
	}

	// On an unloaded system 10ms is usually enough to fail 100% of the time
	// with a broken server. On a loaded system, a broken server might incorrectly
	// be reported as passing, but we're OK with that kind of flakiness.
	// If the code is correct, this test will never fail, regardless of timeout.
	args.A = 10 // 10 ms
	done := make(chan *Call, 1)
	call := client.Go("Arith.SleepMilli", args, reply, done)
	c.(*net.TCPConn).CloseWrite()
	<-done
	if call.Error != nil {
		t.Fatal(err)
	}
}

func benchmarkEndToEnd(dial func(string, string) (*Client, error), b *testing.B) {
	serverAddr, httpServerAddr := startSharedServer()
	client, err := dial(serverAddr, httpServerAddr)
	if err != nil {
		b.Fatal("error dialing:", err)
	}
	defer client.Close()

	// Synchronous calls
	args := &Args{7, 8}
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		reply := new(Reply)
		for pb.Next() {
			err := client.Call("Arith.Add", args, reply)
			if err != nil {
				b.Fatalf("rpc error: Add: expected no error but got string %q", err.Error())
			}
			if reply.C != args.A+args.B {
				b.Fatalf("rpc error: Add: expected %d got %d", reply.C, args.A+args.B)
			}
		}
	})
}

func benchmarkEndToEndAsync(dial func(string, string) (*Client, error), b *testing.B) {
	if b.N == 0 {
		return
	}
	const MaxConcurrentCalls = 100
	serverAddr, httpServerAddr := startSharedServer()
	client, err := dial(serverAddr, httpServerAddr)
	if err != nil {
		b.Fatal("error dialing:", err)
	}
	defer client.Close()

	// Asynchronous calls
	args := &Args{7, 8}
	procs := 4 * runtime.GOMAXPROCS(-1)
	send := int32(b.N)
	recv := int32(b.N)
	var wg sync.WaitGroup
	wg.Add(procs)
	gate := make(chan bool, MaxConcurrentCalls)
	res := make(chan *Call, MaxConcurrentCalls)
	b.ResetTimer()

	for p := 0; p < procs; p++ {
		go func() {
			for atomic.AddInt32(&send, -1) >= 0 {
				gate <- true
				reply := new(Reply)
				client.Go("Arith.Add", args, reply, res)
			}
		}()
		go func() {
			for call := range res {
				A := call.Args.(*Args).A
				B := call.Args.(*Args).B
				C := call.Reply.(*Reply).C
				if A+B != C {
					b.Errorf("incorrect reply: Add: expected %d got %d", A+B, C)
					return
				}
				<-gate
				if atomic.AddInt32(&recv, -1) == 0 {
					close(res)
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkEndToEnd(b *testing.B) {
	benchmarkEndToEnd(dialDirect, b)
}

func BenchmarkEndToEndHTTP(b *testing.B) {
	benchmarkEndToEnd(dialHTTP, b)
}

func BenchmarkEndToEndAsync(b *testing.B) {
	benchmarkEndToEndAsync(dialDirect, b)
}

func BenchmarkEndToEndAsyncHTTP(b *testing.B) {
	benchmarkEndToEndAsync(dialHTTP, b)
}

// accept accepts connections on the listener and serves requests
// for each incoming connection. Accept blocks until the listener
// returns a non-nil error. The caller typically invokes Accept in a
// go statement.
func accept(server *Server, lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Print("rpc.Serve: accept:", err.Error())
			return
		}
		go serveConn(server, conn)
	}
}

// handleHTTP registers an HTTP handler for RPC messages on rpcPath,
// and a debugging handler on debugPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func handleHTTP(server *Server, mux *http.ServeMux, rpcPath, debugPath string) {
	mux.Handle(rpcPath, http.HandlerFunc(serveHTTP(server)))
	mux.Handle(debugPath, debugHTTP{server})
}

// serveHTTP implements an http.HandlerFunc that answers RPC requests.
func serveHTTP(server *Server) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "CONNECT" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMethodNotAllowed)
			io.WriteString(w, "405 must CONNECT\n")
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
			return
		}
		io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
		serveConn(server, conn)
	}
}

func serveConn(server *Server, conn net.Conn) {
	buf := bufio.NewWriter(conn)
	codec := &gobServerCodec{
		conn:   conn,
		dec:    gob.NewDecoder(conn),
		enc:    gob.NewEncoder(buf),
		encBuf: buf,
	}
	defer codec.Close()
	for {
		if err := server.ServeRequest(codec); err == io.EOF {
			return
		}
	}
}
