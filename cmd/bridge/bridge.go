package main

/*
#include <stdlib.h>

typedef void (*EventCallback)(const char* eventJSON);

static EventCallback _event_cb = NULL;

static void setCallback(EventCallback cb) {
    _event_cb = cb;
}

static void invokeCallback(const char* json) {
    if (_event_cb != NULL) {
        _event_cb(json);
    }
}
*/
import "C"
import (
	"encoding/json"
	"sync"
	"unsafe"

	core "github.com/kushiemoon-dev/flacidal-core"
)

var (
	app         *core.Core
	hasCallback bool
	mu          sync.Mutex
	cbMu        sync.Mutex // serialises all invokeCallback calls
)

// sendCallback allocates a C string and posts it to Dart's NativeCallable
// listener under a mutex. Serialising posts prevents concurrent goroutines
// from racing through the CGo ↔ Dart message-delivery path and causing the
// same pointer to be enqueued twice (leading to a double-free in FlacFree).
// The lock is held only for the duration of the allocation + post; Dart
// frees the pointer asynchronously via FlacFree after reading.
func sendCallback(data string) {
	cbMu.Lock()
	cstr := C.CString(data)
	C.invokeCallback(cstr)
	cbMu.Unlock()
}

//export FlacInit
func FlacInit(dataDir *C.char) *C.char {
	mu.Lock()
	defer mu.Unlock()

	dir := C.GoString(dataDir)
	var err error
	app, err = core.NewCore(dir)
	if err != nil {
		return C.CString(marshalErr("INIT_ERROR", err.Error()))
	}

	// Wire download progress to event callback
	app.SetEventCallback(func(event core.Event) {
		if !hasCallback {
			return
		}
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		sendCallback(string(data))
	})

	return C.CString(`{"result":{"status":"ok"}}`)
}

//export FlacCall
func FlacCall(methodJSON *C.char) *C.char {
	if app == nil {
		return C.CString(marshalErr("NOT_INITIALIZED", "call FlacInit first"))
	}
	input := C.GoString(methodJSON)
	result := app.HandleRPC(input)
	return C.CString(result)
}

//export FlacCallAsync
func FlacCallAsync(methodJSON *C.char, requestID C.int) {
	if app == nil {
		if hasCallback {
			sendCallback(marshalAsyncResponse(int(requestID), marshalErr("NOT_INITIALIZED", "call FlacInit first")))
		}
		return
	}
	input := C.GoString(methodJSON)
	id := int(requestID)
	go func() {
		result := app.HandleRPC(input)
		if hasCallback {
			sendCallback(marshalAsyncResponse(id, result))
		}
	}()
}

//export FlacSetEventCallback
func FlacSetEventCallback(cb C.EventCallback) {
	mu.Lock()
	defer mu.Unlock()
	C.setCallback(cb)
	hasCallback = true
}

//export FlacFree
func FlacFree(ptr *C.char) {
	C.free(unsafe.Pointer(ptr))
}

//export FlacShutdown
func FlacShutdown() {
	mu.Lock()
	defer mu.Unlock()
	if app != nil {
		app.Shutdown()
		app = nil
	}
}

func marshalErr(code, message string) string {
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]string{"code": code, "message": message},
	})
	return string(data)
}

func marshalAsyncResponse(requestID int, payload string) string {
	data, _ := json.Marshal(map[string]interface{}{
		"type":      "rpc_response",
		"requestId": requestID,
		"payload":   json.RawMessage(payload),
	})
	return string(data)
}

func main() {} // required for c-shared/c-archive
