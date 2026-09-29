package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 增量续读端点 `GET /desk/session/{id}/events?after=<seq>` 的边界。
//
// 这条路由与 `/messages` 共用 `/desk/session/` 这个**子树前缀** —— 所以这一组同时
// 钉住「分派没串线」：`/messages` 仍全量、`/events` 才增量、未知子路径**明说** 404
// （而不是默默回一份看起来正常的全量历史 —— 那会把「增量没生效」伪装成「增量没实现」）。
func TestHandleSessionEvents(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".magic-agent", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	id := "sess-ev"
	lines := []string{
		`{"seq":1,"kind":"user","text":"问题","session_id":"` + id + `"}`,
		`{"seq":2,"kind":"thinking","text":"思考","session_id":"` + id + `"}`,
		`{"seq":3,"kind":"text","text":"正文","session_id":"` + id + `"}`,
		`{"seq":4,"kind":"turn_end","text":"完成","session_id":"` + id + `"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	type eventsBody struct {
		Session          string           `json:"session"`
		Events           []map[string]any `json:"events"`
		Count            int              `json:"count"`
		After            uint64           `json:"after"`
		LastSeq          uint64           `json:"lastSeq"`
		SnapshotRequired bool             `json:"snapshotRequired"`
	}
	get := func(t *testing.T, url string) (int, eventsBody) {
		t.Helper()
		resp := doGET(t, url, true)
		var b eventsBody
		if resp.statusCode == http.StatusOK {
			if err := json.Unmarshal(resp.body, &b); err != nil {
				t.Fatalf("解析响应失败：%v（body=%s）", err, resp.body)
			}
		}
		return resp.statusCode, b
	}

	t.Run("省略 after = 全量", func(t *testing.T) {
		code, b := get(t, srv.URL+"/desk/session/"+id+"/events?token=tok")
		if code != http.StatusOK {
			t.Fatalf("期望 200，实得 %d", code)
		}
		if len(b.Events) != 4 || b.Count != 4 {
			t.Fatalf("期望 4 条，实得 %d（count=%d）", len(b.Events), b.Count)
		}
		if b.LastSeq != 4 {
			t.Fatalf("lastSeq 期望 4，实得 %d", b.LastSeq)
		}
		if b.SnapshotRequired {
			t.Fatal("全量读不该要求快照重建")
		}
		// 首条必须是 user —— 这条同时验证 core 落盘的 kind:"user" 能被读回来。
		if b.Events[0]["kind"] != "user" {
			t.Fatalf("首条 kind 期望 user，实得 %v", b.Events[0]["kind"])
		}
	})

	t.Run("after 排他", func(t *testing.T) {
		code, b := get(t, srv.URL+"/desk/session/"+id+"/events?after=2&token=tok")
		if code != http.StatusOK {
			t.Fatalf("期望 200，实得 %d", code)
		}
		if len(b.Events) != 2 {
			t.Fatalf("after=2 期望 2 条（3、4），实得 %d", len(b.Events))
		}
		// 排他：不能把 seq=2 自己再回一遍。
		if seq, _ := b.Events[0]["seq"].(float64); seq != 3 {
			t.Fatalf("首条期望 seq=3，实得 %v", b.Events[0]["seq"])
		}
		if b.After != 2 || b.LastSeq != 4 || b.SnapshotRequired {
			t.Fatalf("元信息不对：after=%d lastSeq=%d snapshot=%v", b.After, b.LastSeq, b.SnapshotRequired)
		}
	})

	t.Run("追平 = 空增量不要求快照", func(t *testing.T) {
		code, b := get(t, srv.URL+"/desk/session/"+id+"/events?after=4&token=tok")
		if code != http.StatusOK {
			t.Fatalf("期望 200，实得 %d", code)
		}
		if len(b.Events) != 0 {
			t.Fatalf("已追平期望空数组，实得 %d 条", len(b.Events))
		}
		if b.SnapshotRequired {
			t.Fatal("追平不是超前，不该要求快照重建")
		}
		// 空数组而非 null：客户端是「往画布上追加」，null 得处处判空。
		if !strings.Contains(string(doGET(t, srv.URL+"/desk/session/"+id+"/events?after=4&token=tok", true).body), `"events":[]`) {
			t.Fatal("空增量应序列化成 []，不是 null")
		}
	})

	t.Run("游标超前 = 要求快照重建", func(t *testing.T) {
		code, b := get(t, srv.URL+"/desk/session/"+id+"/events?after=99&token=tok")
		if code != http.StatusOK {
			t.Fatalf("期望 200，实得 %d", code)
		}
		if !b.SnapshotRequired {
			t.Fatal("after 超前于磁盘 maxSeq 时应要求快照重建")
		}
		if b.LastSeq != 4 {
			t.Fatalf("超前时 lastSeq 仍须是文件的 4，实得 %d", b.LastSeq)
		}
	})

	t.Run("不存在会话 = 空数组不是 null", func(t *testing.T) {
		code, b := get(t, srv.URL+"/desk/session/no-such/events?token=tok")
		if code != http.StatusOK {
			t.Fatalf("期望 200，实得 %d", code)
		}
		if b.Events == nil || len(b.Events) != 0 {
			t.Fatalf("期望空数组，实得 %#v", b.Events)
		}
		body := string(doGET(t, srv.URL+"/desk/session/no-such/events?token=tok", true).body)
		if !strings.Contains(body, `"events":[]`) {
			t.Fatalf("期望 \"events\":[]，实得 %s", body)
		}
	})

	t.Run("after 非法 = 400", func(t *testing.T) {
		// ⚠️ 不能静默当 0：那样客户端以为拿到了增量、实际每次收全量，
		// 性能问题会被伪装成「没问题」。
		for _, bad := range []string{"abc", "-1", "1.5", "99999999999999999999999"} {
			code, _ := get(t, srv.URL+"/desk/session/"+id+"/events?after="+bad+"&token=tok")
			if code != http.StatusBadRequest {
				t.Fatalf("after=%q 期望 400，实得 %d", bad, code)
			}
		}
	})

	t.Run("未知子路径 = 404", func(t *testing.T) {
		code, _ := get(t, srv.URL+"/desk/session/"+id+"/bogus?token=tok")
		if code != http.StatusNotFound {
			t.Fatalf("未知子路径期望 404，实得 %d", code)
		}
	})

	t.Run("缺令牌 = 401", func(t *testing.T) {
		resp := doGET(t, srv.URL+"/desk/session/"+id+"/events?after=1", false)
		if resp.statusCode != http.StatusUnauthorized {
			t.Fatalf("期望 401，实得 %d", resp.statusCode)
		}
	})

	t.Run("路径注入 = 400", func(t *testing.T) {
		resp := doGET(t, srv.URL+"/desk/session/..%2F..%2Fetc%2Fpasswd/events?token=tok", true)
		if resp.statusCode != http.StatusBadRequest {
			t.Fatalf("路径注入期望 400，实得 %d", resp.statusCode)
		}
	})

	// 分派没串线：/messages 必须仍是全量（否则增量改动会悄悄改掉既有行为）。
	t.Run("messages 仍全量", func(t *testing.T) {
		resp := doGET(t, srv.URL+"/desk/session/"+id+"/messages?token=tok", true)
		var b struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(resp.body, &b); err != nil {
			t.Fatal(err)
		}
		if len(b.Events) != 4 {
			t.Fatalf("/messages 期望仍返回全部 4 条，实得 %d", len(b.Events))
		}
	})
}
