package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aumi314/lingshu/internal/store"
)

func TestDeleteGroupOrphansAPIKeys(t *testing.T) {
	e := newEnv(t)
	admin, bob := e.login("admin"), e.login("bob")
	var group store.Group
	if status, body := admin.do("POST", "/api/admin/groups", map[string]any{
		"name": "collection", "ratio": "1", "enabled": true, "is_public": true,
		"is_collection": true, "member_ids": []int64{1},
	}, &group); status != http.StatusOK {
		t.Fatalf("创建集合: %d %s", status, body)
	}
	groupPath := "/api/admin/groups/" + itoa(group.ID)
	previewPath := groupPath + "/delete-impact"
	var impact store.GroupDeleteImpact
	if status, body := admin.do("GET", previewPath, nil, &impact); status != http.StatusOK || impact.KeyCount != 0 || impact.Usernames == nil || len(impact.Usernames) != 0 {
		t.Fatalf("空集合的删除预览: %d %s", status, body)
	}
	var bobKey string
	for idx, owner := range []*client{bob, bob, admin} {
		var key struct {
			Key string `json:"key"`
		}
		if status, body := owner.do("POST", "/api/me/keys", map[string]any{"name": "key-" + itoa(int64(idx)), "group_id": group.ID}, &key); status != http.StatusCreated {
			t.Fatalf("创建 key: %d %s", status, body)
		}
		if owner == bob {
			bobKey = key.Key
		}
	}
	if status, body := bob.do("GET", previewPath, nil, nil); status != http.StatusForbidden {
		t.Fatalf("普通用户读取删除预览: %d %s", status, body)
	}
	if status, body := admin.do("GET", previewPath, nil, &impact); status != http.StatusOK || impact.KeyCount != 3 || !slices.Equal(impact.Usernames, []string{"admin", "bob"}) {
		t.Fatalf("有 key 的删除预览: %d %s", status, body)
	}
	if status, body := admin.do("DELETE", groupPath, nil, nil); status != http.StatusOK {
		t.Fatalf("删除集合: %d %s", status, body)
	}
	var missing int
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE group_id IS NULL`).Scan(&missing); err != nil || missing != 3 {
		t.Fatalf("删除后应保留三把分组留空的 key: count=%d err=%v", missing, err)
	}
	var keys []struct {
		ID      int64           `json:"id"`
		GroupID json.RawMessage `json:"group_id"`
	}
	if status, body := bob.do("GET", "/api/me/keys", nil, &keys); status != http.StatusOK || len(keys) != 2 || string(keys[0].GroupID) != "null" || string(keys[1].GroupID) != "null" {
		t.Fatalf("key 列表应返回 group_id: null: %d %s", status, body)
	}
	if status, body := bob.do("PATCH", "/api/me/keys/"+itoa(keys[0].ID), map[string]any{"group_id": 1}, nil); status != http.StatusOK {
		t.Fatalf("给 key 重选分组: %d %s", status, body)
	}
	if change := e.lastOp().change("group"); change == nil || string(change.To) != `"default"` {
		t.Fatalf("重选分组应记入操作日志: %+v", change)
	}
	if status, body := bob.do("GET", "/api/me/keys", nil, &keys); status != http.StatusOK || len(keys) != 2 || string(keys[0].GroupID) != "1" || string(keys[1].GroupID) != "null" {
		t.Fatalf("仅重选分组的 key 应更新 group_id: %d %s", status, body)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	request.Header.Set("Authorization", "Bearer "+bobKey)
	response := httptest.NewRecorder()
	e.api.Relay.ServeUsage(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"group":"default"`) {
		t.Fatalf("重选后原 key 应通过网关鉴权: %d %s", response.Code, response.Body)
	}
}
