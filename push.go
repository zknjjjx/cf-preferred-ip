package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- 输出文件 ----------

func writeCSV(dir string, rows []*result) error {
	var b strings.Builder
	b.WriteString("IP地址,端口,数据中心,平均延迟(ms),丢包率,下载速度(MB/s),验活状态\n")
	for _, r := range rows {
		st := ""
		if r.Verified {
			st = "通过"
		} else if r.VerifyStatus != 0 {
			st = fmt.Sprintf("HTTP %d", r.VerifyStatus)
		}
		fmt.Fprintf(&b, "%s,%d,%s,%.0f,%.2f,%.2f,%s\n", r.IP, r.Port, r.Colo, r.LatencyMS, r.Loss, r.SpeedMBs, st)
	}
	return os.WriteFile(filepath.Join(dir, "result.csv"), []byte(b.String()), 0644)
}

// ip.txt 格式：IP:端口#备注 —— edgetunnel 的优选 API 地址可以直接拉这个文件。
// 返回文件内容供 GitHub 推送复用。
func writeIPTxt(dir string, rows []*result) (string, error) {
	var b strings.Builder
	for _, r := range rows {
		colo := r.Colo
		if colo == "" {
			colo = "CF"
		}
		fmt.Fprintf(&b, "%s#%s %.2fMB/s %.0fms\n", r.addr(), colo, r.SpeedMBs, r.LatencyMS)
	}
	content := b.String()
	return content, os.WriteFile(filepath.Join(dir, "ip.txt"), []byte(content), 0644)
}

// ---------- cfnew 直推 ----------

// cfnew API（需先在 cfnew 面板开启“允许API管理”，变量 ae=yes）：
// DELETE /{path}/api/preferred-ips {"all":true} 清空，再 POST 批量添加。
func pushCFnew(ctx context.Context, c *config, rows []*result) error {
	var base string
	if c.cfnewPushURL != "" {
		// 推送网址优先：可填 https://域名/UUID 或 https://域名/自定义路径
		u := strings.TrimSuffix(strings.TrimSpace(c.cfnewPushURL), "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "https://" + u
		}
		if strings.HasSuffix(u, "/api/preferred-ips") {
			base = u
		} else {
			base = u + "/api/preferred-ips"
		}
	} else {
		path := c.cfnewPath
		if path == "" {
			path = c.cfnewUUID
		}
		if path == "" {
			return fmt.Errorf("未设置 cfnew UUID/自定义路径/推送网址")
		}
		path = strings.Trim(path, "/")
		base = "https://" + strings.TrimSuffix(c.cfnewDomain, "/") + "/" + path + "/api/preferred-ips"
	}
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}

	do := func(method string, body []byte) (int, error) {
		req, err := http.NewRequestWithContext(ctx, method, base, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return resp.StatusCode, nil
	}

	if code, err := do(http.MethodDelete, []byte(`{"all":true}`)); err != nil || code >= 300 {
		return fmt.Errorf("清空失败 HTTP %d: %v", code, err)
	}
	type item struct {
		IP   string `json:"ip"`
		Port int    `json:"port"`
		Name string `json:"name"`
	}
	items := make([]item, 0, len(rows))
	for i, r := range rows {
		colo := r.Colo
		if colo == "" {
			colo = "CF"
		}
		items = append(items, item{IP: r.IP, Port: r.Port, Name: fmt.Sprintf("%s-%02d", colo, i+1)})
	}
	payload, _ := json.Marshal(items)
	if code, err := do(http.MethodPost, payload); err != nil || code >= 300 {
		return fmt.Errorf("批量添加失败 HTTP %d: %v", code, err)
	}
	return nil
}

// ---------- GitHub 仓库推送（给 edgetunnel 拉取） ----------

func pushGitHub(ctx context.Context, c *config, content string) error {
	api := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", c.ghRepo, c.ghPath)
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 先查现有文件 sha（更新已有文件必须带 sha）
	var sha string
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?ref="+c.ghBranch, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.ghToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if resp, err := cl.Do(req); err == nil {
		if resp.StatusCode == 200 {
			var got struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&got)
			sha = got.SHA
		}
		resp.Body.Close()
	}

	body := map[string]any{
		"message": "cfscan update " + time.Now().Format("2006-01-02 15:04"),
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  c.ghBranch,
	}
	if sha != "" {
		body["sha"] = sha
	}
	payload, _ := json.Marshal(body)
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, api, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.ghToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
