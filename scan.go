package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type result struct {
	IP           string
	Port         int
	LatencyMS    float64
	Loss         float64
	SpeedMBs     float64
	Colo         string
	Verified     bool
	VerifyStatus int
}

func (r *result) addr() string {
	if strings.Contains(r.IP, ":") {
		return "[" + r.IP + "]:" + strconv.Itoa(r.Port)
	}
	return r.IP + ":" + strconv.Itoa(r.Port)
}

// ---------- 候选 IP 抽样 ----------

// 官方 IP 段拉取失败时的内置兜底（Cloudflare 公布的 IPv4 段）
var fallbackV4 = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

func fetchCIDRs(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Fields(string(b)) {
		if strings.Contains(line, "/") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("空列表")
	}
	return out, nil
}

func addrToU32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func u32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// 把一个 IPv4 CIDR 按 /24 切分，每段抽 per24 个随机主机地址。
func sampleV4CIDR(p netip.Prefix, per24 int, rng *rand.Rand, seen map[string]bool, out *[]string) {
	base := addrToU32(p.Masked().Addr())
	bits := p.Bits()
	var subnets int
	var hostsPerSubnet uint32
	if bits >= 24 {
		subnets = 1
		hostsPerSubnet = 1 << (32 - bits)
	} else {
		subnets = 1 << (24 - bits)
		hostsPerSubnet = 256
	}
	for s := 0; s < subnets; s++ {
		subnetBase := base + uint32(s)*hostsPerSubnet
		if bits >= 24 {
			subnetBase = base
		}
		for k := 0; k < per24; k++ {
			var off uint32
			if hostsPerSubnet <= 2 {
				off = 0
			} else {
				off = uint32(1 + rng.Intn(int(hostsPerSubnet)-2)) // 避开网络/广播地址
			}
			ip := u32ToAddr(subnetBase + off).String()
			if !seen[ip] {
				seen[ip] = true
				*out = append(*out, ip)
			}
		}
		if bits >= 24 {
			break
		}
	}
}

func sampleV6CIDR(p netip.Prefix, n int, rng *rand.Rand, seen map[string]bool, out *[]string) {
	base := p.Masked().Addr()
	bb := base.As16()
	hostBits := 128 - p.Bits()
	for i := 0; i < n; i++ {
		b := bb
		// 在主机位里随机
		for bit := 0; bit < hostBits; bit++ {
			byteIdx := 15 - bit/8
			bitInByte := uint(bit % 8)
			if rng.Intn(2) == 1 {
				b[byteIdx] |= 1 << bitInByte
			} else {
				b[byteIdx] &^= 1 << bitInByte
			}
		}
		ip := netip.AddrFrom16(b).String()
		if !seen[ip] {
			seen[ip] = true
			*out = append(*out, ip)
		}
	}
}

func buildCandidates(ctx context.Context, c *config) ([]string, error) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	seen := map[string]bool{}
	var out []string

	cidrs, err := fetchCIDRs(ctx, "https://www.cloudflare.com/ips-v4")
	if err != nil {
		cidrs = fallbackV4
	}
	for _, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		sampleV4CIDR(p, c.per24, rng, seen, &out)
	}
	if c.ipv6 {
		v6, err := fetchCIDRs(ctx, "https://www.cloudflare.com/ips-v6")
		if err == nil {
			for _, s := range v6 {
				p, err := netip.ParsePrefix(s)
				if err != nil || p.Addr().Is4() {
					continue
				}
				sampleV6CIDR(p, c.v6Per, rng, seen, &out)
			}
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if c.maxCand > 0 && len(out) > c.maxCand {
		out = out[:c.maxCand]
	}
	return out, nil
}

// ---------- 阶段 1：TCPing 粗筛 ----------

func tcpingOnce(ip string, port int, timeout time.Duration) (time.Duration, bool) {
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0, false
	}
	el := time.Since(start)
	conn.Close()
	return el, true
}

func tcpingAll(ctx context.Context, c *config, ips []string) []*result {
	var mu sync.Mutex
	var results []*result
	var done int
	jobs := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < c.threads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				var total time.Duration
				var ok int
				for i := 0; i < c.tries; i++ {
					if el, success := tcpingOnce(ip, c.port, c.pingTimeout); success {
						total += el
						ok++
					}
				}
				mu.Lock()
				done++
				if done%1000 == 0 {
					fmt.Printf("\r[粗筛] %d/%d", done, len(ips))
				}
				if ok > 0 {
					avg := float64(total.Microseconds()) / float64(ok) / 1000.0
					loss := float64(c.tries-ok) / float64(c.tries)
					if avg <= c.maxLatencyMS && loss <= c.maxLoss {
						results = append(results, &result{IP: ip, Port: c.port, LatencyMS: avg, Loss: loss})
					}
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		for _, ip := range ips {
			select {
			case jobs <- ip:
			case <-ctx.Done():
				break
			}
		}
		close(jobs)
	}()
	wg.Wait()
	fmt.Printf("\r")
	sort.Slice(results, func(i, j int) bool {
		if results[i].Loss != results[j].Loss {
			return results[i].Loss < results[j].Loss
		}
		return results[i].LatencyMS < results[j].LatencyMS
	})
	if len(results) > c.keep1 {
		results = results[:c.keep1]
	}
	return results
}

// ---------- 阶段 2：下载测速 ----------

const dlHost = "speed.cloudflare.com"

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// 直连指定 IP 的 HTTP 客户端：不走任何代理，TLS 的 SNI/证书名用 sni 指定。
func directClient(ip string, port int, sni string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		},
		TLSClientConfig:       &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func parseColo(resp *http.Response) string {
	ray := resp.Header.Get("Cf-Ray")
	if ray == "" {
		return ""
	}
	parts := strings.Split(ray, "-")
	if len(parts) < 2 {
		return ""
	}
	return strings.ToUpper(parts[len(parts)-1])
}

// downloadAll 返回 (进入验活的列表, 全部达标列表)。
// 启用地区筛选时：先按 colo 分区，只让命中地区的 IP 进验活，
// 避免远地区（如美国）节点在全局速度截断时被近地区淘汰掉。
func downloadAll(ctx context.Context, c *config, in []*result) ([]*result, []*result) {
	var mu sync.Mutex
	var out []*result
	jobs := make(chan *result)
	var wg sync.WaitGroup
	for w := 0; w < c.dlThreads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				dctx, cancel := context.WithTimeout(ctx, c.dlTimeout)
				cl := directClient(r.IP, c.port, dlHost, c.dlTimeout)
				url := fmt.Sprintf("https://%s/__down?bytes=%d", dlHost, c.dlBytes)
				req, err := http.NewRequestWithContext(dctx, http.MethodGet, url, nil)
				if err != nil {
					cancel()
					continue
				}
				resp, err := cl.Do(req)
				if err != nil {
					cancel()
					continue
				}
				colo := parseColo(resp)
				start := time.Now()
				cw := &countWriter{}
				_, _ = io.Copy(cw, io.LimitReader(resp.Body, int64(c.dlBytes)))
				resp.Body.Close()
				el := time.Since(start).Seconds()
				cancel()
				if cw.n < 100*1024 || el <= 0 { // 至少下到 100KB 才算有效测速
					continue
				}
				r.SpeedMBs = float64(cw.n) / el / 1e6
				if colo != "" {
					r.Colo = colo
				}
				if r.SpeedMBs >= c.minSpeed {
					mu.Lock()
					out = append(out, r)
					mu.Unlock()
				}
			}
		}()
	}
	go func() {
		for _, r := range in {
			select {
			case jobs <- r:
			case <-ctx.Done():
			}
		}
		close(jobs)
	}()
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].SpeedMBs > out[j].SpeedMBs })
	all := out
	kept := out
	if c.coloSet != nil {
		kept = nil
		for _, r := range out {
			if r.Colo != "" && c.coloSet[r.Colo] {
				kept = append(kept, r)
			}
		}
	}
	if len(kept) > c.keep2 {
		kept = kept[:c.keep2]
	}
	return kept, all
}

// ---------- 阶段 3：真实域名验活 ----------

// 拿用户自己的域名逐个 IP 验活：TLS SNI=域名、Host=域名，实际发一个 GET。
// 状态码 2xx/3xx 且带 CF-Ray 头才算通过（Workers/Pages 域名必然有 CF-Ray）。
func verifyAll(ctx context.Context, c *config, in []*result) []*result {
	if c.verifyDomain == "" {
		// 未提供验活域名：跳过验活，按测速结果直接输出（不推荐）
		for _, r := range in {
			r.Verified = true
		}
		return in
	}
	var mu sync.Mutex
	var out []*result
	jobs := make(chan *result)
	var wg sync.WaitGroup
	for w := 0; w < c.verifyThreads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				vctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				cl := directClient(r.IP, c.port, c.verifyDomain, 6*time.Second)
				cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				}
				path := c.verifyPath
				if !strings.HasPrefix(path, "/") {
					path = "/" + path
				}
				req, err := http.NewRequestWithContext(vctx, http.MethodGet, "https://"+c.verifyDomain+path, nil)
				if err != nil {
					cancel()
					continue
				}
				req.Header.Set("User-Agent", "cfscan/1.0")
				resp, err := cl.Do(req)
				cancel()
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
				resp.Body.Close()
				r.VerifyStatus = resp.StatusCode
				if colo := parseColo(resp); colo != "" {
					r.Colo = colo
				}
				if resp.StatusCode >= 200 && resp.StatusCode < 400 && resp.Header.Get("Cf-Ray") != "" {
					r.Verified = true
					mu.Lock()
					out = append(out, r)
					mu.Unlock()
				}
			}
		}()
	}
	go func() {
		for _, r := range in {
			select {
			case jobs <- r:
			case <-ctx.Done():
			}
		}
		close(jobs)
	}()
	wg.Wait()
	// 验活通过者综合排序：速度优先，其次延迟
	sort.Slice(out, func(i, j int) bool {
		if out[i].SpeedMBs != out[j].SpeedMBs {
			return out[i].SpeedMBs > out[j].SpeedMBs
		}
		return out[i].LatencyMS < out[j].LatencyMS
	})
	return out
}
