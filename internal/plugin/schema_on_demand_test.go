package plugin

import (
	"context"
	"net"
	"testing"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// mockSchemaService 提供 PluginService.GetConfigSchema：返回"运行期更新的
// schema"（如 update_manager 动态填充 options），模拟插件运行中更新
// config.schema 后宿主实时拉取的场景。
type mockSchemaService struct {
	sdkv1.UnimplementedPluginServiceServer
	schemaJSON []byte
}

func (s *mockSchemaService) GetConfigSchema(_ context.Context, _ *sdkv1.Empty) (*sdkv1.GetConfigSchemaResponse, error) {
	return &sdkv1.GetConfigSchemaResponse{SchemaJson: s.schemaJSON}, nil
}

func TestConfigSchemaOnDemand(t *testing.T) {
	// 运行期更新后的 schema（含动态 options）。
	updated := []byte(`{"white_plugin_list":{"type":"list","items":{"type":"string"},"options":["astrbot_plugin_box","astrbot_plugin_meme_manager"],"labels":["开盒插件（astrbot_plugin_box）","表情包管理器（meme_manager）"]},"black_plugin_list":{"type":"list","items":{"type":"string"},"options":["astrbot_plugin_box"],"labels":["开盒插件（astrbot_plugin_box）"]}}`)

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	sdkv1.RegisterPluginServiceServer(srv, &mockSchemaService{schemaJSON: updated})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pluginsdk.NewClient(conn)
	m := NewSubprocessManager(nil, t.TempDir())
	inst := &PluginInstance{
		ID:       "test_plugin_python",
		Name:     "test_plugin",
		Language: "python",
		Client:   client,
	}
	m.instances[inst.ID] = inst

	got := m.ConfigSchema(inst.ID)
	if len(got) == 0 {
		t.Fatal("ConfigSchema 返回空（未实时拉取运行期 schema）")
	}
	bl, ok := got["black_plugin_list"].(map[string]interface{})
	if !ok {
		t.Fatalf("black_plugin_list 缺失: %v", got)
	}
	opts, _ := bl["options"].([]interface{})
	if len(opts) != 1 || opts[0] != "astrbot_plugin_box" {
		t.Fatalf("black_plugin_list.options 应为动态更新的值, got %v", opts)
	}
	t.Logf("按需加载 OK: black_plugin_list.options=%v", opts)
}
