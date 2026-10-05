//go:build windows

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall"
	"unsafe"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"google.golang.org/protobuf/proto"
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
// Request/response cross the C ABI as sdkv1 protobuf bytes (one marshal/
// unmarshal round trip), which keeps the C surface tiny and reuses the same
// protocol as the gRPC runtime. No loopback gRPC, no JSON.
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
// the DLL's AstrBotPluginCall with sdkv1 protobuf bytes.
type nativeABIClient struct {
	id        string
	handle    uintptr
	procCall  *syscall.Proc
	procFree  *syscall.Proc
	procClose *syscall.Proc
}

var _ pluginsdk.PluginClient = (*nativeABIClient)(nil)

func (c *nativeABIClient) PluginID() string                   { return c.id }
func (c *nativeABIClient) ForPlugin(string) pluginsdk.PluginClient { return c }
func (c *nativeABIClient) Close() error {
	if c.handle != 0 {
		_, _, _ = c.procClose.Call(c.handle)
		c.handle = 0
	}
	return nil
}

// call marshals req, invokes the DLL and unmarshals the response into out
// (out may be nil for methods with an empty response). code 3 from the DLL
// carries a plain error string instead of a protobuf body.
func (c *nativeABIClient) call(method string, req proto.Message, out proto.Message) error {
	var reqBytes []byte
	if req != nil {
		var err error
		if reqBytes, err = proto.Marshal(req); err != nil {
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
	var respBytes []byte
	if respPtr != nil && respLen > 0 {
		respBytes = unsafe.Slice(respPtr, int(respLen))
	}
	if respPtr != nil {
		_, _, _ = c.procFree.Call(uintptr(unsafe.Pointer(respPtr)))
	}
	switch r {
	case 0:
		if out != nil && len(respBytes) > 0 {
			return proto.Unmarshal(respBytes, out)
		}
		return nil
	case 3:
		return fmt.Errorf("%s", string(respBytes))
	default:
		return fmt.Errorf("native ABI call %s failed (code %d)", method, r)
	}
}

func (c *nativeABIClient) Register(ctx context.Context) (*sdkv1.RegisterResponse, error) {
	resp := &sdkv1.RegisterResponse{}
	if err := c.call("Register", &sdkv1.RegisterRequest{ProtocolVersion: pluginsdk.P1ProtocolVersion, PluginId: c.id}, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *nativeABIClient) HandleCommand(ctx context.Context, name string, args []string, se *sdkv1.SDKEvent) (string, []pluginsdk.Component, *sdkv1.EventResult, error) {
	resp := &sdkv1.HandleCommandResponse{}
	if err := c.call("HandleCommand", &sdkv1.HandleCommandRequest{Name: name, Args: args, Event: se, PluginId: c.id}, resp); err != nil {
		return "", nil, &sdkv1.EventResult{}, err
	}
	return resp.Text, pluginsdk.ProtoToComponents(resp.Chain), cabiResult(resp.Result, resp.Sent, resp.Stop, false), nil
}

func (c *nativeABIClient) HandleFilter(ctx context.Context, name string, se *sdkv1.SDKEvent) (bool, *sdkv1.EventResult, error) {
	resp := &sdkv1.HandleFilterResponse{}
	if err := c.call("HandleFilter", &sdkv1.HandleFilterRequest{Name: name, Event: se, PluginId: c.id}, resp); err != nil {
		return true, &sdkv1.EventResult{}, err
	}
	return resp.Allow, cabiResult(resp.Result, resp.Sent, false, false), nil
}

func (c *nativeABIClient) HandleHook(ctx context.Context, name string, se *sdkv1.SDKEvent, chain []pluginsdk.Component) ([]pluginsdk.Component, bool, *sdkv1.EventResult, error) {
	return c.handleHook(ctx, name, se, chain, nil)
}

func (c *nativeABIClient) HandleHookWithPayload(ctx context.Context, name string, se *sdkv1.SDKEvent, chain []pluginsdk.Component, payload any) ([]pluginsdk.Component, bool, *sdkv1.EventResult, error) {
	return c.handleHook(ctx, name, se, chain, payload)
}

func (c *nativeABIClient) handleHook(ctx context.Context, name string, se *sdkv1.SDKEvent, chain []pluginsdk.Component, payload any) ([]pluginsdk.Component, bool, *sdkv1.EventResult, error) {
	var payloadJSON []byte
	if payload != nil {
		var err error
		if payloadJSON, err = json.Marshal(payload); err != nil {
			return chain, false, &sdkv1.EventResult{}, err
		}
	}
	resp := &sdkv1.HookResponse{}
	if err := c.call("HandleHook", &sdkv1.HandleHookRequest{Name: name, Event: se, Chain: pluginsdk.ComponentsToProto(chain), PayloadJson: payloadJSON, PluginId: c.id}, resp); err != nil {
		return chain, false, &sdkv1.EventResult{}, err
	}
	if len(resp.Chain) > 0 {
		chain = pluginsdk.ProtoToComponents(resp.Chain)
	}
	res := cabiResult(resp.Result, resp.Sent, resp.Stop, resp.Handled)
	return chain, res.StopPropagation, res, nil
}

func (c *nativeABIClient) HandleLLMRequest(ctx context.Context, name string, se *sdkv1.SDKEvent, systemPrompt, userPrompt string) (string, string, bool, *sdkv1.EventResult, error) {
	resp := &sdkv1.HandleLLMRequestResponse{}
	if err := c.call("HandleLLMRequest", &sdkv1.HandleLLMRequestRequest{Name: name, Event: se, SystemPrompt: systemPrompt, UserPrompt: userPrompt, PluginId: c.id}, resp); err != nil {
		return systemPrompt, userPrompt, false, &sdkv1.EventResult{}, err
	}
	result := cabiResult(resp.Result, resp.Sent, resp.Stop, false)
	return resp.SystemPrompt, resp.UserPrompt, result.StopPropagation, result, nil
}

func (c *nativeABIClient) ListWebApis(ctx context.Context, ref *sdkv1.PluginRef) ([]*sdkv1.WebApiDesc, error) {
	if ref == nil {
		ref = &sdkv1.PluginRef{PluginId: c.id}
	}
	resp := &sdkv1.ListWebApisResponse{}
	if err := c.call("ListWebApis", ref, resp); err != nil {
		return nil, err
	}
	return resp.GetWebApis(), nil
}

func (c *nativeABIClient) ListTools(ctx context.Context, ref *sdkv1.PluginRef) ([]*sdkv1.ToolDesc, error) {
	if ref == nil {
		ref = &sdkv1.PluginRef{PluginId: c.id}
	}
	resp := &sdkv1.ListToolsResponse{}
	if err := c.call("ListTools", ref, resp); err != nil {
		return nil, err
	}
	return resp.GetTools(), nil
}

func (c *nativeABIClient) GetConfigSchema(ctx context.Context, ref *sdkv1.PluginRef) ([]byte, error) {
	if ref == nil {
		ref = &sdkv1.PluginRef{PluginId: c.id}
	}
	resp := &sdkv1.GetConfigSchemaResponse{}
	if err := c.call("GetConfigSchema", ref, resp); err != nil {
		return nil, err
	}
	return resp.GetSchemaJson(), nil
}

func (c *nativeABIClient) HandleTool(ctx context.Context, name string, args map[string]any, se *sdkv1.SDKEvent) (string, bool, *sdkv1.EventResult, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", false, &sdkv1.EventResult{}, err
	}
	resp := &sdkv1.HandleToolResponse{}
	if err := c.call("HandleTool", &sdkv1.HandleToolRequest{Name: name, ArgsJson: argsJSON, Event: se, PluginId: c.id}, resp); err != nil {
		return "", false, &sdkv1.EventResult{}, err
	}
	return resp.Text, resp.IsError, cabiResult(resp.Result, resp.Sent, false, false), nil
}

func (c *nativeABIClient) HandleWebRequest(ctx context.Context, req *sdkv1.HandleWebRequestRequest) (*sdkv1.HandleWebRequestResponse, error) {
	if req != nil && req.PluginId == "" {
		req.PluginId = c.id
	}
	resp := &sdkv1.HandleWebRequestResponse{}
	if err := c.call("HandleWebRequest", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *nativeABIClient) HealthCheck(ctx context.Context) (*sdkv1.HealthResponse, error) {
	resp := &sdkv1.HealthResponse{}
	if err := c.call("HealthCheck", &sdkv1.Empty{}, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *nativeABIClient) Cleanup(ctx context.Context, ref *sdkv1.PluginRef) error {
	if ref == nil {
		ref = &sdkv1.PluginRef{PluginId: c.id}
	}
	return c.call("Cleanup", ref, &sdkv1.Empty{})
}

func (c *nativeABIClient) SetLogLevel(ctx context.Context, level string) error {
	return c.call("SetLogLevel", &sdkv1.SetLogLevelRequest{Level: level, PluginId: c.id}, &sdkv1.Empty{})
}

func (c *nativeABIClient) FeedSessionWait(ctx context.Context, se *sdkv1.SDKEvent) (bool, error) {
	resp := &sdkv1.FeedSessionWaitResponse{}
	if err := c.call("FeedSessionWait", &sdkv1.FeedSessionWaitRequest{Event: se, PluginId: c.id}, resp); err != nil {
		return false, err
	}
	return resp.GetHandled(), nil
}

func (c *nativeABIClient) ManagePlugin(ctx context.Context, req *sdkv1.ManagePluginRequest) (*sdkv1.ManagePluginResponse, error) {
	if req != nil && req.PluginId == "" {
		req.PluginId = c.id
	}
	resp := &sdkv1.ManagePluginResponse{}
	if err := c.call("ManagePlugin", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *nativeABIClient) FeedCronJob(ctx context.Context, req *sdkv1.FeedCronJobRequest) (*sdkv1.FeedCronJobResponse, error) {
	if req != nil && req.PluginId == "" {
		req.PluginId = c.id
	}
	resp := &sdkv1.FeedCronJobResponse{}
	if err := c.call("FeedCronJob", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// cabiResult normalizes a response's EventResult, mirroring native.NativeClient.
func cabiResult(r *sdkv1.EventResult, legacySent, legacyStop, legacyHandled bool) *sdkv1.EventResult {
	if r != nil {
		return r
	}
	return &sdkv1.EventResult{Handled: legacyHandled, Sent: legacySent, StopPropagation: legacyStop}
}
