package cdr

import (
	"testing"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// makeTestRequest 创建用于 CDR 测试的 SIP 请求。
func makeTestRequest(fromUser, toUser string) *message.Request {
	toURI := &message.URI{Scheme: "sip", User: toUser, Host: "example.com"}
	req := message.NewRequest(message.INVITE, toURI)
	req.Headers.Add("From", "<sip:"+fromUser+"@example.com>;tag=abc123")
	req.Headers.Add("To", "<sip:"+toUser+"@example.com>")
	return req
}

func TestCDRStartEndCall(t *testing.T) {
	mgr := NewManager(logger.NopLogger())

	req := makeTestRequest("alice", "bob")
	rec := mgr.StartCall("call-001", req, nil)
	if rec == nil {
		t.Fatal("StartCall returned nil")
	}
	if rec.CallID != "call-001" {
		t.Errorf("CallID: got %s, want call-001", rec.CallID)
	}

	// 验证记录存在
	got := mgr.GetRecord("call-001")
	if got == nil {
		t.Fatal("GetRecord: got nil")
	}
	if got.CallID != "call-001" {
		t.Errorf("CallID: got %s, want call-001", got.CallID)
	}

	// 结束呼叫
	mgr.EndCall("call-001", CauseNormalClearing, 200, "normal", "caller")

	// 验证记录已从活跃记录中移除（EndCall 会 Delete）
	got = mgr.GetRecord("call-001")
	if got != nil {
		t.Error("GetRecord after EndCall: should be nil")
	}
}

func TestCDRUpdateConnect(t *testing.T) {
	mgr := NewManager(logger.NopLogger())

	req := makeTestRequest("alice", "charlie")
	mgr.StartCall("call-002", req, nil)
	time.Sleep(10 * time.Millisecond)

	connectTime := time.Now()
	mgr.UpdateConnect("call-002", 200, connectTime)

	rec := mgr.GetRecord("call-002")
	if rec == nil {
		t.Fatal("GetRecord: got nil")
	}
	if rec.ConnectTime.IsZero() {
		t.Error("ConnectTime should not be zero after UpdateConnect")
	}
	if rec.SIPCode != 200 {
		t.Errorf("SIPCode: got %d, want 200", rec.SIPCode)
	}
}

func TestCDRStats(t *testing.T) {
	mgr := NewManager(logger.NopLogger())

	req1 := makeTestRequest("a", "b")
	req2 := makeTestRequest("c", "d")
	mgr.StartCall("call-a", req1, nil)
	mgr.StartCall("call-b", req2, nil)

	mgr.UpdateConnect("call-a", 200, time.Now())
	mgr.EndCall("call-a", CauseNormalClearing, 200, "normal", "caller")
	mgr.EndCall("call-b", CauseUserBusy, 486, "busy", "callee")

	stats := mgr.GetStats()
	if stats.TotalCalls.Load() != 2 {
		t.Errorf("TotalCalls: got %d, want 2", stats.TotalCalls.Load())
	}
	if stats.CompletedCalls.Load() != 1 {
		t.Errorf("CompletedCalls: got %d, want 1", stats.CompletedCalls.Load())
	}
}

func TestCDRSetCustomField(t *testing.T) {
	mgr := NewManager(logger.NopLogger())

	req := makeTestRequest("x", "y")
	mgr.StartCall("call-custom", req, nil)
	mgr.SetCustomField("call-custom", "carrier", "carrier-A")

	rec := mgr.GetRecord("call-custom")
	if rec == nil {
		t.Fatal("GetRecord: got nil")
	}
	if rec.CustomFields["carrier"] != "carrier-A" {
		t.Errorf("CustomFields[carrier]: got %s, want carrier-A", rec.CustomFields["carrier"])
	}
}

func TestCDRMemoryStore(t *testing.T) {
	store := NewMemoryStore(100)

	rec := &Record{
		CallID:    "test-001",
		FromUser:  "alice",
		ToUser:    "bob",
		StartTime: time.Now(),
	}

	err := store.Write(rec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	results, err := store.Query(&QueryFilter{CallID: "test-001"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Query: got %d results, want 1", len(results))
	}
	if results[0].CallID != "test-001" {
		t.Errorf("Query result CallID: got %s", results[0].CallID)
	}
}

func TestCDRMemoryStoreQueryByUser(t *testing.T) {
	store := NewMemoryStore(100)

	store.Write(&Record{CallID: "c1", FromUser: "alice", ToUser: "bob", StartTime: time.Now()})
	store.Write(&Record{CallID: "c2", FromUser: "charlie", ToUser: "dave", StartTime: time.Now()})
	store.Write(&Record{CallID: "c3", FromUser: "alice", ToUser: "eve", StartTime: time.Now()})

	// 查询 alice 发起的呼叫
	results, err := store.Query(&QueryFilter{FromUser: "alice"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("Query(alice): got %d results, want 2", len(results))
	}
}

func TestCDRMemoryStoreLRU(t *testing.T) {
	store := NewMemoryStore(3)

	store.Write(&Record{CallID: "c1", StartTime: time.Now()})
	store.Write(&Record{CallID: "c2", StartTime: time.Now()})
	store.Write(&Record{CallID: "c3", StartTime: time.Now()})
	store.Write(&Record{CallID: "c4", StartTime: time.Now()}) // 应淘汰 c1

	results, _ := store.Query(&QueryFilter{CallID: "c1"})
	if len(results) != 0 {
		t.Error("LRU: c1 should have been evicted")
	}

	results, _ = store.Query(&QueryFilter{CallID: "c4"})
	if len(results) != 1 {
		t.Error("LRU: c4 should exist")
	}
}

func TestCDRManagerWithStore(t *testing.T) {
	store := NewMemoryStore(100)
	mgr := NewManager(logger.NopLogger())
	mgr.SetStore(store)

	req := makeTestRequest("a", "b")
	mgr.StartCall("call-store", req, nil)
	mgr.EndCall("call-store", CauseNormalClearing, 200, "normal", "caller")

	// 验证数据写入了 store
	results, _ := store.Query(&QueryFilter{CallID: "call-store"})
	if len(results) != 1 {
		t.Errorf("Store should have the record after EndCall, got %d", len(results))
	}
}
