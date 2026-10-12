#!/bin/sh
# luci-app-cfscan 手动安装：把 root/ 下文件复制到路由器根目录并刷新 LuCI 缓存。
# 前置：cfscan 容器已在 Docker 里运行（端口 7788），本插件只负责在 LuCI 里加菜单并内嵌页面。
set -e
cd "$(dirname "$0")"
cp -r root/* /
rm -rf /tmp/luci-modulecache /tmp/luci-indexcache
/etc/init.d/uhttpd restart || true
/etc/init.d/rpcd restart || true
echo "luci-app-cfscan 已安装：刷新路由器后台，在「服务」菜单里找「优选IP扫描」。"
