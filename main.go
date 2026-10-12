// cfscan —— 自研 Cloudflare 优选 IP 扫描测速工具
//
// 与 cfst / yx-tools 的关键区别：
//  1. 三段漏斗之后多一段「真实域名验活」：拿你自己的 Worker 域名（cfnew / edgetunnel）
//     逐个候选 IP 建 TLS(SNI=你的域名) 并实际发请求，能通才算数，
//     滤掉“测速能下载、但当节点用不了”的 IP。
//  2. 永不卡死：每个连接有硬超时，整轮有看门狗到点强制退出。
//  3. 失败不毁现有列表：验活通过数量不足时不推送、不覆盖输出文件。
//
// 用法：
//
//	cfscan [参数]            命令行单次扫描
//	cfscan serve -addr :8080 -data /data   网页模式（Docker 部署用）：设置页 + 定时 + 立即扫描
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
	_ "time/tzdata" // 内嵌时区库：scratch 镜像里没有 zoneinfo 也能用 Asia/Shanghai
)

type config struct {
	port         int
	per24        int
	maxCand      int
	threads      int
	tries        int
	pingTimeout  time.Duration
	maxLatencyMS float64
	maxLoss      float64
	keep1        int

	dlThreads int
	dlTimeout time.Duration
	dlBytes   int
	minSpeed  float64
	keep2     int

	verifyDomain  string
	verifyPath    string
	verifyThreads int

	coloSpec string
	coloSet  map[string]bool

	limit   int
	timeout time.Duration
	outDir  string
	ipv6    bool
	v6Per   int

	cfnewDomain  string
	cfnewUUID    string
	cfnewPath    string
	cfnewPushURL string
	ghRepo       string
	ghToken      string
	ghPath       string
	ghBranch     string
	minPush      int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runOnce 执行一轮完整扫描（含输出与推送）。返回错误而非直接退出，供 CLI 和 serve 模式共用。
func runOnce(ctx context.Context, c *config) error {
	c.coloSet = parseColoSpec(c.coloSpec)
	if err := os.MkdirAll(c.outDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 看门狗：ctx 之外再留 60 秒宽限，仍未结束则强杀，杜绝吊死。
	hardExit := time.AfterFunc(c.timeout+60*time.Second, func() {
		log.Printf("[看门狗] 超过 %s 仍未结束，强制退出", c.timeout)
		os.Exit(124)
	})
	defer hardExit.Stop()

	t0 := time.Now()
	log.Printf("cfscan 开始（端口 %d，验活域名 %q，地区 %q，整轮上限 %s）", c.port, c.verifyDomain, c.coloSpec, c.timeout)
	if c.coloSet != nil {
		log.Printf("[地区] 已启用地区筛选（%d 个数据中心）；提示：远地区延迟高，-tl 建议放宽到 500 以上", len(c.coloSet))
	}

	cands, err := buildCandidates(ctx, c)
	if err != nil {
		return fmt.Errorf("生成候选 IP 失败: %w", err)
	}
	log.Printf("[抽样] 候选 IP 共 %d 个", len(cands))

	stage1 := tcpingAll(ctx, c, cands)
	log.Printf("[粗筛] TCPing 通过 %d 个（延迟≤%.0fms 且丢包≤%.2f 取前 %d）", len(stage1), c.maxLatencyMS, c.maxLoss, c.keep1)
	if len(stage1) == 0 {
		return fmt.Errorf("粗筛后无可用 IP，本轮放弃（不推送、不覆盖）")
	}

	stage2, stage2All := downloadAll(ctx, c, stage1)
	if c.coloSet != nil {
		log.Printf("[测速] 下载达标共 %d 个，其中地区 %q 命中 %d 个（取前 %d 进验活）", len(stage2All), c.coloSpec, len(stage2), c.keep2)
	} else {
		log.Printf("[测速] 下载达标 %d 个（≥%.2f MB/s，取前 %d）", len(stage2), c.minSpeed, c.keep2)
	}
	if len(stage2) == 0 {
		return fmt.Errorf("下载测速后无可用 IP，本轮放弃（不推送、不覆盖）")
	}

	final := verifyAll(ctx, c, stage2)
	log.Printf("[验活] 真实域名验活通过 %d 个", len(final))
	if len(final) > c.limit {
		final = final[:c.limit]
	}

	if err := writeCSV(c.outDir, stage2All); err != nil {
		log.Printf("写 result.csv 失败: %v", err)
	}
	if len(final) == 0 {
		return fmt.Errorf("验活通过 0 个，本轮放弃（不推送、不覆盖 ip.txt）。提示：检查验活域名是否为你自己的 CF 域名")
	}
	ipTxt, err := writeIPTxt(c.outDir, final)
	if err != nil {
		return fmt.Errorf("写 ip.txt 失败: %w", err)
	}

	for i, r := range final {
		log.Printf("  %2d. %-40s colo=%s 延迟=%.0fms 丢包=%.2f 速度=%.2fMB/s", i+1, r.addr(), r.Colo, r.LatencyMS, r.Loss, r.SpeedMBs)
	}

	if len(final) < c.minPush {
		log.Printf("[推送] 通过数 %d < min-push %d，跳过所有推送以保护现有列表", len(final), c.minPush)
	} else {
		if c.cfnewPushURL != "" || (c.cfnewDomain != "" && (c.cfnewUUID != "" || c.cfnewPath != "")) {
			if err := pushCFnew(ctx, c, final); err != nil {
				log.Printf("[推送] cfnew 失败: %v", err)
			} else {
				log.Printf("[推送] cfnew 已更新 %d 个 IP", len(final))
			}
		}
		if c.ghRepo != "" && c.ghToken != "" {
			if err := pushGitHub(ctx, c, ipTxt); err != nil {
				log.Printf("[推送] GitHub 仓库失败: %v", err)
			} else {
				log.Printf("[推送] GitHub 仓库已更新 %s", c.ghPath)
			}
		}
	}
	log.Printf("cfscan 本轮完成，耗时 %s", time.Since(t0).Round(time.Second))
	return nil
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		runServer(os.Args[2:])
		return
	}

	var c config
	flag.IntVar(&c.port, "port", 443, "测速端口")
	flag.IntVar(&c.per24, "per24", 1, "每个 /24 段抽样 IP 数")
	flag.IntVar(&c.maxCand, "max-candidates", 0, "候选 IP 总数上限（0=不限，调试用）")
	flag.IntVar(&c.threads, "threads", 80, "TCPing 线程数（软路由别开太高）")
	flag.IntVar(&c.tries, "tries", 3, "单个 IP 的 TCPing 次数")
	flag.DurationVar(&c.pingTimeout, "ping-timeout", 1000*time.Millisecond, "单次 TCPing 超时")
	flag.Float64Var(&c.maxLatencyMS, "tl", 300, "平均延迟上限 ms")
	flag.Float64Var(&c.maxLoss, "max-loss", 0.25, "丢包率上限 0~1")
	flag.IntVar(&c.keep1, "keep1", 120, "粗筛后进入下载测速的数量")
	flag.IntVar(&c.dlThreads, "dl-threads", 8, "下载测速并发")
	flag.DurationVar(&c.dlTimeout, "dt", 6*time.Second, "单个 IP 下载测速硬截止")
	flag.IntVar(&c.dlBytes, "dl-bytes", 5000000, "下载测速目标字节数")
	flag.Float64Var(&c.minSpeed, "sl", 0.5, "下载速度下限 MB/s")
	flag.IntVar(&c.keep2, "keep2", 30, "下载测速后进入验活的数量")
	flag.StringVar(&c.verifyDomain, "verify-domain", envOr("CFSCAN_VERIFY_DOMAIN", ""), "验活域名（你自己的 cfnew/edgetunnel 域名，强烈建议填）")
	flag.StringVar(&c.verifyPath, "verify-path", "/", "验活请求路径")
	flag.IntVar(&c.verifyThreads, "verify-threads", 8, "验活并发")
	flag.StringVar(&c.coloSpec, "colo", "", "地区筛选：机场码或国家别名逗号分隔，如 US、美国、LAX,SJC、JP；留空=不限地区")
	flag.IntVar(&c.limit, "limit", 15, "最终输出/推送的 IP 数量")
	flag.DurationVar(&c.timeout, "timeout", 15*time.Minute, "整轮看门狗超时")
	flag.StringVar(&c.outDir, "outdir", ".", "输出目录")
	flag.BoolVar(&c.ipv6, "ipv6", false, "同时扫描 IPv6（实验）")
	flag.IntVar(&c.v6Per, "v6-per-cidr", 20, "每个 IPv6 段抽样数")
	flag.StringVar(&c.cfnewDomain, "cfnew-domain", envOr("CFSCAN_CFNEW_DOMAIN", ""), "cfnew 域名，填了才直推 cfnew")
	flag.StringVar(&c.cfnewUUID, "cfnew-uuid", envOr("CFSCAN_CFNEW_UUID", ""), "cfnew 的 UUID（或用 -cfnew-path）")
	flag.StringVar(&c.cfnewPath, "cfnew-path", envOr("CFSCAN_CFNEW_PATH", ""), "cfnew 自定义路径 d（设置后优先于 UUID）")
	flag.StringVar(&c.cfnewPushURL, "cfnew-push-url", envOr("CFSCAN_CFNEW_PUSH_URL", ""), "cfnew 推送网址（完整地址，填了优先于域名+UUID 拼装）")
	flag.StringVar(&c.ghRepo, "github-repo", envOr("CFSCAN_GITHUB_REPO", ""), "GitHub 中转仓库 owner/repo")
	flag.StringVar(&c.ghToken, "github-token", envOr("CFSCAN_GITHUB_TOKEN", ""), "GitHub Token（只需该仓库 contents 写权限）")
	flag.StringVar(&c.ghPath, "github-path", "cloudflare_ips.txt", "仓库内 IP 文件路径")
	flag.StringVar(&c.ghBranch, "github-branch", "main", "仓库分支")
	flag.IntVar(&c.minPush, "min-push", 3, "验活通过少于此数则不推送、不覆盖（保护现有列表）")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	if err := runOnce(ctx, &c); err != nil {
		log.Fatalf("%v", err)
	}
}
