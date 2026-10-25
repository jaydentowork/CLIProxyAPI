//go:build windows

package pluginhost

import (
	"context"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var testReentrantHostCallback uintptr

var testWindowsCloseShutdownCalls atomic.Int64

func testWindowsCloseShutdown() uintptr {
	testWindowsCloseShutdownCalls.Add(1)
	return 0
}

func TestWindowsClosedClientKeepsResidentHostState(t *testing.T) {
	host := New()
	id := windowsHostCallbackID.Add(1)
	instance := &hostCallbackInstance{}
	hostCtx := new(uintptr)
	*hostCtx = id
	hostAPI := &windowsHostAPI{
		abiVersion: pluginHostABIVersion,
		hostCtx:    uintptr(unsafe.Pointer(hostCtx)),
		call:       windowsHostCallCallback,
		freeBuffer: windowsHostFreeCallback,
	}
	windowsResidentHostState.Store(id, windowsResidentHostBlocks{hostCtx: hostCtx, hostAPI: hostAPI})
	windowsHostCallbackEntries.Store(id, dynamicHostCallbackEntry{host: host, pluginID: "alpha", instance: instance})
	shutdownsBefore := testWindowsCloseShutdownCalls.Load()
	client := &dynamicLibraryClient{
		hostCtx:  hostCtx,
		hostAPI:  hostAPI,
		host:     host,
		pluginID: "alpha",
		instance: instance,
		api: windowsPluginAPI{
			call:       1,
			freeBuffer: 1,
			shutdown:   syscall.NewCallback(testWindowsCloseShutdown),
		},
	}

	client.close()

	if _, okEntry := windowsHostCallbackEntries.Load(id); okEntry {
		t.Fatal("callback registration retained after close; repeated reloads would pin the host")
	}
	rawBlocks, okBlocks := windowsResidentHostState.Load(id)
	if !okBlocks {
		t.Fatal("resident host state missing after close; late plugin callbacks could dereference freed memory")
	}
	blocks, okBlocks := rawBlocks.(windowsResidentHostBlocks)
	if !okBlocks || blocks.hostCtx != hostCtx || blocks.hostAPI != hostAPI {
		t.Fatalf("resident host blocks = %+v, want pinned hostCtx/hostAPI", rawBlocks)
	}
	if client.api.call != 0 || client.api.freeBuffer != 0 || client.api.shutdown != 0 {
		t.Fatalf("plugin API table not cleared by close: %+v", client.api)
	}
	if gotShutdowns := testWindowsCloseShutdownCalls.Load(); gotShutdowns != shutdownsBefore+1 {
		t.Fatalf("plugin shutdown calls = %d, want %d", gotShutdowns, shutdownsBefore+1)
	}

	// A late callback from the resident plugin runtime dereferences the
	// pinned context, finds no registration, and fails safely.
	methodBytes, errMethod := syscall.BytePtrFromString("host.log")
	if errMethod != nil {
		t.Fatalf("BytePtrFromString() error = %v", errMethod)
	}
	if rc := windowsHostCall(hostCtx, methodBytes, nil, 0, nil); rc == 0 {
		t.Fatal("windowsHostCall() with detached context succeeded, want failure")
	}
	windowsResidentHostState.Delete(id)
}

func TestDynamicLibraryClientCallSurvivesReentrantCallbackStackGrowth(t *testing.T) {
	testReentrantHostCallback = syscall.NewCallback(testGrowHostCallbackStack)
	client := newGuardedPluginClient(&dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testReentrantPluginCall),
		freeBuffer: syscall.NewCallback(testReentrantPluginFree),
	}})
	t.Cleanup(client.Shutdown)

	got, errCall := client.Call(context.Background(), "model.route", []byte(`{}`))
	if errCall != nil {
		t.Fatalf("Call() error = %v", errCall)
	}
	want := `{"ok":true,"result":{"Handled":true}}`
	if string(got) != want {
		t.Fatalf("Call() response = %q, want %q", got, want)
	}
}

func testReentrantPluginCall(_ *byte, _ *byte, _ uintptr, response *windowsBuffer) uintptr {
	if testReentrantHostCallback == 0 || response == nil {
		return 1
	}
	_, _, _ = syscall.SyscallN(testReentrantHostCallback)
	buffer, errAlloc := windowsAllocBuffer([]byte(`{"ok":true,"result":{"Handled":true}}`))
	if errAlloc != nil {
		return 1
	}
	*response = buffer
	return 0
}

func testReentrantPluginFree(ptr, _ uintptr) uintptr {
	if ptr != 0 {
		_, _ = windows.LocalFree(windows.Handle(ptr))
	}
	return 0
}

func testGrowHostCallbackStack() uintptr {
	return uintptr(testGrowStack(64))
}

//go:noinline
func testGrowStack(depth int) int {
	var padding [1024]byte
	for index := range padding {
		padding[index] = byte(index + depth)
	}
	if depth == 0 {
		return int(padding[0])
	}
	return testGrowStack(depth-1) + int(padding[depth%len(padding)])
}

var (
	testLivenessFinalizedDuringCallback atomic.Bool
	testLivenessCallbackReadCorrupted   atomic.Bool
	testLivenessCurrentOwnerID          atomic.Uint64
	testLivenessFinalizedOwnerID        atomic.Uint64
)

type testLivenessRequestBuffer [2 * 1024 * 1024]byte

func TestDynamicLibraryClientCallKeepsRequestMemoryAlive(t *testing.T) {
	testLivenessFinalizedDuringCallback.Store(false)
	testLivenessCallbackReadCorrupted.Store(false)

	client := newGuardedPluginClient(&dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testLivenessPluginCall),
		freeBuffer: syscall.NewCallback(testReentrantPluginFree),
	}})
	t.Cleanup(client.Shutdown)

	req := new(testLivenessRequestBuffer)
	req[0] = 0xAA
	req[len(req)-1] = 0x55
	ownerID := testLivenessCurrentOwnerID.Add(1)
	runtime.SetFinalizer(req, func(_ *testLivenessRequestBuffer) {
		testLivenessFinalizedOwnerID.Store(ownerID)
	})

	got, errCall := client.Call(context.Background(), "test.liveness", req[:])
	if errCall != nil {
		t.Fatalf("Call() error = %v", errCall)
	}
	want := `{"ok":true}`
	if string(got) != want {
		t.Fatalf("Call() response = %q, want %q", got, want)
	}
	if testLivenessFinalizedDuringCallback.Load() {
		t.Fatalf("request memory was finalized during native callback execution")
	}
	if testLivenessCallbackReadCorrupted.Load() {
		t.Fatalf("request buffer was corrupted during native callback execution")
	}
}

func testLivenessPluginCall(methodPtr *byte, requestPtr *byte, requestLen uintptr, response *windowsBuffer) uintptr {
	if methodPtr == nil || requestPtr == nil || requestLen == 0 || response == nil {
		return 1
	}

	for i := 0; i < 5; i++ {
		runtime.GC()
		runtime.Gosched()
	}

	currentID := testLivenessCurrentOwnerID.Load()
	if testLivenessFinalizedOwnerID.Load() == currentID {
		testLivenessFinalizedDuringCallback.Store(true)
		return 2
	}

	buf := unsafe.Slice(requestPtr, requestLen)
	if buf[0] != 0xAA || buf[requestLen-1] != 0x55 {
		testLivenessCallbackReadCorrupted.Store(true)
		return 3
	}

	buffer, errAlloc := windowsAllocBuffer([]byte(`{"ok":true}`))
	if errAlloc != nil {
		return 1
	}
	*response = buffer
	return 0
}

func TestWindowsNativeBuffer(t *testing.T) {
	raw := []byte("native response")
	buffer, errAlloc := windowsAllocBuffer(raw)
	if errAlloc != nil {
		t.Fatal(errAlloc)
	}
	defer windowsHostFree(buffer.ptr, buffer.len)
	got, errRead := windowsReadBuffer(buffer)
	if errRead != nil || string(got) != string(raw) {
		t.Fatalf("native round trip = %q, %v", got, errRead)
	}
	for _, invalid := range []windowsBuffer{
		{ptr: 0, len: 1},
		{ptr: 1, len: 1},
		{ptr: buffer.ptr, len: ^uintptr(0)},
		{ptr: ^uintptr(0), len: 2},
	} {
		if _, errRead := windowsReadBuffer(invalid); errRead == nil {
			t.Errorf("accepted invalid native range: %+v", invalid)
		}
	}
	if got, errRead := windowsReadBuffer(windowsBuffer{}); errRead != nil || len(got) != 0 {
		t.Fatalf("empty buffer = %v, %v", got, errRead)
	}
}

var testInvalidResponseFreed atomic.Bool

func testInvalidResponseCall(_ *byte, _ *byte, _ uintptr, response *windowsBuffer) uintptr {
	*response = windowsBuffer{ptr: 1, len: 1}
	return 0
}

func testInvalidResponseFree(ptr, size uintptr) uintptr {
	testInvalidResponseFreed.Store(ptr == 1 && size == 1)
	return 0
}

func TestWindowsInvalidPluginResponseIsFreed(t *testing.T) {
	testInvalidResponseFreed.Store(false)
	client := &dynamicLibraryClient{api: windowsPluginAPI{
		call:       syscall.NewCallback(testInvalidResponseCall),
		freeBuffer: syscall.NewCallback(testInvalidResponseFree),
	}}
	if _, errCall := client.Call(context.Background(), "test.invalid", nil); errCall == nil {
		t.Fatal("accepted inaccessible plugin response")
	}
	if !testInvalidResponseFreed.Load() {
		t.Error("plugin buffer was not freed after failed read")
	}
}

func TestWindowsHostCallbackABI(t *testing.T) {
	id := windowsHostCallbackID.Add(1)
	windowsHostCallbackEntries.Store(id, dynamicHostCallbackEntry{host: New(), pluginID: "alpha", instance: &hostCallbackInstance{}})
	defer windowsHostCallbackEntries.Delete(id)
	method, errMethod := syscall.BytePtrFromString("test.unsupported")
	if errMethod != nil {
		t.Fatal(errMethod)
	}
	response := new(windowsBuffer)
	var pin runtime.Pinner
	pin.Pin(response)
	pin.Pin(&id)
	defer pin.Unpin()
	rc, _, _ := syscall.SyscallN(windowsHostCallCallback,
		uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(method)), 0, 0, uintptr(unsafe.Pointer(response)))
	runtime.KeepAlive(method)
	if rc != 0 {
		t.Fatalf("host callback returned %d", rc)
	}
	defer windowsHostFree(response.ptr, response.len)
	got, errRead := windowsReadBuffer(*response)
	if errRead != nil || !isPluginErrorEnvelope(got) {
		t.Fatalf("unsupported host method response = %q, %v", got, errRead)
	}
	invalidResponse := windowsBuffer{ptr: 1, len: 1}
	if rc := windowsHostCall(&id, method, nil, 1, &invalidResponse); rc == 0 || invalidResponse != (windowsBuffer{}) {
		t.Fatalf("nil request with nonzero length: rc=%d response=%+v", rc, invalidResponse)
	}
}
