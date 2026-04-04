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
)

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
		cstr := C.CString(string(data))
		C.invokeCallback(cstr)
		// Do NOT free here — Dart's NativeCallable.listener processes
		// callbacks asynchronously; the pointer must remain valid until
		// Dart copies the string. Dart calls FlacFree after reading.
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
			errJSON := marshalAsyncResponse(int(requestID), marshalErr("NOT_INITIALIZED", "call FlacInit first"))
			cstr := C.CString(errJSON)
			C.invokeCallback(cstr)
			// Dart frees via FlacFree after reading
		}
		return
	}
	input := C.GoString(methodJSON)
	id := int(requestID)
	go func() {
		result := app.HandleRPC(input)
		if hasCallback {
			resp := marshalAsyncResponse(id, result)
			cstr := C.CString(resp)
			C.invokeCallback(cstr)
			// Dart frees via FlacFree after reading
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
