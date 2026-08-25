package router

import (
	"context"
	"testing"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// makeRequest 创建用于路由测试的 SIP 请求。
func makeRequest(user string) *message.Request {
	uri := &message.URI{
		Scheme: "sip",
		User:   user,
		Host:   "example.com",
	}
	req := message.NewRequest(message.INVITE, uri)
	return req
}

func TestRouterExactMatch(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	err := r.AddEntry(&RouteEntry{
		ID:           "route-1",
		MatchPattern: "1234567890",
		MatchType:    MatchExact,
		Action:       RouteActionProxy,
		TargetURI:    "sip:1234567890@gateway1.example.com",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	req := makeRequest("1234567890")
	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if result == nil {
		t.Fatal("Route: expected result, got nil")
	}
	if result.TargetURI != "sip:1234567890@gateway1.example.com" {
		t.Errorf("TargetURI: got %s", result.TargetURI)
	}
}

func TestRouterPrefixMatch(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	err := r.AddEntry(&RouteEntry{
		ID:           "route-prefix",
		MatchPattern: "1234",
		MatchType:    MatchPrefix,
		Action:       RouteActionProxy,
		TargetURI:    "sip:gateway2.example.com",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	// 匹配前缀
	req := makeRequest("1234567")
	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if result == nil {
		t.Fatal("Route: expected result for prefix match")
	}

	// 不匹配
	req2 := makeRequest("5678")
	result2, _ := r.Route(context.Background(), req2)
	if result2 != nil {
		t.Error("Route: should not match non-prefix number")
	}
}

func TestRouterWildcardMatch(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	err := r.AddEntry(&RouteEntry{
		ID:           "route-wild",
		MatchPattern: "*",
		MatchType:    MatchWildcard,
		Action:       RouteActionProxy,
		TargetURI:    "sip:default.example.com",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	req := makeRequest("9999999")
	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if result == nil {
		t.Fatal("Route: wildcard should match any number")
	}
}

func TestRouterRemoveEntry(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	r.AddEntry(&RouteEntry{
		ID:           "route-remove",
		MatchPattern: "555",
		MatchType:    MatchExact,
		Action:       RouteActionProxy,
		TargetURI:    "sip:test.example.com",
		Enabled:      true,
	})

	err := r.RemoveEntry("route-remove")
	if err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}

	req := makeRequest("555")
	result, _ := r.Route(context.Background(), req)
	if result != nil {
		t.Error("Route: should not find removed entry")
	}
}

func TestRouterRejectAction(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	r.AddEntry(&RouteEntry{
		ID:           "route-reject",
		MatchPattern: "000",
		MatchType:    MatchExact,
		Action:       RouteActionReject,
		RejectCode:   403,
		RejectReason: "Forbidden",
		Enabled:      true,
	})

	req := makeRequest("000")
	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if result == nil {
		t.Fatal("Route: expected result for reject action")
	}
	if result.Action != RouteActionReject {
		t.Errorf("Action: got %s, want reject", result.Action)
	}
}

func TestRouterGetEntries(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	r.AddEntry(&RouteEntry{ID: "r1", MatchPattern: "111", MatchType: MatchExact, Action: RouteActionProxy, Enabled: true})
	r.AddEntry(&RouteEntry{ID: "r2", MatchPattern: "222", MatchType: MatchExact, Action: RouteActionProxy, Enabled: true})
	r.AddEntry(&RouteEntry{ID: "r3", MatchPattern: "333", MatchType: MatchExact, Action: RouteActionProxy, Enabled: true})

	entries := r.GetEntries()
	if len(entries) != 3 {
		t.Errorf("GetEntries: got %d entries, want 3", len(entries))
	}
}

func TestRouterUpdateEntry(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	r.AddEntry(&RouteEntry{
		ID:           "route-update",
		MatchPattern: "999",
		MatchType:    MatchExact,
		Action:       RouteActionProxy,
		TargetURI:    "sip:old.example.com",
		Enabled:      true,
	})

	err := r.UpdateEntry(&RouteEntry{
		ID:           "route-update",
		MatchPattern: "999",
		MatchType:    MatchExact,
		Action:       RouteActionProxy,
		TargetURI:    "sip:new.example.com",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	req := makeRequest("999")
	result, _ := r.Route(context.Background(), req)
	if result != nil && result.TargetURI != "sip:new.example.com" {
		t.Errorf("TargetURI after update: got %s, want sip:new.example.com", result.TargetURI)
	}
}

func TestRouterNoMatch(t *testing.T) {
	r := NewRouter(logger.NopLogger())

	r.AddEntry(&RouteEntry{
		ID:           "route-specific",
		MatchPattern: "1111",
		MatchType:    MatchExact,
		Action:       RouteActionProxy,
		Enabled:      true,
	})

	req := makeRequest("2222")
	result, _ := r.Route(context.Background(), req)
	if result != nil {
		t.Error("Route: should not match non-existent pattern")
	}
}
