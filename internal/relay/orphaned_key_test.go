package relay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

const orphanedGroupHint = "这把 key 的服务分组已被管理员删除，请到 API keys 页面重新选择一个分组"

type orphanedKeyResponse struct {
	status int
	body   string
}

// orphanedKeyGet 用当前 key 请求一个鉴权入口，返回状态码和原始响应体。
func (e *env) orphanedKeyGet(path string) orphanedKeyResponse {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.gateway.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return orphanedKeyResponse{status: resp.StatusCode, body: string(body)}
}

// orphanedKeyPost 用当前 key 请求一个模型调用入口。
func (e *env) orphanedKeyPost(path string) orphanedKeyResponse {
	e.t.Helper()
	resp := e.post(path, `{"model":"std-chat","messages":[]}`, nil)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return orphanedKeyResponse{status: resp.StatusCode, body: string(body)}
}

// assertOrphanedKey 检查孤儿 key 必须被 403 拒绝，提示要指向 API keys 页。
func (e *env) assertOrphanedKey(t *testing.T, name string, response orphanedKeyResponse) {
	t.Helper()
	if response.status != http.StatusForbidden || !strings.Contains(response.body, orphanedGroupHint) {
		t.Errorf("%s: 希望 403 且提示重选分组，实际 %d %s", name, response.status, response.body)
	}
}

func TestOrphanedKeyRejectedOnAllEntries(t *testing.T) {
	e := newEnv(t)
	var upstreamCalls int
	e.upstream = func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		t.Errorf("孤儿 key 不该打到上游: %s", r.URL.Path)
	}

	e.exec(`UPDATE api_keys SET group_id = NULL WHERE id = 1`)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/systemone"} {
		e.assertOrphanedKey(t, path, e.orphanedKeyPost(path))
	}
	for _, path := range []string{"/v1/models", "/v1/usage"} {
		e.assertOrphanedKey(t, path, e.orphanedKeyGet(path))
	}
	if upstreamCalls != 0 {
		t.Errorf("孤儿 key 打到上游 %d 次", upstreamCalls)
	}

	var balance, reserved, used string
	var logs int
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT balance::text, reserved::text,
		(SELECT quota_used::text FROM api_keys WHERE id = 1), (SELECT count(*) FROM request_logs)
		FROM users WHERE id = 1`).Scan(&balance, &reserved, &used, &logs); err != nil {
		t.Fatal(err)
	}
	if balance != "10.0000000000" || reserved != "0.0000000000" || used != "0.0000000000" || logs != 0 {
		t.Fatalf("孤儿 key 的请求不该扣费或写调用日志：balance=%s reserved=%s used=%s logs=%d", balance, reserved, used, logs)
	}
}

func TestOrphanedKeyRecoveredAfterReselect(t *testing.T) {
	e := newEnv(t)
	e.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}

	e.exec(`UPDATE api_keys SET group_id = NULL WHERE id = 1`)
	e.assertOrphanedKey(t, "invocation", e.orphanedKeyPost("/v1/chat/completions"))

	e.exec(`UPDATE api_keys SET group_id = 1 WHERE id = 1`)
	resp := e.post("/v1/chat/completions", `{"model":"std-chat","messages":[]}`, nil)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重选分组后应恢复调用，实际 %d %s", resp.StatusCode, body)
	}
	e.waitLog(resp.Header.Get("X-Lingshu-Request-Id"))

	usageResp := e.orphanedKeyGet("/v1/usage")
	if usageResp.status != http.StatusOK {
		t.Fatalf("重选分组后 usage 应恢复，实际 %d %s", usageResp.status, usageResp.body)
	}
}
