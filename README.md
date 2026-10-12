# cfscan —— Cloudflare 优选 IP 扫描（Docker + LuCI 插件）

在家里软路由上定时扫描 Cloudflare 优选 IP，**用你自己的域名逐个验活**后推送：cfnew 走 API 直推，edgetunnel 拉取本仓库的 IP 文件。

## 为什么自研

cfst / yx-tools 一类工具只测「TCP 能连 + 能下载测速文件」，推上去的 IP 经常当节点用不了，还可能卡死。cfscan 多做三件事：

1. **真实域名验活**：候选 IP 必须拿你自己部署的 cfnew / edgetunnel 域名建 TLS 并实际请求成功（2xx/3xx + CF-Ray）才入选。实测一轮 20 个测速达标 IP 里近一半验活不过。
2. **永不卡死**：每个连接硬超时 + 整轮看门狗（页面可设分钟数）。
3. **失败不毁现有列表**：验活通过太少时不推送、不覆盖。

支持按**国家/地区**筛选（美国/日本/香港…或自定义机场码），远地区不会被全球速度排名提前淘汰。

## 部署（iStoreOS / OpenWrt + Docker）

```sh
# 方式一：docker compose（推荐）
git clone https://github.com/zknjjjx/cf-preferred-ip.git
cd cf-preferred-ip
docker compose up -d        # 首次会用 Dockerfile 构建镜像

# 方式二：手动
docker build -t cfscan:latest .
docker run -d --name cfscan --restart unless-stopped -p 7788:8080 -v ./data:/data cfscan:latest
```

打开设置页：`http://路由器IP:7788`

在页面上填写（谁安装填谁自己的）：

- **测试域名**：你自己部署的 cfnew 域名、edgetunnel 域名（验活用）
- **推送**：cfnew 推送网址或 UUID（需先在 cfnew 面板开启「允许 API 管理」ae=yes）；GitHub 仓库和 Token（推 `cloudflare_ips.txt` / `cfnew_ips.txt`，edgetunnel 后台把优选 API 地址填仓库 raw 地址即可）
- **扫描参数**：国家/地区、速度下限、数量、延迟上限、端口、线程
- **定时**：每天执行时间（北京时间，容器时区 Asia/Shanghai）+ 单轮超时

密钥只保存在本地 `data/config.json`（0600），不会上传。

## LuCI 插件（可选）

装上后路由器后台「服务」菜单里多一项「优选IP扫描」，内嵌上面的设置页：

```sh
cd luci-app-cfscan && sh install.sh     # 卸载：sh uninstall.sh
```

## 工作流程

官方 IP 段按 /24 抽样 → TCPing 粗筛 → 下载测速 → 地区筛选 → 真实域名验活 → 输出/推送。
每天两轮：cfnew 一轮（验 cfnew 域名）、edgetunnel 一轮（验 edgetunnel 域名），各推各的。

## 输出

- 容器 `data/results/{cfnew,edgetunnel}/ip.txt`、`result.csv`
- GitHub 仓库 `cfnew_ips.txt`、`cloudflare_ips.txt`（edgetunnel 拉这个）

## 注意

- Cloudflare 是任播：落地地区主要由宽带运营商路由决定，选 IP 只能在能到达的地区里挑。
- 扫描必须从家里网络直连跑（不要走代理），否则结果不准。
