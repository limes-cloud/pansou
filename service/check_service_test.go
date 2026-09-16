package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"pansou/model"
)

// rewriteTransport 把请求地址改写为测试服务器，避免真实外呼。
type rewriteTransport struct {
	base *url.URL
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *t.base
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newCheck115TestClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务器地址失败: %v", err)
	}
	return &http.Client{Transport: &rewriteTransport{base: base}}
}

func TestCheck115ExpiredShareState(t *testing.T) {
	// 回归用例：115 对过期链接仍返回非空 list/count，
	// 但 share_state=7 + forbid_reason="链接已过期"，必须判为失效。
	const payload = `{
		"state": true,
		"errno": 0,
		"data": {
			"share_state": 7,
			"shareinfo": {"share_state": 7, "forbid_reason": "链接已过期"},
			"count": 1,
			"list": [{"file_name": "test.txt"}]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	s := &CheckService{}
	item := model.CheckItem{
		DiskType: "115",
		URL:      "https://115.com/s/swff16b3ngy?password=CNYY",
		Password: "CNYY",
	}
	result, err := s.check115(item, item.URL, newCheck115TestClient(t, srv))
	if err != nil {
		t.Fatalf("check115 返回错误: %v", err)
	}
	if result.State != checkStateBad {
		t.Fatalf("过期链接应判 %q，实际 %q (summary=%q)", checkStateBad, result.State, result.Summary)
	}
	if !strings.Contains(result.Summary, "过期") {
		t.Fatalf("过期链接 summary 应包含“过期”，实际 %q", result.Summary)
	}
}

func TestCheck115ValidShareState(t *testing.T) {
	// share_state=1 且文件列表非空，应判有效。
	const payload = `{
		"state": true,
		"errno": 0,
		"data": {
			"share_state": 1,
			"shareinfo": {"share_state": 1},
			"count": 1,
			"list": [{"file_name": "test.txt"}]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	s := &CheckService{}
	item := model.CheckItem{
		DiskType: "115",
		URL:      "https://115.com/s/swff16b3ngy?password=CNYY",
		Password: "CNYY",
	}
	result, err := s.check115(item, item.URL, newCheck115TestClient(t, srv))
	if err != nil {
		t.Fatalf("check115 返回错误: %v", err)
	}
	if result.State != checkStateOK {
		t.Fatalf("正常链接应判 %q，实际 %q (summary=%q)", checkStateOK, result.State, result.Summary)
	}
}

func TestCheck115ValidFallbackList(t *testing.T) {
	// 接口未返回 share_state（缺省为 0）时，回退到文件列表启发式判断。
	const payload = `{
		"state": true,
		"errno": 0,
		"data": {
			"count": 1,
			"list": [{"file_name": "test.txt"}]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	s := &CheckService{}
	item := model.CheckItem{
		DiskType: "115",
		URL:      "https://115.com/s/swff16b3ngy?password=CNYY",
		Password: "CNYY",
	}
	result, err := s.check115(item, item.URL, newCheck115TestClient(t, srv))
	if err != nil {
		t.Fatalf("check115 返回错误: %v", err)
	}
	if result.State != checkStateOK {
		t.Fatalf("无状态字段+文件列表非空应判 %q，实际 %q (summary=%q)", checkStateOK, result.State, result.Summary)
	}
}

// 直接构造 CheckService，避免 NewCheckService 打开 bolt 缓存文件等副作用
func newTestCheckService(client *http.Client) *CheckService {
	return &CheckService{
		cache:    make(map[string]cachedCheckResult),
		inflight: make(map[string]*activeCheckCall),
		client:   client,
	}
}

func TestCheckWithProxyConcurrent(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		// 停留一段时间，让并发请求有机会重叠
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		active--
		mu.Unlock()

		_, _ = io.WriteString(w, `{"state":true,"errno":0,"data":{"share_state":1,"count":1,"list":[{"file_name":"t"}]}}`)
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务器地址失败: %v", err)
	}
	s := newTestCheckService(&http.Client{Transport: &rewriteTransport{base: base}})

	const n = 6
	items := make([]model.CheckItem, n)
	for i := range items {
		items[i] = model.CheckItem{
			DiskType: "115",
			URL:      fmt.Sprintf("https://115.com/s/aaaaaaaaa%d?password=1234", i),
			Password: "1234",
		}
	}

	resp, err := s.CheckWithProxy(items, "")
	if err != nil {
		t.Fatalf("CheckWithProxy 返回错误: %v", err)
	}
	if len(resp.Results) != n {
		t.Fatalf("结果数量 = %d, 期望 %d", len(resp.Results), n)
	}
	for i, r := range resp.Results {
		if r.State != checkStateOK {
			t.Fatalf("item %d state = %q, 期望 ok", i, r.State)
		}
		// 结果必须保持与输入一致的顺序
		if r.URL != items[i].URL {
			t.Fatalf("顺序错乱: index %d 对应 %q, 期望 %q", i, r.URL, items[i].URL)
		}
	}
	if maxActive < 2 {
		t.Fatalf("检测未并发执行: 最大并发在飞请求数 = %d, 期望 >= 2", maxActive)
	}
}
