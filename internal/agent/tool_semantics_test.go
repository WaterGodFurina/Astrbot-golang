package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAddToolActivePriority 语义对齐 Python ToolSet.add_tool：同名冲突时
// 优先保留 Active 的一方；active 状态相同时 new 覆盖。
func TestAddToolActivePriority(t *testing.T) {
	ts := NewToolSet()

	// existing active + new inactive → 保留 existing
	active := &FunctionTool{Name: "X", Active: true}
	ts.AddTool(active)
	ts.AddTool(&FunctionTool{Name: "X", Active: false})
	if ts.Get("X") != active {
		t.Fatal("existing active 必须保留（new inactive 不覆盖）")
	}

	// existing inactive + new active → 换成 new
	ts2 := NewToolSet()
	old := &FunctionTool{Name: "Y", Active: false}
	ts2.AddTool(old)
	fresh := &FunctionTool{Name: "Y", Active: true}
	ts2.AddTool(fresh)
	if ts2.Get("Y") != fresh {
		t.Fatal("new active 必须覆盖 existing inactive")
	}

	// active + active → new 覆盖
	ts3 := NewToolSet()
	ts3.AddTool(&FunctionTool{Name: "Z", Active: true})
	second := &FunctionTool{Name: "Z", Active: true}
	ts3.AddTool(second)
	if ts3.Get("Z") != second {
		t.Fatal("两侧 active 时 new 覆盖")
	}

	// inactive + inactive → new 覆盖
	ts4 := NewToolSet()
	ts4.AddTool(&FunctionTool{Name: "W", Active: false})
	secondW := &FunctionTool{Name: "W", Active: false}
	ts4.AddTool(secondW)
	if ts4.Get("W") != secondW {
		t.Fatal("两侧 inactive 时 new 覆盖")
	}
}

// TestBackgroundTaskTool 后台任务工具：Call 语义上由 pipeline 层处理，此处
// 验证 IsBackgroundTask 标记经 AddFuncFull 正确落表、Handler 后台执行完成。
func TestBackgroundTaskTool(t *testing.T) {
	mgr := NewFunctionToolManager()
	done := make(chan string, 1)
	mgr.AddFuncFull("bg_tool", "background demo", nil, func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		time.Sleep(30 * time.Millisecond)
		done <- "result"
		return "result", nil
	}, true)

	tool := mgr.GetFunc("bg_tool")
	if tool == nil {
		t.Fatal("bg_tool 必须已注册")
	}
	if !tool.IsBackgroundTask {
		t.Fatal("IsBackgroundTask 必须为 true")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := tool.Call(context.Background(), nil); err != nil {
			t.Errorf("Call: %v", err)
		}
	}()
	select {
	case v := <-done:
		if v != "result" {
			t.Fatalf("unexpected result %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background handler did not finish")
	}
	wg.Wait()
}
