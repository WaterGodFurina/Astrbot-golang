//go:build windows

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall"
	"unsafe"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
	pluginNative "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2/native"
)

// openNativePlugin loads a Native plugin DLL on Windows and returns a PluginClient
// backed by the C-handle bridge. Go's stdlib `plugin` is unavailable on Windows,
// so the DLL is built with -buildmode=c-shared and exposes four C symbols:
//
//	AstrBotPluginOpen(pluginID, handleOut)          -> 0 ok / nonzero err
//	AstrBotPluginCall(handle, method, req, len, ..)  -> 0 ok / 3 err (err text)
//	AstrBotFree(p)                                    free a response buffer
//	AstrBotPluginClose(handle)
//
// Request/response cross the C ABI as encoding/json bytes (a C ABI cannot carry
// Go values): requests are the SDK native `native.CABI*Request` envelopes,
// responses are the JSON of the SDK native result types (sdk.PluginInfo,
// sdk.HandleCommandResult, ...). No protobuf, no gRPC, no JSON event plane.
func openNativePlugin(path, pluginID string) (pluginsdk.PluginClient, func() error, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, nil, fmt.Errorf("LoadDLL(%s): %w", path, err)
	}
	procOpen, err := dll.FindProc("AstrBotPluginOpen")
	if err != nil {
		_ = dll.Release()
		return nil, nil, fmt.Errorf("FindProc(AstrBotPluginOpen) in %s: %w", path, err)
	}
	procCall, err := dll.FindProc("AstrBotPluginCall")
	if err != nil {
		_ = dll.Release()
		return nil, nil, fmt.Errorf("FindProc(AstrBotPluginCall) in %s: %w", path, err)
	}
	procFree, err := dll.FindProc("AstrBotFree")
	if err != nil {
		_ = dll.Release()
		return nil, nil, fmt.Errorf("FindProc(AstrBotFree) in %s: %w", path, err)
	}
	procClose, err := dll.FindProc("AstrBotPluginClose")
	if err != nil {
		_ = dll.Release()
		return nil, nil, fmt.Errorf("FindProc(AstrBotPluginClose) in %s: %w", path, err)
	}
	idPtr, err := syscall.BytePtrFromString(pluginID)
	if err != nil {
		_ = dll.Release()
		return nil, nil, err
	}
	var handle uintptr
	r, _, _ := procOpen.Call(
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if r != 0 {
		_ = dll.Release()
		return nil, nil, fmt.Errorf("AstrBotPluginOpen(%s) failed (code %d)", pluginID, r)
	}

	c := &nativeABIClient{
		id:        pluginID,
		handle:    handle,
		procCall:  procCall,
		procFree:  procFree,
		procClose: procClose,
	}
	cleanup := func() error {
		if c.handle != 0 {
			_, _, _ = c.procClose.Call(c.handle)
			c.handle = 0
		}
		return dll.Release()
	}
	return c, cleanup, nil
}

// nativeABIClient implements pluginsdk.PluginClient by forwarding each call to
// the DLL's AstrBotPluginCall with JSON request/response bytes.
type nativeABIClient struct {
	id        string
	handle    uintptr
	procCall  *syscall.Proc
	procFree  *syscall.Proc
	procClose *syscall.Proc
}

var _ pluginsdk.PluginClient = (*nativeABIClient)(nil)

func (c *nativeABIClient) PluginID() string                        { return c.id }
func (c *nativeABIClient) ForPlugin(string) pluginsdk.PluginClient { return c }
func (c *nativeABIClient) Close() error {
	if c.handle != 0 {
		_, _, _ = c.procClose.Call(c.handle)
		c.handle = 0
	}
	return nil
}

// call JSON-marshals req, invokes the DLL and JSON-unmarshals the response into
// out (out may be nil for methods with an empty response). code 3 from the DLL
// carries a plain error string instead of a JSON body.
func (c *nativeABIClient) call(method string, req any, out any) error {
	var reqBytes []byte
	if req != nil {
		var err error
		if reqBytes, err = json.Marshal(req); err != nil {
			return err
		}
	}
	methodPtr, err := syscall.BytePtrFromString(method)
	if err != nil {
		return err
	}
	var reqPtr *byte
	if len(reqBytes) > 0 {
		reqPtr = &reqBytes[0]
	}
	var respPtr *byte
	var respLen int32
	r, _, _ := c.procCall.Call(
		c.handle,
		uintptr(unsafe.Pointer(methodPtr)),
		uintptr(unsafe.Pointer(reqPtr)),
		uintptr(int32(len(reqBytes))),
		uintptr(unsafe.Pointer(&respPtr)),
		uintptr(unsafe.Pointer(&respLen)),
	)
	// 先把 DLL 分配的响应缓冲复制到 Go 自有内存，再 AstrBotFree——否则
	// respBytes 指向已释放内存，随后的 json.Unmarshal 是 use-after-free
	// （Linux 分配器保留映射而"侥幸"可用，Windows 直接崩溃）。
	var respBytes []byte
	if respPtr != nil && respLen > 0 {
		respBytes = make([]byte, int(respLen))
		copy(respBytes, unsafe.Slice(respPtr, int(respLen)))
	}
	if respPtr != nil {
		_, _, _ = c.procFree.Call(uintptr(unsafe.Pointer(respPtr)))
	}
	switch r {
	case 0:
		if out != nil && len(respBytes) > 0 {
			return json.Unmarshal(respBytes, out)
		}
		return nil
	case 3:
		return fmt.Errorf("%s", string(respBytes))
	default:
		return fmt.Errorf("native ABI call %s failed (code %d)", method, r)
	}
}

func (c *nativeABIClient) Register(ctx context.Context) (pluginsdk.PluginInfo, error) {
	var out pluginsdk.PluginInfo
	if err := c.call("Register", pluginNative.CABIRegisterRequest{ProtocolVersion: pluginsdk.P1ProtocolVersion}, &out); err != nil {
		return pluginsdk.PluginInfo{}, err
	}
	return out, nil
}

func (c *nativeABIClient) HandleCommand(ctx context.Context, name string, args []string, event *pluginsdk.Event) (pluginsdk.HandleCommandResult, error) {
	var out pluginsdk.HandleCommandResult
	if err := c.call("HandleCommand", pluginNative.CABIHandleCommandRequest{Name: name, Args: args, Event: event}, &out); err != nil {
		return pluginsdk.HandleCommandResult{}, err
	}
	return out, nil
}

func (c *nativeABIClient) HandleFilter(ctx context.Context, name string, event *pluginsdk.Event) (pluginsdk.HandleFilterResult, error) {
	var out pluginsdk.HandleFilterResult
	if err := c.call("HandleFilter", pluginNative.CABIHandleFilterRequest{Name: name, Event: event}, &out); err != nil {
		return pluginsdk.HandleFilterResult{Allow: true}, err
	}
	return out, nil
}

func (c *nativeABIClient) HandleHook(ctx context.Context, name string, event *pluginsdk.Event, chain []pluginsdk.Component) (pluginsdk.HandleHookResult, error) {
	return c.handleHook(ctx, name, event, chain, nil)
}

func (c *nativeABIClient) HandleHookWithPayload(ctx context.Context, name string, event *pluginsdk.Event, chain []pluginsdk.Component, payload any) (pluginsdk.HandleHookResult, error) {
	return c.handleHook(ctx, name, event, chain, payload)
}

func (c *nativeABIClient) handleHook(ctx context.Context, name string, event *pluginsdk.Event, chain []pluginsdk.Component, payload any) (pluginsdk.HandleHookResult, error) {
	var payloadJSON json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return pluginsdk.HandleHookResult{Chain: chain}, err
		}
		payloadJSON = b
	}
	var out pluginsdk.HandleHookResult
	if err := c.call("HandleHook", pluginNative.CABIHandleHookRequest{Name: name, Event: event, Chain: chain, PayloadJSON: payloadJSON}, &out); err != nil {
		return pluginsdk.HandleHookResult{Chain: chain}, err
	}
	if len(out.Chain) == 0 {
		out.Chain = chain
	}
	return out, nil
}

func (c *nativeABIClient) HandleLLMRequest(ctx context.Context, name string, event *pluginsdk.Event, systemPrompt, userPrompt string) (pluginsdk.HandleLLMRequestResult, error) {
	var out pluginsdk.HandleLLMRequestResult
	if err := c.call("HandleLLMRequest", pluginNative.CABIHandleLLMRequestRequest{Name: name, Event: event, SystemPrompt: systemPrompt, UserPrompt: userPrompt}, &out); err != nil {
		return pluginsdk.HandleLLMRequestResult{SystemPrompt: systemPrompt, UserPrompt: userPrompt}, err
	}
	return out, nil
}

func (c *nativeABIClient) ListWebApis(ctx context.Context) ([]pluginsdk.WebAPIDesc, error) {
	var out []pluginsdk.WebAPIDesc
	if err := c.call("ListWebApis", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *nativeABIClient) ListTools(ctx context.Context) ([]pluginsdk.ToolDesc, error) {
	var out []pluginsdk.ToolDesc
	if err := c.call("ListTools", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *nativeABIClient) GetConfigSchema(ctx context.Context) ([]byte, error) {
	var out []byte
	if err := c.call("GetConfigSchema", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *nativeABIClient) HandleTool(ctx context.Context, name string, args map[string]any, event *pluginsdk.Event) (pluginsdk.HandleToolResult, error) {
	var out pluginsdk.HandleToolResult
	if err := c.call("HandleTool", pluginNative.CABIHandleToolRequest{Name: name, Args: args, Event: event}, &out); err != nil {
		return pluginsdk.HandleToolResult{}, err
	}
	return out, nil
}

func (c *nativeABIClient) HandleWebRequest(ctx context.Context, req pluginsdk.HandleWebRequest) (pluginsdk.HandleWebResponse, error) {
	if req.PluginID == "" {
		req.PluginID = c.id
	}
	var out pluginsdk.HandleWebResponse
	if err := c.call("HandleWebRequest", pluginNative.CABIHandleWebRequest{Req: req}, &out); err != nil {
		return pluginsdk.HandleWebResponse{}, err
	}
	return out, nil
}

func (c *nativeABIClient) HealthCheck(ctx context.Context) (pluginsdk.HealthInfo, error) {
	var out pluginsdk.HealthInfo
	if err := c.call("HealthCheck", nil, &out); err != nil {
		return pluginsdk.HealthInfo{}, err
	}
	return out, nil
}

func (c *nativeABIClient) Cleanup(ctx context.Context) error {
	return c.call("Cleanup", nil, nil)
}

func (c *nativeABIClient) SetLogLevel(ctx context.Context, level string) error {
	return c.call("SetLogLevel", pluginNative.CABISetLogLevelRequest{Level: level}, nil)
}

func (c *nativeABIClient) FeedSessionWait(ctx context.Context, event *pluginsdk.Event) (bool, error) {
	var out pluginsdk.FeedSessionWaitResult
	if err := c.call("FeedSessionWait", pluginNative.CABIFeedSessionWaitRequest{Event: event}, &out); err != nil {
		return false, err
	}
	return out.Handled, nil
}

func (c *nativeABIClient) ManagePlugin(ctx context.Context, req pluginsdk.ManagePluginRequest) (pluginsdk.ManagePluginResponse, error) {
	if req.PluginID == "" {
		req.PluginID = c.id
	}
	var out pluginsdk.ManagePluginResponse
	if err := c.call("ManagePlugin", req, &out); err != nil {
		return pluginsdk.ManagePluginResponse{}, err
	}
	return out, nil
}

func (c *nativeABIClient) FeedCronJob(ctx context.Context, req pluginsdk.FeedCronJobRequest) (pluginsdk.FeedCronJobResponse, error) {
	if req.PluginID == "" {
		req.PluginID = c.id
	}
	var out pluginsdk.FeedCronJobResponse
	if err := c.call("FeedCronJob", req, &out); err != nil {
		return pluginsdk.FeedCronJobResponse{}, err
	}
	return out, nil
}
