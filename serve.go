package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web.html
var webHTML string

type settings struct {
	CfnewDomain  string `json:"cfnewDomain"`
	CfnewUUID    string `json:"cfnewUUID"`
	CfnewPath    string `json:"cfnewPath"`
	CfnewPushURL string `json:"cfnewPushURL"`
	EnableCfnew  bool   `json:"enableCfnew"`

	EdgetunnelDomain string `json:"edgetunnelDomain"`

	EnableGithub bool   `json:"enableGithub"`
	GhRepo       string `json:"ghRepo"`
	GhToken      string `json:"ghToken"`

	Colo       string  `json:"colo"`
	MinSpeed   float64 `json:"minSpeed"`
	Limit      int     `json:"limit"`
	MaxLatency float64 `json:"maxLatency"`
	Port       int     `json:"port"`
	Threads    int     `json:"threads"`
	VerifyPath string  `json:"verifyPath"`
	ScheduleOn bool    `json:"scheduleOn"`
	ScheduleAt string  `json:"scheduleAt"` // "04:37"，北京时间（容器时区 Asia/Shanghai）
	TimeoutMin int     `json:"timeoutMin"` // 单轮最长运行时间（分钟）
}

func defaultSettings() settings {
	return settings{
		// 域名、仓库一律留空：谁安装谁在设置页填自己的（测试域名 = 你自己部署的 cfnew/edgetunnel 域名）
		CfnewDomain:      "",
		EnableCfnew:      true,
		EdgetunnelDomain: "",
		EnableGithub:     true,
		GhRepo:           "",
		Colo:             "",
		MinSpeed:         0.5,
		Limit:            15,
		MaxLatency:       300,
		Port:             443,
		Threads:          50,
		VerifyPath:       "/",
		ScheduleOn:       true,
		ScheduleAt:       "04:37",
		TimeoutMin:       20,
	}
}

// 环形日志缓冲，供状态页展示
type logBuf struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		b.lines = append(b.lines, line)
	}
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	return len(p), nil
}

func (b *logBuf) tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) <= n {
		return append([]string{}, b.lines...)
	}
	return append([]string{}, b.lines[len(b.lines)-n:]...)
}

type server struct {
	dataDir string
	mu      sync.Mutex
	running bool
	lastRun string
	lastErr string
	buf     *logBuf
}

func (s *server) settingsPath() string { return filepath.Join(s.dataDir, "config.json") }

func (s *server) loadSettings() settings {
	st := defaultSettings()
	b, err := os.ReadFile(s.settingsPath())
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (s *server) saveSettings(st settings) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.settingsPath(), b, 0600)
}

func (s *server) baseConfig(st settings) *config {
	keep2 := st.Limit * 2
	if keep2 < 30 {
		keep2 = 30
	}
	return &config{
		port: st.Port, per24: 1, threads: st.Threads, tries: 3,
		pingTimeout: time.Second, maxLatencyMS: st.MaxLatency, maxLoss: 0.25, keep1: 120,
		dlThreads: 8, dlTimeout: 6 * time.Second, dlBytes: 5000000, minSpeed: st.MinSpeed, keep2: keep2,
		verifyPath: st.VerifyPath, verifyThreads: 8,
		coloSpec: st.Colo, limit: st.Limit, timeout: time.Duration(maxInt(st.TimeoutMin, 1)) * time.Minute,
		ghBranch: "main", minPush: 3,
	}
}

// 两轮：cfnew（验自家域名+直推+推 cfnew_ips.txt）、edgetunnel（验自家域名+推 cloudflare_ips.txt）
func (s *server) runBoth(st settings) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastRun = time.Now().Format("2006-01-02 15:04:05")
		s.mu.Unlock()
	}()

	ctx := context.Background()

	c1 := s.baseConfig(st)
	c1.verifyDomain = st.CfnewDomain
	c1.outDir = filepath.Join(s.dataDir, "results", "cfnew")
	if st.EnableCfnew {
		c1.cfnewDomain = st.CfnewDomain
		c1.cfnewUUID = st.CfnewUUID
		c1.cfnewPath = st.CfnewPath
		c1.cfnewPushURL = st.CfnewPushURL
	}
	if c1.verifyDomain == "" && st.CfnewPushURL != "" {
		c1.verifyDomain = hostOf(st.CfnewPushURL)
	}
	if st.EnableGithub && st.GhToken != "" {
		c1.ghRepo = st.GhRepo
		c1.ghToken = st.GhToken
		c1.ghPath = "cfnew_ips.txt"
	}
	log.Printf("===== 第一轮：cfnew（验活 %s）=====", st.CfnewDomain)
	if err := runOnce(ctx, c1); err != nil {
		log.Printf("[第一轮] 失败: %v", err)
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
	}

	c2 := s.baseConfig(st)
	c2.verifyDomain = st.EdgetunnelDomain
	c2.outDir = filepath.Join(s.dataDir, "results", "edgetunnel")
	if st.EnableGithub && st.GhToken != "" {
		c2.ghRepo = st.GhRepo
		c2.ghToken = st.GhToken
		c2.ghPath = "cloudflare_ips.txt"
	}
	log.Printf("===== 第二轮：edgetunnel（验活 %s）=====", st.EdgetunnelDomain)
	if err := runOnce(ctx, c2); err != nil {
		log.Printf("[第二轮] 失败: %v", err)
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
	}
	log.Printf("===== 全部完成 =====")
}

func (s *server) scheduler() {
	var lastDate string
	for {
		time.Sleep(30 * time.Second)
		st := s.loadSettings()
		if !st.ScheduleOn {
			continue
		}
		now := time.Now()
		if now.Format("15:04") == st.ScheduleAt && now.Format("2006-01-02") != lastDate {
			lastDate = now.Format("2006-01-02")
			log.Printf("[定时] 到点 %s，开始每日扫描", st.ScheduleAt)
			go s.runBoth(st)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// hostOf 从完整 URL 里取域名部分（用于只填推送网址时推导验活域名）
func hostOf(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return u
}

func readIPList(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func runServer(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	data := fs.String("data", "/data", "数据目录（配置/结果/日志）")
	fs.Parse(args)

	if err := os.MkdirAll(filepath.Join(*data, "results"), 0755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	s := &server{dataDir: *data, buf: &logBuf{max: 300}}
	log.SetOutput(io.MultiWriter(os.Stderr, s.buf))
	if _, err := os.Stat(s.settingsPath()); os.IsNotExist(err) {
		_ = s.saveSettings(defaultSettings())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, webHTML)
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var st settings
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&st); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := s.saveSettings(st); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			log.Printf("[设置] 已保存")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		_ = json.NewEncoder(w).Encode(s.loadSettings())
	})
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running {
			http.Error(w, "正在扫描中", 409)
			return
		}
		log.Printf("[手动] 触发立即扫描")
		go s.runBoth(s.loadSettings())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		st := map[string]any{
			"running": s.running,
			"lastRun": s.lastRun,
			"lastErr": s.lastErr,
		}
		s.mu.Unlock()
		st["log"] = s.buf.tail(80)
		st["cfnew"] = readIPList(filepath.Join(s.dataDir, "results", "cfnew", "ip.txt"))
		st["edgetunnel"] = readIPList(filepath.Join(s.dataDir, "results", "edgetunnel", "ip.txt"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})

	go s.scheduler()
	log.Printf("cfscan 设置页已启动: %s（数据目录 %s）", *addr, *data)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
