// rpc_path_bench_test 建立 AstrBot-Go 插件 RPC 数据路径的基准基线。
//
// 它直接测量当前数据平面各 primitives 的成本——与优化后（原生 proto /
// bytes / FileReference）用同一组基准对比，作为"是否真正更快"的判据：
//
//   - BenchmarkEventJSON:      Host→Plugin 的事件下发，sdk.Event 全量
//     json.Marshal + json.Unmarshal（等效 event_json 载荷）。
//   - BenchmarkEventProto:     事件/发送请求 proto.Marshal/Unmarshal
//     （承载 event_json / chain_json bytes，量测 proto 帧在 JSON 之上的开销）。
//   - BenchmarkChainJSON:      SendMessage 结果链 []Component 的 JSON 往返。
//   - BenchmarkBase64Encode/Decode: 二进制路径（TextToImage/图/音/视频/文件）
//     的 base64 成本，精度到 1MB（1024 KB inline/blob 分界）与更大尺寸。
//   - BenchmarkComponentConvert: 组件扁平结构转换（ComponentsFromSDK / 反向）。
//
// 运行：go test ./internal/plugin/ -run=^$ -bench=RpcPath -benchmem
package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"google.golang.org/protobuf/proto"
)

// makeEvent 构造一个含若干文本/At/带 base64 图组件的 sdk.Event，目标大小约
// targetBytes（±15%）——模拟一条真实群消息的事件载荷。
func makeEvent(tb testing.TB, targetBytes int) *pluginsdk.Event {
	tb.Helper()
	ev := &pluginsdk.Event{
		Type:        "message",
		Platform:    "aiocqhttp",
		PlatformID:  "default",
		MessageType: "GroupMessage",
		SelfID:      "2408045264",
		SenderID:    "3442359407",
		SenderName:  "测试用户",
		ConvID:      "grupo:GroupMessage:743460071",
		GroupName:   "测试群",
		IsGroup:     true,
		IsAtBot:     true,
		MessageStr:  "帮我看下这个日志",
		PlainText:   "帮我看下这个日志",
		Timestamp:   1787896210,
		MessageID:   "123456789",
	}
	// 一个约 8KB 的 base64 图片块（真实日志/截图常见）。
	imgB64 := base64.StdEncoding.EncodeToString(makeBytes(6000))
	ev.Chain = append(ev.Chain,
		pluginsdk.Component{Type: pluginsdk.CompAt, TargetID: "2408045264", Name: "星匣"},
		pluginsdk.Component{Type: pluginsdk.CompImage, Base64: imgB64},
	)
	// 用文本组件凑足目标体积。
	cur := approxJSONSize(ev)
	for cur*2 < targetBytes {
		ev.Chain = append(ev.Chain, pluginsdk.Component{Type: pluginsdk.CompPlain, Text: strings.Repeat("日志分析内容", 32)})
		cur = approxJSONSize(ev)
	}
	return ev
}

// makeChain 构造目标体积的发送链（SendMessage chain_json 等价物）。
func makeChain(tb testing.TB, targetBytes int) []pluginsdk.Component {
	tb.Helper()
	chain := []pluginsdk.Component{
		pluginsdk.Component{Type: pluginsdk.CompPlain, Text: "回复内容"},
	}
	imgB64 := base64.StdEncoding.EncodeToString(makeBytes(6000))
	chain = append(chain, pluginsdk.Component{Type: pluginsdk.CompImage, Base64: imgB64})
	cur := approxChainSize(chain)
	for cur*2 < targetBytes {
		chain = append(chain, pluginsdk.Component{Type: pluginsdk.CompPlain, Text: strings.Repeat("正文", 64)})
		cur = approxChainSize(chain)
	}
	return chain
}

func makeBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func approxJSONSize(ev *pluginsdk.Event) int {
	b, e := json.Marshal(ev)
	if e != nil {
		return 1 << 20
	}
	return len(b)
}

func approxChainSize(c []pluginsdk.Component) int {
	b, e := json.Marshal(c)
	if e != nil {
		return 1 << 20
	}
	return len(b)
}

// ── Benchmark A：事件下发（event_json 全量 JSON 往返）────────────────────

func BenchmarkRpcPathEventJSON(b *testing.B) {
	for _, size := range []int{1 << 10, 10 << 10, 100 << 10} {
		ev := makeEvent(b, size)
		b.Run(fmt.Sprintf("size_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once []byte
			var out pluginsdk.Event
			for i := 0; i < b.N; i++ {
				data, err := json.Marshal(ev)
				if err != nil {
					b.Fatal(err)
				}
				if err := json.Unmarshal(data, &out); err != nil {
					b.Fatal(err)
				}
				once = data
			}
			_ = once
		})
	}
}

// ── Benchmark：事件/发送请求的 proto 帧往返（event_json/chain_json bytes）────

func BenchmarkRpcPathProtoEventBytes(b *testing.B) {
	for _, size := range []int{1 << 10, 10 << 10, 100 << 10} {
		ev := makeEvent(b, size)
		req := &sdkv1.HandleCommandRequest{Name: "cmd", Args: []string{"a"}, Event: pluginsdk.EventToSDKEvent(ev)}
		b.Run(fmt.Sprintf("size_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once []byte
			var out sdkv1.HandleCommandRequest
			for i := 0; i < b.N; i++ {
				data, err := proto.Marshal(req)
				if err != nil {
					b.Fatal(err)
				}
				if err := proto.Unmarshal(data, &out); err != nil {
					b.Fatal(err)
				}
				once = data
			}
			_ = once
		})
	}
}

// ── Benchmark C：SendMessage 结果链 JSON 往返（chain_json）──────────────

func BenchmarkRpcPathChainJSON(b *testing.B) {
	for _, size := range []int{1 << 10, 10 << 10, 100 << 10} {
		chain := makeChain(b, size)
		b.Run(fmt.Sprintf("size_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once []byte
			var out []pluginsdk.Component
			for i := 0; i < b.N; i++ {
				data, err := json.Marshal(chain)
				if err != nil {
					b.Fatal(err)
				}
				if err := json.Unmarshal(data, &out); err != nil {
					b.Fatal(err)
				}
				once = data
			}
			_ = once
		})
	}
}

// ── Benchmark B：二进制路径 base64（TextToImage/图/音/视频/文件现状）─────

// TextToImage 现状复合路径：Host 渲染 PNG → base64.Encode → proto string →
// gRPC → Python base64.Decode（用真实 sdkv1.TextToImageResponse 字段）。
func BenchmarkRpcPathTextToImageBase64(b *testing.B) {
	for _, size := range []int{256 << 10, 1024 << 10, 4 << 20} {
		data := makeBytes(size)
		b.Run(fmt.Sprintf("t2i_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once struct {
				dst []byte
				raw []byte
			}
			for i := 0; i < b.N; i++ {
				enc := base64.StdEncoding.EncodeToString(data)
				msg := &sdkv1.TextToImageResponse{ImageBase64: enc}
				wire, err := proto.Marshal(msg)
				if err != nil {
					b.Fatal(err)
				}
				var dec sdkv1.TextToImageResponse
				if err := proto.Unmarshal(wire, &dec); err != nil {
					b.Fatal(err)
				}
				dst, err := base64.StdEncoding.DecodeString(dec.ImageBase64)
				if err != nil {
					b.Fatal(err)
				}
				once.raw = wire
				once.dst = dst
			}
			_, _ = once.raw, once.dst
		})
	}
}

func BenchmarkRpcPathTextToImageBytes(b *testing.B) {
	for _, size := range []int{256 << 10, 1024 << 10, 4 << 20} {
		data := makeBytes(size)
		b.Run(fmt.Sprintf("t2i_bytes_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once struct {
				raw []byte
				dst []byte
			}
			for i := 0; i < b.N; i++ {
				msg := &sdkv1.TextToImageResponse{ImageBytes: data}
				wire, err := proto.Marshal(msg)
				if err != nil {
					b.Fatal(err)
				}
				var dec sdkv1.TextToImageResponse
				if err := proto.Unmarshal(wire, &dec); err != nil {
					b.Fatal(err)
				}
				once.raw = wire
				once.dst = dec.ImageBytes
			}
			_, _ = once.raw, once.dst
		})
	}
}

func BenchmarkRpcPathBase64Encode(b *testing.B) {
	for _, size := range []int{64 << 10, 256 << 10, 1024 << 10, 2 << 20, 4 << 20} {
		data := makeBytes(size)
		b.Run(fmt.Sprintf("enc_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once string
			for i := 0; i < b.N; i++ {
				once = base64.StdEncoding.EncodeToString(data)
			}
			_ = once
		})
	}
}

// BenchmarkRpcPathBlobFileRef 模拟 P0-2 大文件路径：CreateBlob（宿主落盘一次）
// + ReadBlob 分块读回。对比 BenchmarkRpcPathTextToImageBytes（inline bytes 全量
// proto 往返）与 Base64 路径，体现大对象经 handle 传输时宿主/插件各只持一份
// 完整缓冲、且跨进程只传小 handle/分块。
func BenchmarkRpcPathBlobFileRef(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20, 16 << 20} {
		data := makeBytes(size)
		b.Run(fmt.Sprintf("blob_%dMB", size>>20), func(b *testing.B) {
			b.ReportAllocs()
			bs, err := NewBlobStore(b.TempDir(), 10*time.Minute, 1<<20)
			if err != nil {
				b.Fatal(err)
			}
			defer bs.Stop()
			var once []byte
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ref, err := bs.Create(data, "application/octet-stream", "big.bin", 0)
				if err != nil {
					b.Fatal(err)
				}
				var off int64
				for {
					chunk, eof, _, err := bs.Read(ref.HandleId, off, 1<<20)
					if err != nil {
						b.Fatal(err)
					}
					once = append(once[:0], chunk...)
					off += int64(len(chunk))
					if eof {
						break
					}
				}
				_ = bs.Release(ref.HandleId)
			}
			_ = once
		})
	}
}

func BenchmarkRpcPathBase64Decode(b *testing.B) {
	for _, size := range []int{64 << 10, 256 << 10, 1024 << 10, 2 << 20, 4 << 20} {
		s := base64.StdEncoding.EncodeToString(makeBytes(size))
		b.Run(fmt.Sprintf("dec_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once []byte
			for i := 0; i < b.N; i++ {
				once, _ = base64.StdEncoding.DecodeString(s)
			}
			_ = once
		})
	}
}

// ── Benchmark D：组件扁平结构转换（ComponentsFromSDK / componentToSDK 等价）─

func BenchmarkRpcPathComponentConvert(b *testing.B) {
	chain := makeChain(b, 10<<10)
	sdkComps := make([]pluginsdk.Component, len(chain))
	// 正向：sdk.Component → message.Component（ComponentsFromSDK 的核心循环）。
	// 反向：message.Component → sdk.Component（组件扁平化，见 star/bridge）。
	b.Run("sdk_to_host", func(b *testing.B) {
		b.ReportAllocs()
		var once []message.Component
		for i := 0; i < b.N; i++ {
			once = ComponentsFromSDK(chain)
		}
		_ = once
	})
	b.Run("sdk_to_flat", func(b *testing.B) {
		b.ReportAllocs()
		var once []pluginsdk.Component
		for i := 0; i < b.N; i++ {
			once = sdkComps[:0:0]
			for _, c := range chain {
				once = append(once, pluginsdk.Component{
					Type: c.Type, Text: c.Text, TargetID: c.TargetID, Name: c.Name,
					URL: c.URL, Path: c.Path, File: c.File, Base64: c.Base64,
					FileID: c.FileID, ID: c.ID, Data: c.Data,
				})
			}
		}
		_ = once
	})
}

// BenchmarkRpcPathEventJSON1MB：1MB Event 的 legacy 全量 JSON 往返（Before）。
func BenchmarkRpcPathEventJSON1MB(b *testing.B) {
	ev := makeEvent(b, 1<<20)
	b.ReportAllocs()
	var once []byte
	var out pluginsdk.Event
	for i := 0; i < b.N; i++ {
		data, _ := json.Marshal(ev)
		_ = json.Unmarshal(data, &out)
		once = data
	}
	_ = once
}

// BenchmarkRpcPathSDKEventProto（P1 After）：native SDKEvent 全量 proto 往返，
// 不含 Event JSON（仅 metadata 一次 JSON）。
func BenchmarkRpcPathSDKEventProto(b *testing.B) {
	for _, size := range []int{1 << 10, 100 << 10, 1 << 20} {
		ev := makeEvent(b, size)
		b.Run(fmt.Sprintf("size_%dKB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var once []byte
			var out sdkv1.SDKEvent
			for i := 0; i < b.N; i++ {
				se := pluginsdk.EventToSDKEvent(ev)
				data, err := proto.Marshal(se)
				if err != nil {
					b.Fatal(err)
				}
				if err := proto.Unmarshal(data, &out); err != nil {
					b.Fatal(err)
				}
				_ = pluginsdk.SDKEventToEvent(&out)
				once = data
			}
			_ = once
		})
	}
}
